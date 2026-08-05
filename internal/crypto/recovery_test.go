package crypto_test

import (
	"errors"
	"strings"
	"testing"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

// RECOVERY MUST FAIL ON THE KEY, NOT ON THE DATA.
//
// The failure mode being guarded against is a user concluding their files are
// corrupt when in fact they restored the wrong key file. Those two situations
// are indistinguishable from the symptoms alone: both produce failures on
// every shard.
//
// The envelope already stores the key id in the clear and checks it before
// attempting the open (envelope.go:80), which is what makes the diagnosis
// possible at all. This pins that the diagnosis SURVIVES to the caller as a
// distinct condition, rather than being flattened into a generic decrypt
// failure by anything in between.
func TestAWrongKeyIsDiagnosedAsAWrongKeyNotAsCorruption(t *testing.T) {
	original, err := skcrypto.NewKeyring(testMaster(0xA1))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	wrong, err := skcrypto.NewKeyring(testMaster(0xB2))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	salt := []byte("file-id")
	sealed, err := original.Seal(skcrypto.InfoFile, salt, []byte("the plaintext"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	_, err = wrong.Open(skcrypto.InfoFile, salt, sealed)
	if err == nil {
		t.Fatal("the wrong key opened the ciphertext")
	}
	if !errors.Is(err, skcrypto.ErrWrongKey) {
		t.Fatalf("error = %v, want ErrWrongKey; a generic decrypt failure sends the "+
			"user hunting for data corruption they do not have", err)
	}
	// It must not read as corruption.
	if strings.Contains(strings.ToLower(err.Error()), "corrupt") {
		t.Errorf("error %q describes corruption; the data is fine", err)
	}
}

// The right key still opens it, so the test above cannot pass by everything
// being refused.
func TestTheRightKeyStillOpens(t *testing.T) {
	ring, err := skcrypto.NewKeyring(testMaster(0xC3))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	salt := []byte("file-id")
	sealed, err := ring.Seal(skcrypto.InfoFile, salt, []byte("the plaintext"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	got, err := ring.Open(skcrypto.InfoFile, salt, sealed)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if string(got) != "the plaintext" {
		t.Fatalf("round trip = %q", got)
	}
}

// THE END-TO-END RECOVERY STORY, as a user performs it.
//
// Export the key, lose the instance, rebuild elsewhere from the file, and read
// data written by the original. This is the whole point of the export, and
// nothing else in the suite exercises it as one sequence.
func TestExportedKeyRecoversDataWrittenByTheOriginalInstance(t *testing.T) {
	original, err := skcrypto.NewKeyring(testMaster(0xD4))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	salt := []byte("file-id")
	sealed, err := original.Seal(skcrypto.InfoFile, salt, []byte("survives the rebuild"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	// The user downloads the key file, keeps it, and the instance is lost.
	file := skcrypto.ExportKeyFile(original)

	// Months later, on a new machine: verify BEFORE anything else.
	if verr := skcrypto.VerifyKeyFileMatches(file, original.KeyID()); verr != nil {
		t.Fatalf("the instance's own key file failed verification: %v", verr)
	}
	recoveredKey, err := skcrypto.ParseKeyFile(file)
	if err != nil {
		t.Fatalf("ParseKeyFile() = %v", err)
	}
	rebuilt, err := skcrypto.NewKeyring(recoveredKey)
	if err != nil {
		t.Fatalf("NewKeyring(recovered) = %v", err)
	}

	if rebuilt.KeyIDString() != original.KeyIDString() {
		t.Fatalf("rebuilt key id %s != original %s",
			rebuilt.KeyIDString(), original.KeyIDString())
	}
	got, err := rebuilt.Open(skcrypto.InfoFile, salt, sealed)
	if err != nil {
		t.Fatalf("the rebuilt instance cannot read the original's data: %v", err)
	}
	if string(got) != "survives the rebuild" {
		t.Fatalf("recovered plaintext = %q", got)
	}
}
