package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 parameters from Rules.md §2.9. They are stored in every hash string,
// so raising them later re-hashes on next login rather than locking anyone out.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB, i.e. 64 MB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrInvalidHash reports a stored hash that does not parse. It is returned to
// the caller as a failed verification, never surfaced to the client.
var ErrInvalidHash = errors.New("auth: password hash is malformed")

// HashPassword returns a PHC-format argon2id hash of pw.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether pw matches encoded. Comparison is constant
// time, and the parameters come from the stored hash rather than from the
// constants above so that old hashes keep verifying after a parameter bump.
func VerifyPassword(encoded, pw string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// timingPadSalt is a fixed, public salt used only by SpendPasswordWork. It is
// never used to hash a real password, so its being a constant is harmless.
var timingPadSalt = []byte("skein-login-timing-pad-v1")

// SpendPasswordWork runs the same argon2id cost as a real verification and
// throws the result away.
//
// Login calls it when the email is unknown. Without it, an unknown address
// returns in microseconds while a known one costs 64 MB of hashing, and that
// difference is a free user-enumeration oracle.
func SpendPasswordWork(pw string) {
	_ = argon2.IDKey([]byte(pw), timingPadSalt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// NeedsRehash reports whether encoded was produced with weaker parameters than
// the current ones, so a successful login can transparently upgrade it.
func NeedsRehash(encoded string) bool {
	p, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.time < argonTime || p.memory < argonMemory || p.threads < argonThreads
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, serr := fmt.Sscanf(parts[2], "v=%d", &version); serr != nil || version != argon2.Version {
		return p, nil, nil, ErrInvalidHash
	}

	kv := map[string]uint64{}
	for _, field := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return p, nil, nil, ErrInvalidHash
		}
		n, perr := strconv.ParseUint(value, 10, 32)
		if perr != nil {
			return p, nil, nil, ErrInvalidHash
		}
		kv[name] = n
	}
	m, okM := kv["m"]
	t, okT := kv["t"]
	th, okP := kv["p"]
	if !okM || !okT || !okP || th == 0 || th > 255 {
		return p, nil, nil, ErrInvalidHash
	}
	p = argonParams{memory: uint32(m), time: uint32(t), threads: uint8(th)}

	if salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
