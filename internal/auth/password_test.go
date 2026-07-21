package auth

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// mustWeakKey produces the key half of a hash string built with deliberately
// low parameters, so the rehash-on-login path can be exercised.
func mustWeakKey(t *testing.T, pw string) string {
	t.Helper()
	salt, err := base64.RawStdEncoding.DecodeString("c2FsdHNhbHRzYWx0c2FsdA")
	if err != nil {
		t.Fatalf("decode fixture salt: %v", err)
	}
	key := argon2.IDKey([]byte(pw), salt, 1, 4096, 1, 32)
	return base64.RawStdEncoding.EncodeToString(key)
}

func TestHashAndVerifyPassword(t *testing.T) {
	const pw = "correct horse battery staple"

	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("the hash contains the plaintext")
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash = %q, want the Rules.md §2.9 parameters", hash)
	}

	ok, err := VerifyPassword(hash, pw)
	if err != nil {
		t.Fatalf("VerifyPassword() = %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false for the correct password")
	}

	ok, err = VerifyPassword(hash, pw+"x")
	if err != nil {
		t.Fatalf("VerifyPassword() = %v", err)
	}
	if ok {
		t.Error("VerifyPassword() = true for a wrong password")
	}
}

func TestHashPasswordUsesAFreshSalt(t *testing.T) {
	const pw = "correct horse battery staple"

	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
}

// A malformed stored hash must fail verification, never panic. Rules.md §2.6.
func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	malformed := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=19$m=notanumber,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!not-base64$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!not-base64",
		"$argon2id$v=19$m=65536,t=3,p=4$$",
		"$argon2id$v=19$m=65536,t=3,p=999$c2FsdA$a2V5",
	}
	for _, h := range malformed {
		t.Run(h, func(t *testing.T) {
			ok, err := VerifyPassword(h, "anything")
			if ok {
				t.Fatalf("VerifyPassword(%q) = true", h)
			}
			if err == nil {
				t.Errorf("VerifyPassword(%q) returned no error", h)
			}
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	current, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	if NeedsRehash(current) {
		t.Error("NeedsRehash() = true for a hash at current parameters")
	}

	weak := "$argon2id$v=19$m=4096,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$" + mustWeakKey(t, "x")
	if !NeedsRehash(weak) {
		t.Error("NeedsRehash() = false for weaker parameters")
	}
	if !NeedsRehash("garbage") {
		t.Error("NeedsRehash() = false for an unparseable hash")
	}
}

func TestVerifyPasswordHonoursStoredParameters(t *testing.T) {
	// A hash written with old parameters must keep verifying after the
	// package constants are raised, or a parameter bump locks everyone out.
	const pw = "correct horse battery staple"
	weak := "$argon2id$v=19$m=4096,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$" + mustWeakKey(t, pw)

	ok, err := VerifyPassword(weak, pw)
	if err != nil {
		t.Fatalf("VerifyPassword() = %v", err)
	}
	if !ok {
		t.Error("a hash at old parameters no longer verifies")
	}
}

func TestSpendPasswordWorkDoesNotPanic(t *testing.T) {
	SpendPasswordWork("")
	SpendPasswordWork(strings.Repeat("x", 256))
}
