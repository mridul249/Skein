package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	master := make([]byte, KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("read master key: %v", err)
	}
	k, err := NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	return k
}

func TestNewKeyringRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := NewKeyring(make([]byte, n)); !errors.Is(err, ErrKeyLength) {
			t.Errorf("NewKeyring(%d bytes) = %v, want ErrKeyLength", n, err)
		}
	}
}

func TestNewKeyringCopiesTheMasterKey(t *testing.T) {
	master := bytes.Repeat([]byte{7}, KeyLen)
	k, err := NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	before := k.KeyIDString()

	// A caller zeroing its own buffer must not affect the ring.
	for i := range master {
		master[i] = 0
	}
	if k.KeyIDString() != before {
		t.Error("the keyring aliased the caller's master key slice")
	}
	if _, err := k.Seal(InfoToken, []byte("salt"), []byte("secret")); err != nil {
		t.Errorf("Seal() after the caller zeroed its buffer = %v", err)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := testKeyring(t)
	salt := []byte("account-id")

	sizes := []int{0, 1, 15, 16, 17, 1024, 64 * 1024}
	for _, n := range sizes {
		plaintext := make([]byte, n)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatalf("read plaintext: %v", err)
		}

		sealed, err := k.Seal(InfoToken, salt, plaintext)
		if err != nil {
			t.Fatalf("Seal(%d bytes) = %v", n, err)
		}
		if bytes.Contains(sealed, plaintext) && n > 0 {
			t.Fatalf("the plaintext appears verbatim in the ciphertext (n=%d)", n)
		}

		got, err := k.Open(InfoToken, salt, sealed)
		if err != nil {
			t.Fatalf("Open(%d bytes) = %v", n, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip mismatch at %d bytes", n)
		}
	}
}

// Rules.md §2.9: every ciphertext carries a version byte and a key id.
func TestEnvelopeCarriesVersionAndKeyID(t *testing.T) {
	k := testKeyring(t)

	sealed, err := k.Seal(InfoToken, []byte("salt"), []byte("a refresh token"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if sealed[0] != EnvelopeV1 {
		t.Errorf("version byte = %d, want %d", sealed[0], EnvelopeV1)
	}
	id := k.KeyID()
	if !bytes.Equal(sealed[1:1+keyIDLength], id[:]) {
		t.Error("the key id is not present in the envelope header")
	}
	if len(sealed) != headerLen+len("a refresh token")+tagLen {
		t.Errorf("envelope length = %d, want header + plaintext + tag", len(sealed))
	}
}

func TestSealUsesAFreshNoncePerCall(t *testing.T) {
	k := testKeyring(t)
	salt := []byte("salt")

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		sealed, err := k.Seal(InfoToken, salt, []byte("same plaintext every time"))
		if err != nil {
			t.Fatalf("Seal() = %v", err)
		}
		nonce := string(sealed[1+keyIDLength : headerLen])
		if seen[nonce] {
			t.Fatal("a nonce repeated under the same key")
		}
		seen[nonce] = true
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	k := testKeyring(t)
	salt := []byte("salt")
	plaintext := []byte("a refresh token nobody should read")

	sealed, err := k.Seal(InfoToken, salt, plaintext)
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr error
	}{
		{
			name:    "flipped version byte",
			mutate:  func(b []byte) []byte { c := clone(b); c[0] = 2; return c },
			wantErr: ErrUnknownVersion,
		},
		{
			name:    "flipped key id",
			mutate:  func(b []byte) []byte { c := clone(b); c[1] ^= 0xff; return c },
			wantErr: ErrWrongKey,
		},
		{
			name:    "flipped nonce",
			mutate:  func(b []byte) []byte { c := clone(b); c[6] ^= 0x01; return c },
			wantErr: ErrDecrypt,
		},
		{
			name:    "flipped ciphertext",
			mutate:  func(b []byte) []byte { c := clone(b); c[headerLen] ^= 0x01; return c },
			wantErr: ErrDecrypt,
		},
		{
			name:    "flipped tag",
			mutate:  func(b []byte) []byte { c := clone(b); c[len(c)-1] ^= 0x01; return c },
			wantErr: ErrDecrypt,
		},
		{
			name:    "truncated",
			mutate:  func(b []byte) []byte { return clone(b)[:len(b)-1] },
			wantErr: ErrDecrypt,
		},
		{
			name:    "empty",
			mutate:  func([]byte) []byte { return nil },
			wantErr: ErrCiphertextTooShort,
		},
		{
			name:    "header only",
			mutate:  func(b []byte) []byte { return clone(b)[:headerLen] },
			wantErr: ErrCiphertextTooShort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := k.Open(InfoToken, salt, tc.mutate(sealed))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Open() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The info string and the salt are both bound into the key, so a token
// ciphertext cannot be replayed as a file key or moved between accounts.
func TestOpenRejectsWrongInfoOrSalt(t *testing.T) {
	k := testKeyring(t)

	sealed, err := k.Seal(InfoToken, []byte("account-a"), []byte("secret"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	if _, err := k.Open(InfoFile, []byte("account-a"), sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with the wrong info = %v, want ErrDecrypt", err)
	}
	if _, err := k.Open(InfoToken, []byte("account-b"), sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with the wrong salt = %v, want ErrDecrypt", err)
	}
}

func TestOpenRejectsADifferentMasterKey(t *testing.T) {
	a, b := testKeyring(t), testKeyring(t)

	sealed, err := a.Seal(InfoToken, []byte("salt"), []byte("secret"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if _, err := b.Open(InfoToken, []byte("salt"), sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Open() with another master key = %v, want ErrWrongKey", err)
	}
}

func TestDeriveIsDeterministicAndSeparated(t *testing.T) {
	k := testKeyring(t)

	a1, err := k.Derive(InfoToken, []byte("x"))
	if err != nil {
		t.Fatalf("Derive() = %v", err)
	}
	a2, err := k.Derive(InfoToken, []byte("x"))
	if err != nil {
		t.Fatalf("Derive() = %v", err)
	}
	if !bytes.Equal(a1, a2) {
		t.Fatal("Derive() is not deterministic")
	}
	if len(a1) != KeyLen {
		t.Fatalf("derived key = %d bytes, want %d", len(a1), KeyLen)
	}

	other, err := k.Derive(InfoFile, []byte("x"))
	if err != nil {
		t.Fatalf("Derive() = %v", err)
	}
	if bytes.Equal(a1, other) {
		t.Fatal("two info strings produced the same key")
	}

	salted, err := k.Derive(InfoToken, []byte("y"))
	if err != nil {
		t.Fatalf("Derive() = %v", err)
	}
	if bytes.Equal(a1, salted) {
		t.Fatal("two salts produced the same key")
	}
}

func TestSealStringRoundTrip(t *testing.T) {
	k := testKeyring(t)
	const secret = "ya29.a0AfH6SM…not-a-real-token"

	sealed, err := k.SealString(InfoToken, []byte("acct"), secret)
	if err != nil {
		t.Fatalf("SealString() = %v", err)
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatal("the token appears verbatim in the ciphertext")
	}

	got, err := k.OpenString(InfoToken, []byte("acct"), sealed)
	if err != nil {
		t.Fatalf("OpenString() = %v", err)
	}
	if got != secret {
		t.Errorf("OpenString() = %q, want %q", got, secret)
	}
}

func clone(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
