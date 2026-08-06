package config

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// .env.example IS DOCUMENTATION THAT CAN BE WRONG, and on 2026-08-06 it was:
// following the README quickstart from a clean checkout produced
//
//	skein: configuration: SKEIN_JWT_SECRET must be at least 32 characters
//
// The very first command a new user runs failed. Four more keys existed in
// Config and were absent from the example entirely, including
// SKEIN_BACKUP_TOKEN — which the Recovery UI actively instructs users to set.
//
// Prose cannot be tested, but the KEY LIST can, and that is what drifts. These
// tests read the real file rather than a fixture: a copy would drift from the
// thing being checked, which is the whole failure mode.

func envExamplePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", ".env.example")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("cannot find .env.example at %s: %v", p, err)
	}
	return p
}

// envExampleKeys returns every SKEIN_* key the example ASSIGNS, mapped to its
// value. Commented-out mentions do not count: a key a user must uncomment is
// not a key the file sets.
func envExampleKeys(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(envExamplePath(t))
	if err != nil {
		t.Fatalf("open .env.example: %v", err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	return out
}

// configEnvKeys returns every SKEIN_* key Config reads, and whether the tag
// marks it required.
func configEnvKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	ty := reflect.TypeOf(Config{})
	for i := 0; i < ty.NumField(); i++ {
		tag := ty.Field(i).Tag.Get("env")
		if tag == "" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if !strings.HasPrefix(name, "SKEIN_") {
			continue
		}
		required := false
		for _, p := range parts[1:] {
			if p == "required" {
				required = true
			}
		}
		out[name] = required
	}
	if len(out) == 0 {
		t.Fatal("found no SKEIN_* env tags on Config; this test is not reading what it thinks")
	}
	return out
}

// EVERY key Config reads must appear in the example. A setting a user cannot
// discover is a setting that does not exist to them.
func TestEnvExampleDocumentsEveryConfigKey(t *testing.T) {
	inFile := envExampleKeys(t)
	inCode := configEnvKeys(t)

	var missing []string
	for name := range inCode {
		if _, ok := inFile[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf(".env.example does not mention %d key(s) Config reads: %v\n"+
			"A user cannot set what the example does not name — SKEIN_BACKUP_TOKEN "+
			"was missing while the Recovery UI told users to set it.", len(missing), missing)
	}
}

// readOutsideConfig lists SKEIN_* variables that are real and required but
// deliberately NOT fields on Config.
//
// SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET is read with os.Getenv at its two call
// sites (desktopoauth.Connector, cmd/skein-recover) rather than parsed here,
// because the desktop OAuth config is built per attempt. It is nonetheless
// required for the desktop build — Google demands a secret at token exchange
// even for Desktop-type clients — so it must appear in .env.example.
//
// This list is the exception, and it is written down rather than the check
// being loosened: an unexplained key in the example is how a stale setting
// survives, and Config's own comment claimed for a while that desktop clients
// need no secret, which is false.
var readOutsideConfig = map[string]string{
	"SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET": "internal/desktopoauth/connect.go",
}

// The example must not name keys nothing reads: a stale key is an instruction
// to configure something that does nothing.
func TestEnvExampleHasNoKeysNothingReads(t *testing.T) {
	inFile := envExampleKeys(t)
	inCode := configEnvKeys(t)

	var stale []string
	for name := range inFile {
		if _, ok := inCode[name]; ok {
			continue
		}
		if _, known := readOutsideConfig[name]; known {
			continue
		}
		stale = append(stale, name)
	}
	if len(stale) > 0 {
		t.Errorf(".env.example sets %d key(s) nothing reads: %v", len(stale), stale)
	}
}

// The documented exceptions must stay real. If a key here stops being read
// anywhere, it is stale in the example and this list is now lying too.
func TestKeysReadOutsideConfigAreStillRead(t *testing.T) {
	for name, where := range readOutsideConfig {
		path := filepath.Join("..", "..", where)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: documented as read by %s, which does not exist: %v", name, where, err)
			continue
		}
		if !strings.Contains(string(body), name) {
			t.Errorf("%s: documented as read by %s, but that file no longer mentions it; "+
				"either the exception is stale or the variable moved", name, where)
		}
	}
}

// THE ONE THAT REPRODUCES THE LIVE FAILURE.
//
// A required key shipped empty means `cp .env.example .env` produces a file
// that cannot boot. Either the example carries a working value, or it must
// carry a generation command in the comment immediately above it — so the
// reader knows what to do rather than discovering it from a startup error.
func TestRequiredKeysAreEitherFilledOrCarryAGenerationCommand(t *testing.T) {
	inFile := envExampleKeys(t)
	inCode := configEnvKeys(t)

	body, err := os.ReadFile(envExamplePath(t))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	text := string(body)

	for name, required := range inCode {
		if !required {
			continue
		}
		value, present := inFile[name]
		if !present {
			continue // reported by TestEnvExampleDocumentsEveryConfigKey
		}
		if value != "" {
			continue
		}
		// Empty and required: the comment block above it must tell the reader
		// how to produce a value.
		idx := strings.Index(text, "\n"+name+"=")
		if idx < 0 {
			t.Errorf("%s: could not locate its assignment to inspect the comment above it", name)
			continue
		}
		preceding := text[:idx]
		if !mentionsAGenerator(preceding) {
			t.Errorf("%s is required, ships empty, and the comment above it names no way to "+
				"generate a value. `cp .env.example .env` then produces a file that cannot "+
				"boot — which is exactly what a new user does first.", name)
		}
	}
}

// mentionsAGenerator reports whether the trailing comment block names a
// command that produces a value.
func mentionsAGenerator(preceding string) bool {
	lines := strings.Split(preceding, "\n")
	// Walk back over the contiguous comment block immediately above.
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "#") {
			return false
		}
		if strings.Contains(l, "openssl") || strings.Contains(l, "head -c") {
			return true
		}
	}
	return false
}
