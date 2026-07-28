package crypto

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The framed stream format.
//
// A multi-gigabyte file cannot be one AES-GCM message. GCM authenticates only
// at the end, so a whole-file message means either buffering the entire thing
// before a single plaintext byte can be trusted, or emitting unauthenticated
// plaintext and hoping. Neither is acceptable here.
//
// So the content is split into fixed 64 KiB plaintext frames, each its own
// AEAD message:
//
//	header:  version(1) || key_id(4)
//	frame i: ciphertext(<=65536) || tag(16)
//
// Properties this buys:
//
//   - Streaming. A frame is verified as soon as it has arrived, so plaintext
//     is released 64 KiB at a time and memory stays constant.
//   - Localised corruption. A damaged frame fails that frame rather than the
//     whole file, and the failure names the offset.
//   - Seekable. Frame i starts at a computable ciphertext offset, so a range
//     request decrypts only the frames it overlaps. This is what keeps
//     scrubbing an encrypted striped video working.
//
// Nonces are derived, never random: nonce = shard_index(4) || frame_index(8),
// under a key that is already unique per file. A 96-bit random nonce would
// have a birthday problem at this frame count; a counter under a per-file key
// has none, and it is what makes the offset arithmetic possible at all.
const (
	// StreamV1 is the version byte at the head of every framed stream.
	StreamV1 byte = 1

	// FrameSize is the plaintext bytes per frame.
	FrameSize = 64 * 1024

	// StreamHeaderLen is version || key_id.
	StreamHeaderLen = 1 + keyIDLength

	// frameOverhead is the GCM tag appended to each frame.
	frameOverhead = tagLen
)

// Errors from the framed stream.
var (
	ErrStreamVersion   = errors.New("crypto: unknown stream version")
	ErrStreamKey       = errors.New("crypto: stream was written with a different master key")
	ErrStreamCorrupt   = errors.New("crypto: stream frame failed authentication")
	ErrStreamTruncated = errors.New("crypto: stream ended before its final frame")
)

// StreamOverhead returns the stored size of a plaintext of the given length.
//
// The upload path needs this before the first byte is read: a resumable
// session must declare its length up front, and the declared length has to be
// the ciphertext length, not the plaintext one.
func StreamOverhead(plainSize int64) int64 {
	if plainSize < 0 {
		return StreamHeaderLen
	}
	frames := plainSize / FrameSize
	if plainSize%FrameSize != 0 {
		frames++
	}
	return StreamHeaderLen + plainSize + frames*frameOverhead
}

// FrameCount returns how many frames a plaintext of this length occupies.
func FrameCount(plainSize int64) int64 {
	if plainSize <= 0 {
		return 0
	}
	frames := plainSize / FrameSize
	if plainSize%FrameSize != 0 {
		frames++
	}
	return frames
}

// StreamKey derives the content key for one file.
func (k *Keyring) StreamKey(fileID []byte) ([]byte, error) {
	return k.Derive(InfoFile, fileID)
}

// encryptReader turns a plaintext stream into a framed ciphertext stream.
type encryptReader struct {
	aead       cipher.AEAD
	src        io.Reader
	shardIndex uint32
	keyID      [keyIDLength]byte

	frame   uint64
	plain   []byte
	pending []byte
	header  bool
	done    bool
	err     error
}

// NewEncryptReader wraps r so that reading from the result yields the framed
// ciphertext of r's contents.
//
// It is a Reader rather than a Writer because the provider pulls: Backend.Put
// takes an io.Reader and copies from it, so an encrypting Writer would need a
// pipe and a goroutine to bridge the two, and a pipe is one more place for a
// cancelled upload to leak a goroutine.
func (k *Keyring) NewEncryptReader(fileID []byte, shardIndex uint32, r io.Reader) (io.Reader, error) {
	key, err := k.StreamKey(fileID)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &encryptReader{
		aead:       aead,
		src:        r,
		shardIndex: shardIndex,
		keyID:      k.keyID,
		plain:      make([]byte, FrameSize),
	}, nil
}

