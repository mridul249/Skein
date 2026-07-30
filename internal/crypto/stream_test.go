package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func encrypt(t *testing.T, k *Keyring, fileID []byte, shard uint32, plain []byte) []byte {
	t.Helper()
	r, err := k.NewEncryptReader(fileID, shard, bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewEncryptReader() = %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return out
}

func TestStreamRoundTrip(t *testing.T) {
	k := testKeyring(t)
	fileID := []byte("file-id-0001")

	sizes := []int{
		0, 1, 100,
		FrameSize - 1, FrameSize, FrameSize + 1,
		2 * FrameSize, 2*FrameSize + 7,
		5*FrameSize + 1234,
	}

	for _, size := range sizes {
		plain := randomBytes(t, size)
		cipher := encrypt(t, k, fileID, 0, plain)

		// The declared stored size has to be exact: a resumable session
		// commits to its length before the first byte is sent.
		if want := StreamOverhead(int64(size)); int64(len(cipher)) != want {
			t.Errorf("size %d: ciphertext is %d bytes, StreamOverhead said %d",
				size, len(cipher), want)
		}
		// Only meaningful once the plaintext is long enough that a coincidental
		// match is impossible. A 1-byte plaintext encrypts to 22 bytes (5-byte
		// header + 1 + 16-byte tag), so the chance that some byte of it happens
		// to equal that one plaintext byte is 1-(255/256)^22 ≈ 8% — a false
		// failure, not a leak. At 16 bytes the coincidence needs 16 consecutive
		// bytes to match, around 2^-128. Do not lower this bound.
		if size >= 16 && bytes.Contains(cipher, plain) {
			t.Errorf("size %d: the plaintext appears verbatim in the ciphertext", size)
		}

		dec, err := k.NewDecryptReader(fileID, 0, bytes.NewReader(cipher))
		if err != nil {
			t.Fatalf("NewDecryptReader() = %v", err)
		}
		got, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("size %d: decrypt = %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("size %d: round trip changed the bytes", size)
		}
	}

	// One distinctive plaintext, long enough for the leak check above to carry
	// real weight rather than being skipped by the 16-byte guard. A repeated
	// marker rather than incrementing or zero bytes: those occur naturally in
	// headers and padding, so finding them would prove nothing either way.
	marker := []byte("SKEINPLAINTEXTMARKER0123456789AB") // 32 bytes
	cipher := encrypt(t, k, fileID, 0, marker)
	if bytes.Contains(cipher, marker) {
		t.Error("the marker plaintext appears verbatim in the ciphertext")
	}
	dec, err := k.NewDecryptReader(fileID, 0, bytes.NewReader(cipher))
	if err != nil {
		t.Fatalf("NewDecryptReader(marker) = %v", err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decrypt marker = %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Error("round trip changed the marker plaintext")
	}
}

func TestStreamHeaderCarriesVersionAndKeyID(t *testing.T) {
	k := testKeyring(t)
	cipher := encrypt(t, k, []byte("f"), 0, []byte("hello"))

	if cipher[0] != StreamV1 {
		t.Errorf("version = %d, want %d", cipher[0], StreamV1)
	}
	id := k.KeyID()
	if !bytes.Equal(cipher[1:1+keyIDLength], id[:]) {
		t.Error("the key id is missing from the stream header")
	}
}

func TestStreamRejectsTampering(t *testing.T) {
	k := testKeyring(t)
	fileID := []byte("file-id")
	plain := randomBytes(t, 3*FrameSize+99)
	original := encrypt(t, k, fileID, 0, plain)

	tests := []struct {
		name   string
		mutate func([]byte) []byte
		errIs  error
	}{
		{
			name:   "version byte",
			mutate: func(b []byte) []byte { c := clone(b); c[0] = 9; return c },
			errIs:  ErrStreamVersion,
		},
		{
			name:   "key id",
			mutate: func(b []byte) []byte { c := clone(b); c[2] ^= 0xff; return c },
			errIs:  ErrStreamKey,
		},
		{
			name:   "first frame ciphertext",
			mutate: func(b []byte) []byte { c := clone(b); c[StreamHeaderLen+10] ^= 1; return c },
			errIs:  ErrStreamCorrupt,
		},
		{
			name:   "a middle frame",
			mutate: func(b []byte) []byte { c := clone(b); c[StreamHeaderLen+FrameSize+100] ^= 1; return c },
			errIs:  ErrStreamCorrupt,
		},
		{
			name:   "final tag",
			mutate: func(b []byte) []byte { c := clone(b); c[len(c)-1] ^= 1; return c },
			errIs:  ErrStreamCorrupt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := k.NewDecryptReader(fileID, 0, bytes.NewReader(tc.mutate(original)))
			if err != nil {
				t.Fatalf("NewDecryptReader() = %v", err)
			}
			_, err = io.ReadAll(dec)
			if !errors.Is(err, tc.errIs) {
				t.Fatalf("decrypt = %v, want %v", err, tc.errIs)
			}
		})
	}
}

// A frame moved between shards, or reordered within one, must fail rather than
// decrypt into plausible-looking wrong plaintext. The frame index and the
// shard index are both in the nonce and the AAD, which is what enforces it.
func TestStreamFramesAreBoundToTheirPosition(t *testing.T) {
	k := testKeyring(t)
	fileID := []byte("file-id")
	plain := randomBytes(t, 2*FrameSize)

	shard0 := encrypt(t, k, fileID, 0, plain)

	t.Run("read shard 0 as shard 1", func(t *testing.T) {
		dec, err := k.NewDecryptReader(fileID, 1, bytes.NewReader(shard0))
		if err != nil {
			t.Fatalf("NewDecryptReader() = %v", err)
		}
		if _, err := io.ReadAll(dec); !errors.Is(err, ErrStreamCorrupt) {
			t.Fatalf("decrypt = %v, want ErrStreamCorrupt", err)
		}
	})

	t.Run("swapped frames", func(t *testing.T) {
		const stride = FrameSize + frameOverhead
		swapped := clone(shard0)
		a := swapped[StreamHeaderLen : StreamHeaderLen+stride]
		b := swapped[StreamHeaderLen+stride : StreamHeaderLen+2*stride]
		tmp := clone(a)
		copy(a, b)
		copy(b, tmp)

		dec, err := k.NewDecryptReader(fileID, 0, bytes.NewReader(swapped))
		if err != nil {
			t.Fatalf("NewDecryptReader() = %v", err)
		}
		if _, err := io.ReadAll(dec); !errors.Is(err, ErrStreamCorrupt) {
			t.Fatalf("decrypt = %v, want ErrStreamCorrupt", err)
		}
	})

	t.Run("wrong file id", func(t *testing.T) {
		dec, err := k.NewDecryptReader([]byte("another-file"), 0, bytes.NewReader(shard0))
		if err != nil {
			t.Fatalf("NewDecryptReader() = %v", err)
		}
		if _, err := io.ReadAll(dec); !errors.Is(err, ErrStreamCorrupt) {
			t.Fatalf("decrypt = %v, want ErrStreamCorrupt", err)
		}
	})
}

func TestStreamNoncesNeverRepeat(t *testing.T) {
	seen := map[string]bool{}
	for shard := uint32(0); shard < 4; shard++ {
		for frame := uint64(0); frame < 1000; frame++ {
			n := string(frameNonce(shard, frame))
			if seen[n] {
				t.Fatalf("nonce repeated at shard %d frame %d", shard, frame)
			}
			seen[n] = true
		}
	}
}

func TestStreamOverheadMatchesRealOutput(t *testing.T) {
	k := testKeyring(t)
	for _, size := range []int64{0, 1, FrameSize - 1, FrameSize, FrameSize + 1, 10 * FrameSize} {
		got := int64(len(encrypt(t, k, []byte("f"), 0, make([]byte, size))))
		if want := StreamOverhead(size); got != want {
			t.Errorf("plain %d: real ciphertext %d, StreamOverhead %d", size, got, want)
		}
	}
}

// The property that makes scrubbing an encrypted striped video work: a range
// request decrypts only the frames it overlaps.
func TestCipherRangeAndRangeDecrypt(t *testing.T) {
	k := testKeyring(t)
	fileID := []byte("file-id")

	const plainSize = 5*FrameSize + 4321
	plain := randomBytes(t, plainSize)
	cipher := encrypt(t, k, fileID, 0, plain)

	tests := []struct {
		name       string
		start      int64
		length     int64
		wantFrames int64 // how many frames the request should touch
	}{
		{"first byte", 0, 1, 1},
		{"within the first frame", 100, 500, 1},
		{"across a frame boundary", FrameSize - 10, 20, 2},
		{"a whole middle frame", 2 * FrameSize, FrameSize, 1},
		{"spanning three frames", FrameSize + 5, 2 * FrameSize, 3},
		{"the tail", 5 * FrameSize, 4321, 1},
		{"last byte", plainSize - 1, 1, 1},
		{"everything", 0, plainSize, 6},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cStart, cLen, firstFrame, skip := CipherRange(tc.start, tc.length, plainSize)

			// Only the frames that overlap are fetched.
			const stride = FrameSize + frameOverhead
			touched := (cLen + stride - 1) / stride
			if touched != tc.wantFrames {
				t.Errorf("fetched %d frames, want %d", touched, tc.wantFrames)
			}
			if cStart+cLen > int64(len(cipher)) {
				t.Fatalf("range [%d,%d) runs past the %d-byte ciphertext",
					cStart, cStart+cLen, len(cipher))
			}

			slice := cipher[cStart : cStart+cLen]
			dec, err := k.NewDecryptRangeReader(fileID, 0, bytes.NewReader(slice),
				firstFrame, skip, tc.length)
			if err != nil {
				t.Fatalf("NewDecryptRangeReader() = %v", err)
			}
			got, err := io.ReadAll(dec)
			if err != nil {
				t.Fatalf("decrypt range = %v", err)
			}

			want := plain[tc.start : tc.start+tc.length]
			if !bytes.Equal(got, want) {
				t.Errorf("range [%d,%d): got %d bytes, want %d; contents differ",
					tc.start, tc.start+tc.length, len(got), len(want))
			}
		})
	}
}

