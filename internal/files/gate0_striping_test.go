package files_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi/handlers"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
	"github.com/mridul249/Skein/internal/router"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/local"
)

// The network-free, race-detectable form of the Phase 7 Gate 0 manual check:
// striping across two drives, a whole-file checksum round trip, and ranges that
// cross a frame boundary and a shard boundary. The manual two-Drive run
// confirms the provider; this confirms the arithmetic, and it can run under
// -race and -count in CI.
const (
	// 1 MiB is 16 AEAD frames exactly, so it is a valid shard size under the
	// boot check in config.go.
	gate0ShardSize = 1 << 20

	// Four full 1 MiB shards plus a partial fifth.
	//
	// The extra 1234 bytes are deliberate and are a correction to the work
	// order, which specified 4.5 MiB exactly. 4.5 MiB is itself an exact
	// multiple of the 64 KiB frame size (72 frames), and the tail shard would
	// be exactly 8 frames — so every frame in the file would be full and the
	// "final partial frame / EOF handling" case below would not exercise a
	// short final frame at all. 1234 is not a multiple of anything relevant,
	// which is the point.
	gate0FileSize = 4<<20 + 512<<10 + 1234 // 4,719,826

	// Where shard 0 ends and shard 1 begins, in plaintext bytes.
	gate0ShardBoundary = gate0ShardSize // 1,048,576
)