func (e *encryptReader) Read(p []byte) (int, error) {
	if len(e.pending) > 0 {
		n := copy(p, e.pending)
		e.pending = e.pending[n:]
		return n, nil
	}
	if e.err != nil {
		return 0, e.err
	}

	if !e.header {
		e.header = true
		head := make([]byte, StreamHeaderLen)
		head[0] = StreamV1
		copy(head[1:], e.keyID[:])
		e.pending = head
		return e.Read(p)
	}

	if e.done {
		return 0, io.EOF
	}

	// ReadFull rather than Read: a frame must be exactly FrameSize unless it
	// is the last one, or the frame boundaries stop being computable and
	// range reads break.
	n, rerr := io.ReadFull(e.src, e.plain)
	switch {
	case rerr == nil:
		// A full frame. There may or may not be more.
	case errors.Is(rerr, io.EOF), errors.Is(rerr, io.ErrUnexpectedEOF):
		e.done = true
		if n == 0 {
			return 0, io.EOF
		}
	default:
		e.err = fmt.Errorf("read plaintext frame %d: %w", e.frame, rerr)
		return 0, e.err
	}

	nonce := frameNonce(e.shardIndex, e.frame)
	aad := frameAAD(e.keyID, e.shardIndex, e.frame)

	// Sealed into a fresh slice each frame: the previous one may still be
	// referenced by a partially consumed pending buffer.
	e.pending = e.aead.Seal(make([]byte, 0, n+frameOverhead), nonce, e.plain[:n], aad)
	e.frame++

	return e.Read(p)
}

// decryptReader turns a framed ciphertext stream back into plaintext.
type decryptReader struct {
	aead       cipher.AEAD
	src        io.Reader
	shardIndex uint32
	keyID      [keyIDLength]byte

	// frame is the index of the next frame to read. It starts non-zero for
	// a range read that begins partway into a shard.
	frame uint64

	// skip is how many plaintext bytes to discard from the first frame,
	// because a range rarely starts on a frame boundary.
	skip int

	// limit is how many plaintext bytes to emit in total. Negative means
	// "until the stream ends".
	limit int64

	cipherBuf []byte
	pending   []byte
	header    bool
	eof       bool
	err       error
}

// NewDecryptReader wraps a framed ciphertext stream.
func (k *Keyring) NewDecryptReader(fileID []byte, shardIndex uint32, r io.Reader) (io.Reader, error) {
	return k.newDecryptReader(fileID, shardIndex, r, 0, 0, -1)
}

// NewDecryptRangeReader decrypts a slice of a shard.
//
// firstFrame is the index of the frame the caller's ciphertext starts at, skip
// is how many plaintext bytes of that frame to discard, and length is how many
// plaintext bytes to emit. Together with CipherRange these are what make a
// Range request over encrypted content fetch only the frames it needs.
func (k *Keyring) NewDecryptRangeReader(
	fileID []byte, shardIndex uint32, r io.Reader,
	firstFrame uint64, skip int, length int64,
) (io.Reader, error) {
	return k.newDecryptReader(fileID, shardIndex, r, firstFrame, skip, length)
}

func (k *Keyring) newDecryptReader(
	fileID []byte, shardIndex uint32, r io.Reader,
	firstFrame uint64, skip int, limit int64,
) (io.Reader, error) {
	key, err := k.StreamKey(fileID)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &decryptReader{
		aead:       aead,
		src:        r,
		shardIndex: shardIndex,
		keyID:      k.keyID,
		frame:      firstFrame,
		skip:       skip,
		limit:      limit,
		cipherBuf:  make([]byte, FrameSize+frameOverhead),
		// A range read starts partway in, past the header.
		header: firstFrame != 0 || skip != 0 || limit >= 0,
	}, nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for {
		if len(d.pending) > 0 {
			if d.limit == 0 {
				return 0, io.EOF
			}
			n := copy(p, d.pending)
			if d.limit > 0 && int64(n) > d.limit {
				n = int(d.limit)
			}
			d.pending = d.pending[n:]
			if d.limit > 0 {
				d.limit -= int64(n)
			}
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		if d.eof || d.limit == 0 {
			return 0, io.EOF
		}
		if err := d.nextFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				d.eof = true
				return 0, io.EOF
			}
			d.err = err
			return 0, err
		}
	}
}

