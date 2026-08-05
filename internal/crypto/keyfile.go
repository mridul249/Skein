package crypto

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrKeyIDMismatch reports a key file that belongs to a different instance.
//
// This is deliberately its own sentinel rather than a generic decryption
// failure. Supplying the wrong key file and having genuinely corrupt data
// produce identical symptoms three layers down — AEAD open failures on every
// shard — and a user told "decryption failed" will go hunting for data loss
// they do not have. The key id is checked before anything is read, so this
// error can be reported instead.
var ErrKeyIDMismatch = errors.New("crypto: key file belongs to a different instance")

// ErrKeyFileMalformed reports a key file that cannot be parsed.
var ErrKeyFileMalformed = errors.New("crypto: key file is not a valid Skein key export")

const (
	keyFileHeader = "SKEIN MASTER KEY"
	keyFileIDTag  = "Key ID:"
	keyFileKeyTag = "Key:"
	keyFileDate   = "Exported:"
	keyFileRule   = "-----------------------------------------------------------"
)

// ExportKeyFile renders the master key as a self-describing text file.
//
// PLAIN TEXT, NOT A BINARY BLOB, and the prose is the point. Recovery happens
// months after export, on a different machine, by someone under stress who may
// not remember what this file was for. A bare key is a file people delete
// during a cleanup because they do not recognise it — and deleting it makes
// every shard in the instance permanently unreadable, since nothing else can
// decrypt them. Anyone who opens this file in any editor learns immediately
// what it is, which instance it belongs to, and what holding it means.
//
// The key id is included so recovery can reject a file from another instance
// before touching data, and so a human can tell two exports apart. It is
// derived (infoKeyID) and reveals nothing about the key itself.
//
// THIS FUNCTION RETURNS SECRET MATERIAL. Its result must never be logged,
// echoed into an error, or written anywhere that outlives the request that
// asked for it.
func ExportKeyFile(k *Keyring) []byte {
	var b bytes.Buffer

	b.WriteString(keyFileRule + "\n")
	b.WriteString(keyFileHeader + "\n")
	b.WriteString(keyFileRule + "\n\n")
	b.WriteString("This file is the encryption key for one Skein instance.\n\n")

	b.WriteString("WHAT IT IS FOR\n")
	b.WriteString("  Skein encrypts every file before it leaves your machine. This\n")
	b.WriteString("  key is the only thing that can decrypt them. It is NOT stored\n")
	b.WriteString("  in your database and it is NOT recoverable from your cloud\n")
	b.WriteString("  drives. If you lose this file and lose the running instance,\n")
	b.WriteString("  every file you have stored is permanently unreadable. No\n")
	b.WriteString("  support, no backup and no vendor can recover them.\n\n")

	b.WriteString("WHO CAN READ YOUR DATA\n")
	b.WriteString("  Anyone holding this file can decrypt every file in this\n")
	b.WriteString("  instance. Possession alone is enough - no password is needed.\n")
	b.WriteString("  Treat this file exactly as you would treat the data itself.\n\n")

	b.WriteString("WHERE TO KEEP IT\n")
	b.WriteString("  Somewhere separate from your Skein database and separate from\n")
	b.WriteString("  the drives holding your shards. Storing it beside the database\n")
	b.WriteString("  defeats the entire purpose: one lost disk then takes both.\n\n")

	b.WriteString("HOW TO USE IT\n")
	b.WriteString("  Set SKEIN_MASTER_KEY to the Key value below. Skein compares the\n")
	b.WriteString("  Key ID and refuses a key belonging to another instance.\n\n")

	b.WriteString(keyFileRule + "\n")
	fmt.Fprintf(&b, "%s %s\n", keyFileDate, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "%s %s\n", keyFileIDTag, k.KeyIDString())
	fmt.Fprintf(&b, "%s %s\n", keyFileKeyTag, base64.StdEncoding.EncodeToString(k.master))
	b.WriteString(keyFileRule + "\n")

	return b.Bytes()
}

// ParseKeyFile extracts the master key from an exported key file.
//
// It verifies the file against ITSELF — the key must derive to the key id the
// file states — so a hand-edited or partially corrupted file is refused rather
// than yielding a key that fails mysteriously later.
func ParseKeyFile(data []byte) ([]byte, error) {
	if !bytes.Contains(data, []byte(keyFileHeader)) {
		return nil, fmt.Errorf("%w: missing the %q header", ErrKeyFileMalformed, keyFileHeader)
	}

	var keyB64, statedID string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// "Key ID:" is checked first: it also has the "Key:" prefix.
		switch {
		case strings.HasPrefix(line, keyFileIDTag):
			statedID = strings.TrimSpace(strings.TrimPrefix(line, keyFileIDTag))
		case strings.HasPrefix(line, keyFileKeyTag):
			keyB64 = strings.TrimSpace(strings.TrimPrefix(line, keyFileKeyTag))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: could not read the file", ErrKeyFileMalformed)
	}
	if keyB64 == "" {
		return nil, fmt.Errorf("%w: no %q line", ErrKeyFileMalformed, keyFileKeyTag)
	}

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		// The decode error is deliberately not wrapped: it quotes the offending
		// input, which is key material.
		return nil, fmt.Errorf("%w: the key is not valid base64", ErrKeyFileMalformed)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w: the key is %d bytes, want %d",
			ErrKeyFileMalformed, len(key), KeyLen)
	}

	// Self-consistency: the key must derive to the id the file claims.
	ring, err := NewKeyring(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyFileMalformed, err)
	}
	if statedID != "" && !strings.EqualFold(statedID, ring.KeyIDString()) {
		return nil, fmt.Errorf("%w: the key does not match the Key ID this file states, "+
			"so it has been edited or damaged", ErrKeyFileMalformed)
	}

	return key, nil
}

// VerifyKeyFileMatches checks a key file against the instance it is being
// restored into, BEFORE any data is read.
//
// This guards the worst recovery outcome: a wrong key accepted, decrypting
// nothing correctly, leaving the user believing their data is corrupt.
// Ordering is the whole mechanism — the check runs first, so the failure is
// "wrong file" and never "your data is broken".
func VerifyKeyFileMatches(data []byte, want [keyIDLength]byte) error {
	key, err := ParseKeyFile(data)
	if err != nil {
		return err
	}
	ring, err := NewKeyring(key)
	if err != nil {
		return err
	}
	got := ring.KeyID()
	// Constant time for consistency with every other secret comparison in this
	// package. The key id is not secret; the habit is worth keeping uniform.
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return fmt.Errorf("%w: this file is for instance %s, but this instance is %s. "+
			"Your data is intact - this is simply the wrong file",
			ErrKeyIDMismatch, ring.KeyIDString(), KeyIDToString(want))
	}
	return nil
}

// KeyIDToString renders a key id in hex. It is not secret.
func KeyIDToString(id [keyIDLength]byte) string {
	return (&Keyring{keyID: id}).KeyIDString()
}
