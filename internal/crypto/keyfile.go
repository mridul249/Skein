package crypto

import "errors"

// ErrKeyIDMismatch reports a key file belonging to a different instance.
var ErrKeyIDMismatch = errors.New("crypto: key file belongs to a different instance")

// STUB — pending implementation.
func ExportKeyFile(*Keyring) []byte { return nil }

// STUB — pending implementation.
func ParseKeyFile([]byte) ([]byte, error) { return nil, errors.New("not implemented") }

// STUB — pending implementation.
func VerifyKeyFileMatches([]byte, [keyIDLength]byte) error { return nil }
