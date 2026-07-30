package capability_test

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/capability"
	skcrypto "github.com/mridul60214/skein/internal/crypto"
)

func testSigner(t *testing.T) *capability.Signer {
	t.Helper()
	master := make([]byte, skcrypto.KeyLen)
	for i := range master {
		master[i] = byte(i)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	s, err := capability.NewSigner(ring)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}
	return s
}

// TestGrantIsScopedToOneFileAndUser is the property that makes a capability URL
// safe to hand to a browser: it authorises exactly one file for exactly one
// user, and nothing else.
func TestGrantIsScopedToOneFileAndUser(t *testing.T) {
	s := testSigner(t)
	now := time.Now()

	fileA, fileB := uuid.New(), uuid.New()
	user := uuid.New()

	grant := s.Sign(fileA, user, now.Add(capability.TTL))

	got, err := s.Verify(fileA, grant, now)
	if err != nil {
		t.Fatalf("Verify(fileA) = %v, want nil", err)
	}
	if got != user {
		t.Errorf("Verify(fileA) user = %v, want %v", got, user)
	}

	// The whole point: presenting A's grant at B's URL must not work.
	if _, err := s.Verify(fileB, grant, now); err == nil {
		t.Error("a grant minted for file A verified against file B")
	}
}

// TestVerifyRejectsEveryTamperedField walks each signed field and confirms that
// changing it invalidates the grant. Expiry has its own case because extending
// it is the attack a stateless credential invites.
func TestVerifyRejectsEveryTamperedField(t *testing.T) {
	s := testSigner(t)
	now := time.Now()
	fileID := uuid.New()
	user := uuid.New()
	valid := s.Sign(fileID, user, now.Add(capability.TTL))

	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"expiry pushed a year out", func(v url.Values) {
			v.Set(capability.ParamExpires, strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10))
		}},
		{"expiry pulled back", func(v url.Values) {
			v.Set(capability.ParamExpires, strconv.FormatInt(now.Add(-time.Hour).Unix(), 10))
		}},
		{"user swapped for another", func(v url.Values) {
			v.Set(capability.ParamUser, uuid.New().String())
		}},
		{"signature flipped in one byte", func(v url.Values) {
			sig := []byte(v.Get(capability.ParamSignature))
			if sig[0] == 'A' {
				sig[0] = 'B'
			} else {
				sig[0] = 'A'
			}
			v.Set(capability.ParamSignature, string(sig))
		}},
		{"signature removed", func(v url.Values) {
			v.Del(capability.ParamSignature)
		}},
		{"signature emptied", func(v url.Values) {
			v.Set(capability.ParamSignature, "")
		}},
		{"expiry not a number", func(v url.Values) {
			v.Set(capability.ParamExpires, "soon")
		}},
		{"user not a uuid", func(v url.Values) {
			v.Set(capability.ParamUser, "../../etc/passwd")
		}},
		{"everything stripped", func(v url.Values) {
			for k := range v {
				v.Del(k)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, vs := range valid {
				q[k] = append([]string(nil), vs...)
			}
			tc.mutate(q)

			if _, err := s.Verify(fileID, q, now); err == nil {
				t.Fatal("Verify() = nil, want an error")
			}
		})
	}
}

// TestGrantExpires pins the one time-dependent property.
func TestGrantExpires(t *testing.T) {
	s := testSigner(t)
	now := time.Now()
	fileID, user := uuid.New(), uuid.New()
	expires := now.Add(capability.TTL)
	grant := s.Sign(fileID, user, expires)

	if _, err := s.Verify(fileID, grant, expires.Add(-time.Second)); err != nil {
		t.Errorf("a second before expiry: Verify() = %v, want nil", err)
	}
	// Exactly at the deadline is already too late: the check is `now < exp`.
	if _, err := s.Verify(fileID, grant, expires); err == nil {
		t.Error("at the expiry instant: Verify() = nil, want an error")
	}
	if _, err := s.Verify(fileID, grant, expires.Add(time.Second)); err == nil {
		t.Error("a second after expiry: Verify() = nil, want an error")
	}
}

// TestGrantsFromAnotherKeyAreRejected confirms the signature is what carries
// the authority, not the shape of the URL.
func TestGrantsFromAnotherKeyAreRejected(t *testing.T) {
	mine := testSigner(t)

	other := make([]byte, skcrypto.KeyLen)
	for i := range other {
		other[i] = byte(255 - i)
	}
	ring, err := skcrypto.NewKeyring(other)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	theirs, err := capability.NewSigner(ring)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}

	now := time.Now()
	fileID, user := uuid.New(), uuid.New()
	forged := theirs.Sign(fileID, user, now.Add(capability.TTL))

	if _, err := mine.Verify(fileID, forged, now); err == nil {
		t.Error("a grant signed under a different master key verified")
	}
}

// TestCapabilityKeyIsNotTheTokenKey pins the domain separation. Deriving both
// from the same master key is fine; deriving them with the same info string
// would make a content signature and a session signature interchangeable.
func TestCapabilityKeyIsNotTheTokenKey(t *testing.T) {
	master := make([]byte, skcrypto.KeyLen)
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	capKey, err := ring.Derive(skcrypto.InfoCapability, nil)
	if err != nil {
		t.Fatalf("Derive(capability) = %v", err)
	}
	for _, info := range []string{
		skcrypto.InfoToken, skcrypto.InfoFile, skcrypto.InfoShare, skcrypto.InfoOAuth,
	} {
		other, derr := ring.Derive(info, nil)
		if derr != nil {
			t.Fatalf("Derive(%s) = %v", info, derr)
		}
		if string(capKey) == string(other) {
			t.Errorf("capability key collides with the %s key", info)
		}
	}
}