// newRoundRobinStriped builds a fixture under PolicyRoundRobin.
//
// This duplicates the wiring in newStriped rather than adding a policy
// parameter to it, because newStriped has seven existing call sites in a green
// test file and Rules.md §0 says not to refactor adjacent code while adding
// something. The duplication is deliberate.
func newRoundRobinStriped(t *testing.T, drives int, capacityEach, shardSize int64) *stripedFixture {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	routerStore := router.NewMemoryStore()
	backends := map[uuid.UUID]*local.Backend{}
	ids := make([]uuid.UUID, 0, drives)

	for i := 0; i < drives; i++ {
		id := uuid.New()
		ids = append(ids, id)
		routerStore.AddAccount(id, int32(i+1), fmt.Sprintf("drive%d@example.com", i), capacityEach, 0)

		b, berr := local.New(t.TempDir(), local.WithFakeCapacity(capacityEach))
		if berr != nil {
			t.Fatalf("local.New() = %v", berr)
		}
		backends[id] = b
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reserver := router.NewReserver(routerStore, logger)
	planner := router.NewPlanner(reserver, router.PolicyRoundRobin, shardSize, skcrypto.StreamOverhead)

	store := files.NewConformanceStore(t)
	svc := files.NewService(
		store,
		files.NewStripingPlanner(planner, reserver),
		multiResolver{backends: backends},
		ring,
		files.Config{Encrypt: true, MaxUploadBytes: 1 << 40},
		logger,
	)

	return &stripedFixture{
		svc:      svc,
		store:    store,
		router:   routerStore,
		backends: backends,
		accounts: ids,
		userID:   uuid.New(),
	}
}

// contentHandler serves GET /api/files/{id}/content as the real handler does,
// so the status code is the one a client would see. The service layer has no
// notion of 206.
func contentHandler(t *testing.T, f *stripedFixture) http.Handler {
	t.Helper()

	// No capability signer: this fixture injects its principal directly, so
	// it exercises the handler rather than either credential path.
	h := handlers.NewFiles(f.svc, middleware.NewConcurrencyLimiter(4), 1<<40, "", nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := middleware.WithPrincipal(req.Context(), middleware.Principal{
				UserID:    f.userID,
				SessionID: uuid.New(),
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/files/{id}/content", h.Content)
	r.Head("/api/files/{id}/content", h.Content)
	return r
}

// framePosition reports where an absolute plaintext offset lands: which shard,
// which frame within that shard's own stream, and the offset inside that frame.
//
// A wrong-bytes failure with a correct length is ambiguous on its own. These
// numbers disambiguate it: a 16-byte skew in the in-frame offset is a tag
// accounted on the wrong side, a frame index off by one is the header term
// missing from the ciphertext mapping, and a shard index off by one is a
// manifest offset error.
func framePosition(shards []files.Shard, abs int64) (shardIdx int32, frame, inFrame int64, ok bool) {
	for _, sh := range shards {
		if abs >= sh.PlainOffset && abs < sh.PlainOffset+sh.PlainSize {
			rel := abs - sh.PlainOffset
			return sh.Index, rel / skcrypto.FrameSize, rel % skcrypto.FrameSize, true
		}
	}
	return 0, 0, 0, false
}

// diagnose logs the four numbers for every shard the range touches. Called only
// on failure.
func diagnose(t *testing.T, shards []files.Shard, start, length int64) {
	t.Helper()

	end := start + length // exclusive
	for _, sh := range shards {
		from := max(start, sh.PlainOffset)
		to := min(end, sh.PlainOffset+sh.PlainSize)
		if to <= from {
			continue
		}
		idx, frame, inFrame, ok := framePosition(shards, from)
		if !ok {
			t.Errorf("diagnostic: offset %d is covered by no shard", from)
			continue
		}
		t.Errorf("diagnostic: shard idx=%d frame=%d in-frame offset=%d length=%d "+
			"(absolute plaintext [%d,%d), shard plain offset=%d size=%d, ciphertext size=%d)",
			idx, frame, inFrame, to-from, from, to, sh.PlainOffset, sh.PlainSize, sh.SizeBytes)
	}
}

func TestGate0StripingAndRangesOnTheLocalBackend(t *testing.T) {
	f := newRoundRobinStriped(t, 2, 8<<20, gate0ShardSize)

	data := make([]byte, gate0FileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	wantSum := sha256.Sum256(data)

	file := f.upload(t, "gate0.bin", data)
	shards := file.Shards

	// 1. Striping.
	t.Run("shards alternate across both drives with contiguous indices", func(t *testing.T) {
		const wantShards = 5 // four full, one partial
		if len(shards) != wantShards {
			t.Fatalf("shards = %d, want %d", len(shards), wantShards)
		}

		perDrive := map[uuid.UUID]int{}
		var covered int64

		for i, sh := range shards {
			// Contiguous from 0, no gaps, no duplicates.
			if sh.Index != int32(i) {
				t.Errorf("shard at position %d has idx %d, want %d", i, sh.Index, i)
			}
			if sh.AccountID == nil {
				t.Fatalf("shard %d has no drive", sh.Index)
			}
			perDrive[*sh.AccountID]++

			// The manifest must tile the plaintext exactly.
			if sh.PlainOffset != covered {
				t.Errorf("shard %d starts at plaintext %d, want %d (gap or overlap)",
					sh.Index, sh.PlainOffset, covered)
			}
			covered += sh.PlainSize

			// Every non-tail shard is a full shard, and the stored object is
			// the ciphertext size for its plaintext.
			wantPlain := int64(gate0ShardSize)
			if i == wantShards-1 {
				wantPlain = gate0FileSize - int64(wantShards-1)*gate0ShardSize
			}
			if sh.PlainSize != wantPlain {
				t.Errorf("shard %d holds %d plaintext bytes, want %d", sh.Index, sh.PlainSize, wantPlain)
			}
			if want := skcrypto.StreamOverhead(sh.PlainSize); sh.SizeBytes != want {
				t.Errorf("shard %d stored size = %d, want %d (5-byte header + plaintext + 16 per frame)",
					sh.Index, sh.SizeBytes, want)
			}

			// Alternation: consecutive shards never share a drive.
			if i > 0 && *shards[i-1].AccountID == *sh.AccountID {
				t.Errorf("shards %d and %d are both on drive %s; round-robin must alternate",
					shards[i-1].Index, sh.Index, sh.AccountID)
			}
		}

		if covered != gate0FileSize {
			t.Errorf("shards cover %d plaintext bytes, want %d", covered, int64(gate0FileSize))
		}
		if len(perDrive) != 2 {
			t.Errorf("the file used %d drives, want 2", len(perDrive))
		}
		// Five shards alternating over two drives is 3 and 2.
		for id, n := range perDrive {
			if n != 3 && n != 2 {
				t.Errorf("drive %s holds %d shards, want 3 or 2 under alternation", id, n)
			}
		}
	})

	// 2. Whole-file round trip.
	t.Run("whole file round trips byte-identically", func(t *testing.T) {
		got := f.read(t, file.ID, nil)
		if len(got) != gate0FileSize {
			t.Fatalf("read %d bytes, want %d", len(got), gate0FileSize)
		}
		if sha256.Sum256(got) != wantSum {
			t.Fatal("SHA-256 changed across a striped, encrypted round trip")
		}
	})

	// 3. Range requests, through the real handler so the status is real.
	t.Run("range requests", func(t *testing.T) {
		srv := contentHandler(t, f)

		tests := []struct {
			name  string
			start int64
			end   int64 // inclusive, as in the Range header
			note  string
		}{
			{"within one frame", 100, 999, "crosses nothing"},
			{"one frame boundary", 65000, 66999, "frame 0 -> 1"},
			{"three frames", 130000, 260000, "frames 1 -> 3"},
			{"shard boundary", 1048000, 1048999, "shard 0 frame 15 -> shard 1 frame 0"},
			{"final partial frame", gate0FileSize - 3000, gate0FileSize - 1, "EOF handling"},
			{"single byte", 4096, 4096, "exactly 1 byte"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				length := tc.end - tc.start + 1

				req := httptest.NewRequest(http.MethodGet,
					"/api/files/"+file.ID.String()+"/content", nil)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", tc.start, tc.end))
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != http.StatusPartialContent {
					t.Fatalf("status = %d, want 206 (%s)", rec.Code, tc.note)
				}

				got := rec.Body.Bytes()
				if int64(len(got)) != length {
					t.Fatalf("got %d bytes, want %d (%s)", len(got), length, tc.note)
				}

				want := data[tc.start : tc.start+length]
				if !bytes.Equal(got, want) {
					// Length right, bytes wrong: the interesting failure.
					first := int64(-1)
					for i := range got {
						if got[i] != want[i] {
							first = int64(i)
							break
						}
					}
					t.Errorf("range [%d,%d] returned the wrong bytes; first difference at "+
						"offset %d within the range (absolute plaintext %d)",
						tc.start, tc.end, first, tc.start+first)
					diagnose(t, shards, tc.start, length)
				}
			})
		}
	})

	// The shard-boundary case split, asserted explicitly. The work order names
	// these numbers, so a change in shard layout that still happens to return
	// 1000 correct bytes does not quietly invalidate the case.
	t.Run("the shard boundary case splits 576 + 424", func(t *testing.T) {
		const start, length = 1048000, 1000

		idx, frame, inFrame, ok := framePosition(shards, start)
		if !ok {
			t.Fatalf("plaintext offset %d is covered by no shard", start)
		}
		if idx != 0 || frame != 15 {
			t.Errorf("offset %d resolves to shard %d frame %d, want shard 0 frame 15",
				start, idx, frame)
		}
		if want := int64(1048000 - 15*skcrypto.FrameSize); inFrame != want {
			t.Errorf("in-frame offset = %d, want %d", inFrame, want)
		}

		firstHalf := gate0ShardBoundary - int64(start)
		if firstHalf != 576 {
			t.Errorf("shard 0 contributes %d bytes, want 576", firstHalf)
		}
		if secondHalf := int64(length) - firstHalf; secondHalf != 424 {
			t.Errorf("shard 1 contributes %d bytes, want 424", secondHalf)
		}

		idx, frame, inFrame, ok = framePosition(shards, gate0ShardBoundary)
		if !ok {
			t.Fatalf("plaintext offset %d is covered by no shard", int64(gate0ShardBoundary))
		}
		if idx != 1 || frame != 0 || inFrame != 0 {
			t.Errorf("offset %d resolves to shard %d frame %d in-frame %d, want shard 1 frame 0 offset 0",
				int64(gate0ShardBoundary), idx, frame, inFrame)
		}
	})
}

// A shard size that is not a whole number of AEAD frames must be refused at
// boot. The check lives in config.validate; this asserts the striping path
// never sees such a size rather than re-testing config, and that the tail
// shard is the only short one.
func TestGate0EveryNonTailShardIsAWholeNumberOfFrames(t *testing.T) {
	f := newRoundRobinStriped(t, 2, 8<<20, gate0ShardSize)

	data := make([]byte, gate0FileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "frames.bin", data)

	for i, sh := range file.Shards {
		last := i == len(file.Shards)-1
		rem := sh.PlainSize % skcrypto.FrameSize
		if !last && rem != 0 {
			t.Errorf("shard %d carries a runt frame: %d plaintext bytes leaves %d over a frame",
				sh.Index, sh.PlainSize, rem)
		}
		if last && rem == 0 {
			t.Logf("note: the tail shard happens to be frame-aligned (%d bytes), "+
				"so no short final frame is exercised", sh.PlainSize)
		}
	}
}

// A range whose start is past the end of the file is refused, not clamped into
// a 206 with the wrong bytes.
func TestGate0RangePastEndOfFileIsRefused(t *testing.T) {
	f := newRoundRobinStriped(t, 2, 8<<20, gate0ShardSize)

	data := make([]byte, gate0FileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "range.bin", data)

	srv := contentHandler(t, f)
	req := httptest.NewRequest(http.MethodGet, "/api/files/"+file.ID.String()+"/content", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", int64(gate0FileSize)+10, int64(gate0FileSize)+20))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", rec.Code)
	}
}

// Reading a shard directly from its drive must not reveal the plaintext, and
// the object must be exactly the ciphertext size the manifest claims. This is
// the striping-specific form of the at-rest check: with two drives, either one
// alone holds an incomplete set of shards.
func TestGate0NeitherDriveHoldsTheWholeFile(t *testing.T) {
	f := newRoundRobinStriped(t, 2, 8<<20, gate0ShardSize)

	marker := []byte("SKEINPLAINTEXTMARKER0123456789AB")
	data := bytes.Repeat(marker, gate0FileSize/len(marker))
	file := f.upload(t, "marked.bin", data)

	perDrive := map[uuid.UUID]int64{}
	for _, sh := range file.Shards {
		backend := f.backends[*sh.AccountID]
		rc, _, err := backend.Get(context.Background(),
			storage.ObjectRef{ProviderID: sh.ProviderID, Size: sh.SizeBytes}, nil)
		if err != nil {
			t.Fatalf("read shard %d at rest: %v", sh.Index, err)
		}
		stored, rerr := io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("Close() = %v", cerr)
		}
		if rerr != nil {
			t.Fatalf("read shard %d: %v", sh.Index, rerr)
		}

		if bytes.Contains(stored, marker) {
			t.Errorf("shard %d holds plaintext at rest", sh.Index)
		}
		if int64(len(stored)) != sh.SizeBytes {
			t.Errorf("shard %d object is %d bytes, manifest says %d",
				sh.Index, len(stored), sh.SizeBytes)
		}
		perDrive[*sh.AccountID] += sh.PlainSize
	}

	for id, n := range perDrive {
		if n >= int64(len(data)) {
			t.Errorf("drive %s holds %d of %d plaintext bytes: the file is not striped",
				id, n, len(data))
		}
	}
}

// The routing policy is configuration, so the string the config package
// validates and the constant the planner switches on must not drift apart. A
// mismatch would silently fall through to most-available.
func TestGate0RoundRobinPolicyStringMatchesTheConstant(t *testing.T) {
	if router.Policy("round-robin") != router.PolicyRoundRobin {
		t.Errorf("router.PolicyRoundRobin = %q, want %q as accepted by SKEIN_ROUTING_POLICY",
			router.PolicyRoundRobin, "round-robin")
	}
}

// A media element does not fetch a file; it probes it and then seeks around
// inside it. This is that sequence, against a file that is encrypted and
// striped across two drives.
//
// The pieces were each covered already — ranged reads cross shard boundaries
// correctly, and the capability URL authenticates HEAD as well as GET — but
// nothing asserted the shape a <video> actually produces: a HEAD for metadata,
// then ranges that land wherever the user drags the scrubber. Frontend preview
// (known issue #16) makes that sequence reachable for the first time, so it is
// worth pinning.
func TestGate0MediaElementRequestSequenceCrossesAShardBoundary(t *testing.T) {
	f := newRoundRobinStriped(t, 2, 8<<20, gate0ShardSize)

	data := make([]byte, gate0FileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	file := f.upload(t, "clip.bin", data)
	srv := contentHandler(t, f)

	// 1. The probe. A media element issues this before deciding whether it can
	//    seek at all: without Accept-Ranges it downloads the whole file, which
	//    for a striped 4 MiB clip means pulling every shard off every drive.
	head := httptest.NewRequest(http.MethodHead,
		"/api/files/"+file.ID.String()+"/content", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, head)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want %q — a player will not seek without it", got, "bytes")
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(gate0FileSize, 10) {
		t.Errorf("Content-Length = %q, want %d", got, gate0FileSize)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body, want 0", rec.Body.Len())
	}

	// 2. The seeks. Each is a plaintext range the player asks for; the reader
	//    has to map it through the shard layout and the AEAD frame stride and
	//    come back with the right bytes. The first deliberately spans the
	//    boundary between shard 0 on drive 1 and shard 1 on drive 2.
	seeks := []struct {
		name       string
		start, end int64
	}{
		{"across the shard 0 to shard 1 boundary", gate0ShardSize - 512, gate0ShardSize + 511},
		{"back to the start, as a scrub to zero would", 0, 4095},
		{"into the final partial shard", gate0FileSize - 2048, gate0FileSize - 1},
	}

	for _, sk := range seeks {
		t.Run(sk.name, func(t *testing.T) {
			length := sk.end - sk.start + 1
			req := httptest.NewRequest(http.MethodGet,
				"/api/files/"+file.ID.String()+"/content", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", sk.start, sk.end))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206", rec.Code)
			}
			wantCR := fmt.Sprintf("bytes %d-%d/%d", sk.start, sk.end, gate0FileSize)
			if got := rec.Header().Get("Content-Range"); got != wantCR {
				t.Errorf("Content-Range = %q, want %q", got, wantCR)
			}

			got := rec.Body.Bytes()
			want := data[sk.start : sk.start+length]
			if !bytes.Equal(got, want) {
				shard, frame, off, _ := framePosition(file.Shards, sk.start)
				t.Fatalf("bytes differ for %s\n  range = %d-%d (%d bytes)\n"+
					"  start lands on shard %d, frame %d, offset %d within that frame",
					sk.name, sk.start, sk.end, length, shard, frame, off)
			}
		})
	}
}
