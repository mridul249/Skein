// Package crypto derives every key in Skein from one master key and provides
// the two ciphertext formats: a one-shot envelope for small secrets, and a
// framed stream for file content.
//
// Only crypto/* and golang.org/x/crypto are used. Nothing here is a
// hand-rolled primitive; the code is composition of standard ones, and the
// parts worth reviewing are the key derivation, the nonce discipline, and the
// framing.
package crypto

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeyLen is the length of every derived key: AES-256.
const KeyLen = 32

// Info strings. Each one namespaces a derived key so that a key used for
// OAuth tokens can never coincide with a key used for file content, even
// though both come from the same master secret.
//
//nolint:gosec // G101: these are HKDF domain separators, not credentials.
const (
	InfoToken   = "skein-token-v1"
	InfoFile    = "skein-file-v1"
	InfoShare   = "skein-share-v1"
	InfoOAuth   = "skein-oauth-state-v1"
	infoKeyID   = "skein-key-id-v1"
	keyIDLength = 4
)

// ErrKeyLength reports a master key that is not 32 bytes.
var ErrKeyLength = errors.New("crypto: master key must be 32 bytes")

// Keyring derives purpose-specific keys from a single master key.
//
// The master key never encrypts anything directly. Every ciphertext in the
// system is produced under a key derived from it, so the master key's only
// exposure is in memory, and a compromise of any one derived key does not
// reveal the others.
type Keyring struct {
	master []byte
	keyID  [keyIDLength]byte
}

// NewKeyring builds a keyring from a 32-byte master key.
func NewKeyring(master []byte) (*Keyring, error) {
	if len(master) != KeyLen {
		return nil, fmt.Errorf("%w, got %d", ErrKeyLength, len(master))
	}
	// Defensive copy: the caller's slice may be reused or zeroed.
	m := make([]byte, KeyLen)
	copy(m, master)

	k := &Keyring{master: m}

	// The key id is itself derived, so it identifies the master key without
	// revealing any of it. It is stored in the clear next to every
	// ciphertext, which is what makes a future rotation able to tell which
	// key a given record needs.
	id, err := hkdf.Key(sha256.New, m, nil, infoKeyID, keyIDLength)
	if err != nil {
		return nil, fmt.Errorf("derive key id: %w", err)
	}
	copy(k.keyID[:], id)
	return k, nil
}

// KeyID returns the 4-byte identifier of the master key this ring wraps.
func (k *Keyring) KeyID() [keyIDLength]byte { return k.keyID }

// KeyIDString returns the key id in hex, for logs and diagnostics. It is not
// secret.
func (k *Keyring) KeyIDString() string { return hex.EncodeToString(k.keyID[:]) }

// Derive returns a 32-byte key for the given purpose and salt.
//
// salt is the per-record binding — a file id for content, an account id for a
// token — so two records of the same kind never share a key. info is the
// purpose separator.
func (k *Keyring) Derive(info string, salt []byte) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, k.master, salt, info, KeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive %s key: %w", info, err)
	}
	return key, nil
}
