package crypto_test

import (
	"errors"
	"strings"
	"testing"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
)

func testMaster(b byte) []byte {
	m := make([]byte, skcrypto.KeyLen)
	for i := range m {
		m[i] = b ^ byte(i*7)
	}
	return m
}

// THE FILE MUST EXPLAIN ITSELF TO SOMEONE WHO HAS FORGOTTEN WHAT IT IS.
//
// Recovery happens months later, on a new machine, under stress. A bare key
// blob is a file people delete during a cleanup because they do not recognise
// it — and deleting it makes every shard in the instance permanently
// unreadable, because the master key is the only thing that decrypts them.
//
// So the file says, in plain text a human reads without any tool: what it is,
// which instance it belongs to, when it was made, and that possession alone is
// sufficient to decrypt everything.
func TestKeyFileIsSelfDescribing(t *testing.T) {
	ring, err := skcrypto.NewKeyring(testMaster(0x11))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	out := string(skcrypto.ExportKeyFile(ring))

	for _, want := range []string{
		"SKEIN MASTER KEY",
		ring.KeyIDString(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the key file does not mention %q:\n%s", want, out)
		}
	}

	// The warning has to be unmissable and unambiguous about what possession
	// means. Checked as a property, not as an exact sentence, so the wording
	// can be improved without breaking the test.
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "decrypt") {
		t.Error("the file never says that holding it decrypts the data")
	}
	if !strings.Contains(lower, "every file") && !strings.Contains(lower, "all files") {
		t.Error("the warning does not convey that it covers the WHOLE instance")
	}
	// An export date, so a reader can tell one file from another after a
	// rotation.
	if !strings.Contains(out, "Exported:") {
		t.Error("the file carries no export date")
	}
}

// The key round-trips exactly. A recovery that silently returns a different
// key is worse than one that fails.
func TestKeyFileRoundTrips(t *testing.T) {
	master := testMaster(0x22)
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	got, err := skcrypto.ParseKeyFile(skcrypto.ExportKeyFile(ring))
	if err != nil {
		t.Fatalf("ParseKeyFile() = %v", err)
	}
	if len(got) != skcrypto.KeyLen {
		t.Fatalf("recovered key is %d bytes, want %d", len(got), skcrypto.KeyLen)
	}
	for i := range master {
		if got[i] != master[i] {
			t.Fatalf("the recovered key differs from the original at byte %d", i)
		}
	}
}

// A WRONG KEY FILE MUST FAIL LOUDLY, BEFORE ANYTHING IS TOUCHED.
//
// The failure being guarded against is someone concluding their data is
// corrupt when in fact they supplied the wrong file. A mismatched key produces
// AEAD failures three layers down that read exactly like corruption — so the
// key id is checked FIRST, and the error says which of the two it is.
func TestRecoveryRefusesAKeyFromAnotherInstance(t *testing.T) {
	mine, err := skcrypto.NewKeyring(testMaster(0x33))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	theirs, err := skcrypto.NewKeyring(testMaster(0x44))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	if mine.KeyIDString() == theirs.KeyIDString() {
		t.Fatal("the two fixtures share a key id; the test would be vacuous")
	}

	// Someone restores the wrong instance's file.
	err = skcrypto.VerifyKeyFileMatches(skcrypto.ExportKeyFile(theirs), mine.KeyID())
	if err == nil {
		t.Fatal("a key file from a DIFFERENT instance was accepted; recovery would " +
			"produce garbage that reads as corruption")
	}
	if !errors.Is(err, skcrypto.ErrKeyIDMismatch) {
		t.Fatalf("error = %v, want ErrKeyIDMismatch", err)
	}

	// The message must name the real problem. "decryption failed" would send
	// the user hunting for corrupt data they do not have.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "different") || !strings.Contains(msg, "instance") {
		t.Errorf("error %q does not say the key belongs to a different instance", err)
	}
	if strings.Contains(msg, "corrupt") || strings.Contains(msg, "decrypt") {
		t.Errorf("error %q suggests corruption; that is the wrong diagnosis", err)
	}
}

// The matching key is accepted, so the test above cannot pass by everything
// being refused.
func TestRecoveryAcceptsTheMatchingKey(t *testing.T) {
	ring, err := skcrypto.NewKeyring(testMaster(0x55))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	if verr := skcrypto.VerifyKeyFileMatches(skcrypto.ExportKeyFile(ring), ring.KeyID()); verr != nil {
		t.Fatalf("the instance's OWN key file was refused: %v", verr)
	}
}

// Damage must be refused rather than silently producing a short or wrong key.
func TestParseKeyFileRejectsDamage(t *testing.T) {
	ring, err := skcrypto.NewKeyring(testMaster(0x66))
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}
	good := string(skcrypto.ExportKeyFile(ring))

	cases := map[string]string{
		"empty":             "",
		"prose only":        "SKEIN MASTER KEY\nnothing else here\n",
		"truncated key":     firstN(good, 20),
		"not base64":        strings.Replace(good, "Key:", "Key: !!!not-base64!!! ", 1),
		"key id but no key": "SKEIN MASTER KEY\nKey ID: " + ring.KeyIDString() + "\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, perr := skcrypto.ParseKeyFile([]byte(in)); perr == nil {
				t.Fatal("damaged input was accepted")
			}
		})
	}
}

// firstN returns the first n bytes, or everything if there are fewer. Written
// out rather than sliced inline so the table cannot panic while the
// implementation is still a stub.
func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// A file whose key does not derive to its own stated key id is corrupt or
// hand-edited, and must not be used.
func TestParseKeyFileRejectsAnInconsistentFile(t *testing.T) {
	a, _ := skcrypto.NewKeyring(testMaster(0x77))
	b, _ := skcrypto.NewKeyring(testMaster(0x88))

	// Take a's file and swap in b's key id.
	tampered := strings.Replace(string(skcrypto.ExportKeyFile(a)),
		"Key ID: "+a.KeyIDString(), "Key ID: "+b.KeyIDString(), 1)

	if _, err := skcrypto.ParseKeyFile([]byte(tampered)); err == nil {
		t.Fatal("a file whose key does not match its own key id was accepted")
	}
}
