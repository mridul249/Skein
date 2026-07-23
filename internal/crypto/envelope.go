package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Envelope version byte. Every ciphertext carries one.
//
// The reference project stored sha256(passphrase)-encrypted blobs with no
// version and no key id, which is a one-way door: there is no way to tell an
// old ciphertext from a new one, so the format can never change and the key
// can never rotate. One byte prevents that permanently.
const (
	EnvelopeV1 byte = 1

	nonceLen  = 12 // AES-GCM standard nonce
	tagLen    = 16
	headerLen = 1 + keyIDLength + nonceLen
)

// Errors returned when a stored ciphertext cannot be read.
var (
	ErrCiphertextTooShort = errors.New("crypto: ciphertext is shorter than its header")
	ErrUnknownVersion     = errors.New("crypto: unknown envelope version")
	ErrWrongKey           = errors.New("crypto: ciphertext was written with a different master key")
	ErrDecrypt            = errors.New("crypto: decryption failed")
)

// Seal encrypts plaintext under a key derived from (info, salt) and returns a
// self-describing envelope:
//
//	version(1) || key_id(4) || nonce(12) || ciphertext || tag(16)
//
// The header is authenticated as additional data, so an attacker cannot swap
// the version or key id of a stored record without the open failing.
//
// Seal is for secrets that fit comfortably in memory — OAuth tokens, share
// passwords. File content never comes through here; it goes through the framed
// stream in stream.go, because a multi-gigabyte GCM message is one message
// whose integrity cannot be checked until the last byte has arrived.
func (k *Keyring) Seal(info string, salt, plaintext []byte) ([]byte, error) {
	key, err := k.Derive(info, salt)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	out := make([]byte, headerLen, headerLen+len(plaintext)+tagLen)
	out[0] = EnvelopeV1
	copy(out[1:], k.keyID[:])

	nonce := out[1+keyIDLength : headerLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}

	header := out[:headerLen]
	return aead.Seal(out, nonce, plaintext, header), nil
}

// Open reverses Seal. info and salt must match what was used to write the
// record; a mismatch derives a different key and fails authentication, which
// is the intended behaviour rather than a silent wrong answer.
func (k *Keyring) Open(info string, salt, envelope []byte) ([]byte, error) {
	if len(envelope) < headerLen+tagLen {
		return nil, ErrCiphertextTooShort
	}
	if envelope[0] != EnvelopeV1 {
		return nil, fmt.Errorf("%w: %d", ErrUnknownVersion, envelope[0])
	}
	// Checked before attempting the open so that a rotated deployment gets
	// a diagnosable error instead of a generic authentication failure.
	if string(envelope[1:1+keyIDLength]) != string(k.keyID[:]) {
		return nil, ErrWrongKey
	}

	key, err := k.Derive(info, salt)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	header := envelope[:headerLen]
	nonce := envelope[1+keyIDLength : headerLen]
	body := envelope[headerLen:]

	plaintext, err := aead.Open(nil, nonce, body, header)
	if err != nil {
		// The underlying error says only that authentication failed; it
		// is wrapped rather than returned so callers match on one
		// sentinel and nothing about the key reaches a log line.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// SealString is Seal for a string secret.
func (k *Keyring) SealString(info string, salt []byte, s string) ([]byte, error) {
	return k.Seal(info, salt, []byte(s))
}

// OpenString is Open for a string secret.
func (k *Keyring) OpenString(info string, salt, envelope []byte) (string, error) {
	b, err := k.Open(info, salt, envelope)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return aead, nil
}
