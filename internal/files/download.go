package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// Content is an open read over a file's bytes.
type Content struct {
	// Body produces exactly Length bytes. The caller closes it on every
	// path, including error paths.
	Body io.ReadCloser
	// Length is how many bytes Body will produce.
	Length int64
	// TotalSize is the file's full size, which a 206 response needs for its
	// Content-Range header.
	TotalSize int64
	// Start is the offset of the first byte, for the same reason.
	Start int64
	// File is the metadata, for naming and content-type decisions.
	File File
}

// Open returns a reader over a file, or over a byte range of one.
//
// rng == nil means the whole file. When a range is given, only the shards that
// intersect it are opened: a request for the last megabyte of a striped
// 30 GB file touches one provider object, not three. That is what makes
// scrubbing a striped video work rather than merely not crash.
func (s *Service) Open(ctx context.Context, userID, fileID uuid.UUID, rng *storage.ByteRange) (*Content, error) {
	file, err := s.Get(ctx, userID, fileID)
	if err != nil {
		return nil, err
	}

	// Rules.md: a file with an incomplete manifest is unreadable and says
	// so. It never returns partial data as if it were whole.
	if file.Status != StatusReady {
		return nil, skerr.Public(skerr.ErrNotFound,
			"That file did not finish uploading.")
	}
	if err := verifyManifest(file); err != nil {
		return nil, err
	}

	start, length := int64(0), file.SizeBytes
	if rng != nil {
		start, length, err = clampRange(*rng, file.SizeBytes)
		if err != nil {
			return nil, err
		}
	}

	if length == 0 {
		return &Content{
			Body:      io.NopCloser(emptyReader{}),
			Length:    0,
			TotalSize: file.SizeBytes,
			Start:     start,
			File:      file,
		}, nil
	}

	body, err := s.openRange(ctx, userID, file, start, length)
	if err != nil {
		return nil, err
	}

	return &Content{
		Body:      body,
		Length:    length,
		TotalSize: file.SizeBytes,
		Start:     start,
		File:      file,
	}, nil
}

// openRange builds a reader over [start, start+length) spanning shards.
//
// The shards are opened lazily, one at a time, as the reader is consumed.
// Opening them all up front would hold a provider connection per shard for the
// whole download and would fetch bytes nobody has asked for yet.
func (s *Service) openRange(ctx context.Context, userID uuid.UUID, file File, start, length int64) (io.ReadCloser, error) {
	segments := planRead(file.Shards, start, length)
	if len(segments) == 0 {
		return nil, skerr.Public(skerr.ErrIntegrity,
			"This file's shard map does not cover the bytes requested.")
	}

	return &shardReader{
		ctx:       ctx,
		svc:       s,
		userID:    userID,
		fileID:    file.ID,
		encrypted: file.IsEncrypted,
		segments:  segments,
		current:   -1,
	}, nil
}

// readSegment is one shard's contribution to a range read.
type readSegment struct {
	shard storage.ObjectRef
	// account is the connected account holding the shard.
	account *uuid.UUID
	// offset is where in the shard to start.
	offset int64
	// length is how many bytes to take from it.
	length int64
	// plainSize is the shard's whole plaintext length, which the ciphertext
	// offset arithmetic needs to locate the short final frame.
	plainSize int64
	index     int32
}

// planRead selects the shards intersecting [start, start+length) and the slice
// of each that is needed. It is pure arithmetic over the manifest, which is
// what makes it worth testing on its own.
func planRead(shards []Shard, start, length int64) []readSegment {
	if length <= 0 {
		return nil
	}
	end := start + length // exclusive

	var out []readSegment
	for _, sh := range shards {
		shardStart := sh.PlainOffset
		shardEnd := sh.PlainOffset + sh.PlainSize

		// No overlap.
		if shardEnd <= start || shardStart >= end {
			continue
		}

		from := max(start, shardStart)
		to := min(end, shardEnd)

		out = append(out, readSegment{
			shard:     storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes},
			account:   sh.AccountID,
			offset:    from - shardStart,
			length:    to - from,
			plainSize: sh.PlainSize,
			index:     sh.Index,
		})
	}
	return out
}

// shardReader concatenates shard slices in order, opening each one only when
// the previous is exhausted.
type shardReader struct {
	ctx       context.Context
	svc       *Service
	userID    uuid.UUID
	fileID    uuid.UUID
	encrypted bool
	segments  []readSegment
	current   int
	// body is what Read consumes: the provider stream for plaintext, or the
	// decrypting reader wrapped around it when the file is encrypted.
	body io.Reader
	// closer is always the provider stream. The decrypting reader has no
	// resources of its own, so this is what has to be closed.
	closer io.Closer
	closed bool
}