func (d *decryptReader) nextFrame() error {
	if !d.header {
		head := make([]byte, StreamHeaderLen)
		if _, err := io.ReadFull(d.src, head); err != nil {
			if errors.Is(err, io.EOF) {
				// An empty stream is a zero-byte plaintext that was
				// never written; treat it as empty rather than as
				// corruption.
				return io.EOF
			}
			return fmt.Errorf("read stream header: %w", err)
		}
		if head[0] != StreamV1 {
			return fmt.Errorf("%w: %d", ErrStreamVersion, head[0])
		}
		if string(head[1:]) != string(d.keyID[:]) {
			return ErrStreamKey
		}
		d.header = true
	}

	n, rerr := io.ReadFull(d.src, d.cipherBuf)
	switch {
	case rerr == nil:
	case errors.Is(rerr, io.EOF):
		return io.EOF
	case errors.Is(rerr, io.ErrUnexpectedEOF):
		// A short final frame is normal. A frame shorter than the tag is
		// not a frame at all.
		if n < frameOverhead {
			return ErrStreamTruncated
		}
	default:
		return fmt.Errorf("read frame %d: %w", d.frame, rerr)
	}

	nonce := frameNonce(d.shardIndex, d.frame)
	aad := frameAAD(d.keyID, d.shardIndex, d.frame)

	plain, err := d.aead.Open(nil, nonce, d.cipherBuf[:n], aad)
	if err != nil {
		// The frame index is in the nonce and the AAD, so a reordered or
		// substituted frame fails here rather than decrypting into
		// plausible-looking wrong plaintext.
		return fmt.Errorf("%w at frame %d", ErrStreamCorrupt, d.frame)
	}
	d.frame++

	if d.skip > 0 {
		if d.skip >= len(plain) {
			d.skip -= len(plain)
			return nil
		}
		plain = plain[d.skip:]
		d.skip = 0
	}
	d.pending = plain
	return nil
}

// CipherRange maps a plaintext range within a shard onto the ciphertext bytes
// that have to be fetched to serve it.
//
// It returns the byte offset and length to request from the provider, the
// index of the first frame in that slice, and how many plaintext bytes of that
// first frame to discard. A request for one byte in the middle of a 30 GB
// shard fetches 64 KiB plus a tag, not 30 GB.
func CipherRange(plainStart, plainLength, shardPlainSize int64) (
	cipherStart, cipherLength int64, firstFrame uint64, skip int,
) {
	if plainStart < 0 {
		plainStart = 0
	}
	if plainLength <= 0 || plainStart >= shardPlainSize {
		return 0, 0, 0, 0
	}
	if plainStart+plainLength > shardPlainSize {
		plainLength = shardPlainSize - plainStart
	}

	first := plainStart / FrameSize
	last := (plainStart + plainLength - 1) / FrameSize

	// Every frame is FrameSize+tag on the wire except the final one, which
	// is short. Slicing to the end of the shard rather than computing the
	// tail length exactly keeps this arithmetic simple and never over-reads,
	// because the caller bounds the request by the object size.
	const stride = FrameSize + frameOverhead
	cipherStart = StreamHeaderLen + first*stride
	cipherLength = (last - first + 1) * stride

	total := StreamOverhead(shardPlainSize)
	if cipherStart+cipherLength > total {
		cipherLength = total - cipherStart
	}

	return cipherStart, cipherLength, uint64(first), int(plainStart - first*FrameSize)
}

// frameNonce derives the 96-bit nonce for one frame.
//
// The key is already unique per file, so uniqueness within a file is all that
// is needed: shard index and frame index together identify a frame exactly
// once. Nothing here is random, and nothing needs to be.
func frameNonce(shardIndex uint32, frame uint64) []byte {
	nonce := make([]byte, nonceLen)
	binary.BigEndian.PutUint32(nonce[0:4], shardIndex)
	binary.BigEndian.PutUint64(nonce[4:12], frame)
	return nonce
}

// frameAAD binds each frame to its position and to the key that wrote it, so a
// frame cannot be moved between shards or reordered within one and still
// authenticate.
func frameAAD(keyID [keyIDLength]byte, shardIndex uint32, frame uint64) []byte {
	aad := make([]byte, 1+keyIDLength+4+8)
	aad[0] = StreamV1
	copy(aad[1:], keyID[:])
	binary.BigEndian.PutUint32(aad[1+keyIDLength:], shardIndex)
	binary.BigEndian.PutUint64(aad[1+keyIDLength+4:], frame)
	return aad
}