func TestCipherRangeEdgeCases(t *testing.T) {
	const size = 10 * FrameSize

	tests := []struct {
		name   string
		start  int64
		length int64
	}{
		{"zero length", 100, 0},
		{"negative length", 100, -5},
		{"start past the end", size + 1, 10},
		{"start exactly at the end", size, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, cLen, _, _ := CipherRange(tc.start, tc.length, size)
			if cLen != 0 {
				t.Errorf("cipher length = %d, want 0", cLen)
			}
		})
	}

	t.Run("length past the end is clamped", func(t *testing.T) {
		cStart, cLen, _, _ := CipherRange(size-100, 99999, size)
		if cStart+cLen > StreamOverhead(size) {
			t.Errorf("clamped range still runs past the ciphertext")
		}
	})

	t.Run("negative start is treated as zero", func(t *testing.T) {
		_, _, frame, skip := CipherRange(-5, 10, size)
		if frame != 0 || skip != 0 {
			t.Errorf("frame = %d skip = %d, want 0 0", frame, skip)
		}
	})
}

func TestFrameCount(t *testing.T) {
	tests := []struct {
		size int64
		want int64
	}{
		{-1, 0},
		{0, 0},
		{1, 1},
		{FrameSize - 1, 1},
		{FrameSize, 1},
		{FrameSize + 1, 2},
		{10 * FrameSize, 10},
		{10*FrameSize + 1, 11},
	}
	for _, tc := range tests {
		if got := FrameCount(tc.size); got != tc.want {
			t.Errorf("FrameCount(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

// Truncating the stream must fail rather than returning a short plaintext that
// looks complete.
func TestStreamTruncationIsDetected(t *testing.T) {
	k := testKeyring(t)
	fileID := []byte("file-id")
	plain := randomBytes(t, 2*FrameSize)
	cipher := encrypt(t, k, fileID, 0, plain)

	// Chop off part of the final frame.
	truncated := cipher[:len(cipher)-100]

	dec, err := k.NewDecryptReader(fileID, 0, bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("NewDecryptReader() = %v", err)
	}
	got, err := io.ReadAll(dec)
	if err == nil {
		t.Fatalf("a truncated stream decrypted cleanly to %d bytes", len(got))
	}
}

func TestStreamHoldsConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the streaming memory check in -short mode")
	}
	k := testKeyring(t)

	// 256 MiB through the encrypt and decrypt readers. If either buffered
	// the whole stream this would be visible immediately.
	const size = 256 << 20

	enc, err := k.NewEncryptReader([]byte("f"), 0, io.LimitReader(zeroes{}, size))
	if err != nil {
		t.Fatalf("NewEncryptReader() = %v", err)
	}
	n, err := io.Copy(io.Discard, enc)
	if err != nil {
		t.Fatalf("encrypt copy = %v", err)
	}
	if n != StreamOverhead(size) {
		t.Errorf("encrypted %d bytes, want %d", n, StreamOverhead(size))
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }
