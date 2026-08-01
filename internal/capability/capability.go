// Package capability mints and verifies the short-lived, stateless grants that
// let a browser fetch one file's bytes without an Authorization header.
//
// Why it exists: the access token lives in a JS variable (Rules.md §2.15), and
// a browser-driven transfer — an <a download>, an <img>, a <video> — cannot set
// a header. The alternative is fetching through JS and handing the result to
// the page, which means materialising the whole response in memory. That was
// known issue #15: a client-side violation of the constant-memory claim in
// Rules.md §2.1, at the one layer no server test covers.
//
// # Why the grant is not single-use
//
// It is deliberately neither single-use nor stored. A ranged media element
// spends one credential across many requests by construction, and one plain
// download is a single GET that may stream for an hour; a redemption counter
// breaks the first and buys nothing on the second. A table would cost a row
// write per range request plus a reaper, to defend a credential that already
// expires on its own.
//
// The properties carrying the security here are narrow scope and short expiry,
// not redemption counting. Phase 7 Task 5.3's single-use rule governs vault
// import, where exactly one redemption is the entire point of the credential.
// Different problem, different rule. Do not conflate the two standards.
package capability

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

// TTL is how long a minted grant stays valid.
//
// Fifteen minutes bounds the window in which a leaked URL is useful, and it is
// not a limit on transfer duration. Expiry is checked once, when the request
// arrives; a signature accepted at that moment authorises a stream of any
// length, so a 15 GB download over a slow link is unaffected by this value —
// only the delay between minting the URL and starting the request is. That is
// the non-obvious part of the design and the reason a short TTL costs nothing
// here.
const TTL = 15 * time.Minute

// purpose is bound into every signature so a grant can never be replayed
// against a different route or against a future kind of capability. A signature
// minted for content is not a signature for anything else, even under the same
// key and for the same file.
const purpose = "content-v1"

// The query parameters a grant travels in.
const (
	ParamUser      = "u"
	ParamExpires   = "exp"
	ParamSignature = "sig"
)

// ErrInvalid is the only failure Verify reports.
//
// Forged, tampered, malformed and expired all return this one error on
// purpose: telling a caller which of those it was tells an attacker whether a
// signature was structurally right, which is the one bit worth not giving away.
var ErrInvalid = errors.New("capability: grant is not valid")

// Signer mints and verifies content grants under a key derived from the master
// key.
type Signer struct {
	key []byte
}

// NewSigner derives the capability signing key from the keyring.
func NewSigner(k *skcrypto.Keyring) (*Signer, error) {
	if k == nil {
		return nil, errors.New("capability: nil keyring")
	}
	key, err := k.Derive(skcrypto.InfoCapability, nil)
	if err != nil {
		return nil, fmt.Errorf("derive capability key: %w", err)
	}
	return &Signer{key: key}, nil
}

// mac computes the signature over the grant's four bound fields.
//
// The purpose is NUL-terminated and everything after it is fixed width — two
// 16-byte UUIDs and an 8-byte expiry — so the signed input has exactly one
// parse. No field can be shifted into another by choosing a value that looks
// like a delimiter.
func (s *Signer) mac(fileID, userID uuid.UUID, exp int64) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write(fileID[:])
	m.Write(userID[:])
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(exp))
	m.Write(e[:])
	return m.Sum(nil)
}

// Sign returns the query parameters that carry a grant for one file, held by
// one user, until expires.
func (s *Signer) Sign(fileID, userID uuid.UUID, expires time.Time) url.Values {
	exp := expires.Unix()
	v := url.Values{}
	v.Set(ParamUser, userID.String())
	v.Set(ParamExpires, strconv.FormatInt(exp, 10))
	v.Set(ParamSignature, base64.RawURLEncoding.EncodeToString(s.mac(fileID, userID, exp)))
	return v
}

// Verify checks a grant against the file actually being requested and returns
// the user it authorises.
//
// fileID comes from the request path, not from the query, which is what stops a
// grant for one file reaching another: the signature is recomputed over the
// path's file id, so a grant presented at a different URL simply does not
// verify.
func (s *Signer) Verify(fileID uuid.UUID, q url.Values, now time.Time) (uuid.UUID, error) {
	userID, err := uuid.Parse(q.Get(ParamUser))
	if err != nil {
		return uuid.Nil, ErrInvalid
	}
	exp, err := strconv.ParseInt(q.Get(ParamExpires), 10, 64)
	if err != nil {
		return uuid.Nil, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(q.Get(ParamSignature))
	if err != nil {
		return uuid.Nil, ErrInvalid
	}

	// The MAC is checked before the expiry is believed. Until this succeeds
	// exp is attacker-supplied text; afterwards it is a value this server
	// signed. Checking expiry first would be checking a number the caller
	// chose. Rules.md §2.9: constant time.
	//
	// Do not "simplify" this to ==. No test covers the difference and none
	// can: ConstantTimeCompare and == are functionally identical and differ
	// only in timing, so swapping them leaves the suite green. That was
	// confirmed by mutation on 2026-07-30 — the swap broke nothing, while
	// removing the comparison altogether failed seven assertions, which is
	// how the tests were shown to be non-vacuous. The absence of a failing
	// test here is therefore evidence about what tests can observe, not
	// evidence that the call is unnecessary.
	if subtle.ConstantTimeCompare(got, s.mac(fileID, userID, exp)) != 1 {
		return uuid.Nil, ErrInvalid
	}
	if !now.Before(time.Unix(exp, 0)) {
		return uuid.Nil, ErrInvalid
	}
	return userID, nil
}
