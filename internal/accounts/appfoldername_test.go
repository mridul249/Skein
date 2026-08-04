package accounts

import (
	"crypto/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

func testRing(t *testing.T) *skcrypto.Keyring {
	t.Helper()
	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	return ring
}

// Deterministic: the same user must resolve the same folder on every call, or
// each connect would create a new folder.
func TestAppFolderNameIsDeterministic(t *testing.T) {
	ring := testRing(t)
	userID := uuid.New()

	first, err := appFolderName(ring, userID)
	if err != nil {
		t.Fatalf("appFolderName() = %v", err)
	}
	for i := 0; i < 5; i++ {
		again, aerr := appFolderName(ring, userID)
		if aerr != nil {
			t.Fatalf("appFolderName() = %v", aerr)
		}
		if again != first {
			t.Fatalf("call %d returned %q, want the stable %q", i, again, first)
		}
	}

	if !regexp.MustCompile(`^Skein \([0-9a-f]{8}\)$`).MatchString(first) {
		t.Errorf("name = %q, want the form \"Skein (a1b2c3d4)\"", first)
	}
}

// Distinct per user, or the whole fix does nothing.
func TestAppFolderNameDiffersPerUser(t *testing.T) {
	ring := testRing(t)

	seen := map[string]uuid.UUID{}
	for i := 0; i < 200; i++ {
		id := uuid.New()
		name, err := appFolderName(ring, id)
		if err != nil {
			t.Fatalf("appFolderName() = %v", err)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("users %s and %s both derived %q", prev, id, name)
		}
		seen[name] = id
	}
}

// The folder name is displayed in the user's own Drive. It must not carry the
// Skein user id.
func TestAppFolderNameDoesNotLeakTheUserID(t *testing.T) {
	ring := testRing(t)
	userID := uuid.New()

	name, err := appFolderName(ring, userID)
	if err != nil {
		t.Fatalf("appFolderName() = %v", err)
	}
	full := userID.String()
	if strings.Contains(name, full) {
		t.Errorf("name %q contains the user id", name)
	}
	// Nor any substantial run of it.
	for _, part := range strings.Split(full, "-") {
		if len(part) >= 8 && strings.Contains(name, part) {
			t.Errorf("name %q contains user id fragment %q", name, part)
		}
	}
}