func (r *shardReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	for {
		if r.body == nil {
			if err := r.openNext(); err != nil {
				return 0, err
			}
			if r.body == nil {
				return 0, io.EOF
			}
		}

		n, err := r.body.Read(p)
		if n > 0 {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			// This shard is done; move to the next one. The loop
			// continues rather than returning 0, nil, which callers
			// are allowed to treat as a stall.
			r.closeCurrent()
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (r *shardReader) openNext() error {
	r.current++
	if r.current >= len(r.segments) {
		return nil // exhausted; Read turns this into io.EOF
	}
	seg := r.segments[r.current]

	backend, err := r.svc.backends.For(r.ctx, r.userID, seg.account)
	if err != nil {
		return skerr.Public(skerr.ErrUnavailable,
			"The drive holding shard %d of this file is not connected.", seg.index)
	}

	// The provider request is expressed in stored bytes. For plaintext that
	// is the same range the caller asked for; for ciphertext it is the
	// frames that overlap it, which is what keeps a one-byte range from
	// fetching a whole 256 MiB shard.
	fetch := storage.ByteRange{Start: seg.offset, Length: seg.length}
	var firstFrame uint64
	var skip int

	if r.encrypted {
		cStart, cLen, frame, sk := skcrypto.CipherRange(seg.offset, seg.length, seg.plainSize)
		if cLen == 0 {
			return skerr.Public(skerr.ErrIntegrity,
				"Shard %d of this file does not cover the bytes requested.", seg.index)
		}
		fetch = storage.ByteRange{Start: cStart, Length: cLen}
		firstFrame, skip = frame, sk
	}

	body, n, err := backend.Get(r.ctx, seg.shard, &fetch)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			// The manifest points at something that is not there. This
			// is a corrupt file, and saying so is the only honest
			// answer: never hand back the shards that do exist as if
			// they were the whole thing.
			return skerr.Public(skerr.ErrIntegrity,
				"Shard %d of this file is missing from its drive.", seg.index)
		}
		return fmt.Errorf("open shard %d: %w", seg.index, err)
	}

	// The provider disagreeing about length means the object is not what
	// the manifest says it is.
	if n >= 0 && n != fetch.Length {
		if cerr := body.Close(); cerr != nil {
			r.svc.log.WarnContext(r.ctx, "closing mismatched shard",
				slog.String("error", cerr.Error()))
		}
		return skerr.Public(skerr.ErrIntegrity,
			"Shard %d of this file is the wrong size.", seg.index)
	}

	r.closer = body
	r.body = body

	if r.encrypted {
		dec, derr := r.svc.keyring.NewDecryptRangeReader(
			r.fileID[:], uint32(seg.index), body, firstFrame, skip, seg.length)
		if derr != nil {
			r.closeCurrent()
			return fmt.Errorf("open decrypt reader for shard %d: %w", seg.index, derr)
		}
		r.body = dec
	}

	return nil
}

// closeCurrent releases the provider stream for the shard in hand. The
// decrypting reader wraps it and holds nothing of its own, so this is the only
// thing that needs closing.
func (r *shardReader) closeCurrent() {
	if r.closer != nil {
		if cerr := r.closer.Close(); cerr != nil {
			r.svc.log.WarnContext(r.ctx, "closing shard reader",
				slog.String("file_id", r.fileID.String()),
				slog.String("error", cerr.Error()))
		}
	}
	r.closer = nil
	r.body = nil
}

// Close releases the shard currently open. It is safe to call more than once.
func (r *shardReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.closer == nil {
		r.body = nil
		return nil
	}
	closer := r.closer
	r.closer = nil
	r.body = nil
	if err := closer.Close(); err != nil {
		return fmt.Errorf("close shard reader: %w", err)
	}
	return nil
}

// verifyManifest checks that the shard list actually covers the file.
//
// Rules.md and Architecture.md §6: a file with a missing or inconsistent
// manifest is unreadable and reports so explicitly. Refusing here, before a
// single byte is served, is what keeps a truncated download from looking like
// a complete one.
func verifyManifest(f File) error {
	if len(f.Shards) == 0 {
		return skerr.Public(skerr.ErrIntegrity, "This file has no shards recorded.")
	}

	var covered int64
	for i, sh := range f.Shards {
		if int32(i) != sh.Index {
			return skerr.Public(skerr.ErrIntegrity,
				"This file's shards are out of order at index %d.", i)
		}
		if sh.PlainOffset != covered {
			return skerr.Public(skerr.ErrIntegrity,
				"This file has a gap in its shard map at shard %d.", sh.Index)
		}
		if sh.PlainSize < 0 {
			return skerr.Public(skerr.ErrIntegrity,
				"Shard %d of this file has an invalid size.", sh.Index)
		}
		covered += sh.PlainSize
	}

	if covered != f.SizeBytes {
		return skerr.Public(skerr.ErrIntegrity,
			"This file's shards cover %s of %s.",
			humanBytes(covered), humanBytes(f.SizeBytes))
	}
	return nil
}

// clampRange turns a requested range into a concrete offset and length.
//
// Rules.md §2.5 and §2.7: a range from the wire is a claim. An unsatisfiable
// one is rejected with a distinct error so the handler can answer 416 with a
// Content-Range, rather than silently returning the whole file — which is what
// makes a media player seek to the wrong place.
func clampRange(rng storage.ByteRange, size int64) (start, length int64, err error) {
	if size == 0 {
		// RFC 9110: no range is satisfiable on an empty representation.
		return 0, 0, skerr.Public(skerr.ErrValidation, "This file is empty.")
	}
	if rng.Start < 0 || rng.Start >= size {
		return 0, 0, ErrRangeNotSatisfiable
	}
	length = rng.Length
	if length <= 0 || rng.Start+length > size {
		length = size - rng.Start
	}
	return rng.Start, length, nil
}

// ErrRangeNotSatisfiable reports a Range header that cannot be served. The
// handler maps it to 416 rather than to the generic validation status.
var ErrRangeNotSatisfiable = errors.New("files: requested range not satisfiable")

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
