//go:build desktop

package files_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/files"
)

// THE REQUESTED DIRECTORY IS AN ARBITRARY-WRITE PRIMITIVE UNLESS IT IS BOUNDED.
//
// `dir` on POST /api/desktop/downloads is caller-supplied and was validated
// only as "exists, is a directory, is writable" — so any authenticated caller
// could name any writable path on the machine and have the server write a file
// of their choosing into it. ResolveTarget hardened the FILENAME meticulously
// (filepath.Base, symlink resolution, a containment check) while accepting the
// DIRECTORY as given: careful work on one half of a pair, the same shape as
// #41.
//
// The build tag is not the boundary. The desktop binary listens on loopback,
// registration is open, and a browser page can reach 127.0.0.1 — so anything
// running locally can register and reach this route. It is reachable.
//
// The rule: a requested directory must resolve INSIDE the configured root,
// after symlink resolution. Absent means the root itself.
func TestResolveTargetConfinesRequestedDirToTheRoot(t *testing.T) {
	root := t.TempDir()

	// A legitimate subdirectory, which must keep working.
	sub := filepath.Join(root, "invoices")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	outside := t.TempDir() // a different root entirely

	t.Run("an absent directory means the root", func(t *testing.T) {
		got, err := files.ResolveTarget(root, "", "report.pdf")
		if err != nil {
			t.Fatalf("ResolveTarget() = %v", err)
		}
		if filepath.Dir(got) != root {
			t.Fatalf("target = %q, want it directly in %q", got, root)
		}
	})

	t.Run("a subdirectory of the root is allowed", func(t *testing.T) {
		got, err := files.ResolveTarget(root, sub, "report.pdf")
		if err != nil {
			t.Fatalf("ResolveTarget(sub) = %v", err)
		}
		if filepath.Dir(got) != sub {
			t.Fatalf("target = %q, want it in %q", got, sub)
		}
	})

	t.Run("a relative subdirectory is allowed and stays inside", func(t *testing.T) {
		got, err := files.ResolveTarget(root, "invoices", "report.pdf")
		if err != nil {
			t.Fatalf("ResolveTarget(relative) = %v", err)
		}
		if filepath.Dir(got) != sub {
			t.Fatalf("target = %q, want it in %q", got, sub)
		}
	})

	// The traversal payloads already used for the FILENAME, now aimed at the
	// DIRECTORY — which is where they were never tested.
	escapes := []string{
		"../../etc",
		"/etc",
		"../escape",
		"..",
		"invoices/../../bar",
		outside,
		filepath.Join(root, "..", filepath.Base(outside)),
		"/tmp",
		"/",
	}
	for _, dir := range escapes {
		t.Run("rejects "+dir, func(t *testing.T) {
			got, err := files.ResolveTarget(root, dir, "payload.sh")
			if err != nil {
				return // refusing outright is the expected outcome
			}
			if !within(t, root, got) {
				t.Fatalf("ResolveTarget(dir=%q) = %q, which is OUTSIDE the configured "+
					"root %q: this is an arbitrary write", dir, got, root)
			}
		})
	}
}

// THE CASE FILENAME HARDENING NEVER HAD TO HANDLE.
//
// filepath.Base cannot be fooled by a symlink, because it never touches the
// filesystem. A DIRECTORY can: a symlink inside the root pointing out of it
// passes every lexical check and every os.Stat — it exists, it is a directory,
// it is writable — while resolving somewhere else entirely.
func TestResolveTargetRejectsASymlinkOutOfTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Sanity: the trap really is a usable directory, so a rejection is the
	// containment check working rather than the path being broken.
	if info, err := os.Stat(link); err != nil || !info.IsDir() {
		t.Fatalf("symlink is not a usable directory: %v", err)
	}

	got, err := files.ResolveTarget(root, link, "payload.sh")
	if err != nil {
		return // refusing is correct
	}
	if !within(t, root, got) {
		t.Fatalf("ResolveTarget followed a symlink out of the root: %q resolves "+
			"outside %q (the link pointed at %q)", got, root, outside)
	}
}

// A symlinked ROOT is legitimate — /home/x/Downloads may itself be a link —
// so confinement must compare resolved forms rather than refusing outright.
func TestResolveTargetAllowsASymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	real := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "downloads")
	if err := os.Symlink(real, linkRoot); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := files.ResolveTarget(linkRoot, "", "report.pdf")
	if err != nil {
		t.Fatalf("a symlinked root was refused: %v", err)
	}
	if !within(t, linkRoot, got) {
		t.Fatalf("target %q is not inside the symlinked root %q", got, linkRoot)
	}
}

// within reports whether path is inside root, comparing fully resolved forms
// so a symlinked root or tmpdir does not produce a false negative.
func within(t *testing.T, root, path string) bool {
	t.Helper()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	// The file itself does not exist yet; resolve its parent.
	realDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		realDir = filepath.Dir(path)
	}
	realRoot = filepath.Clean(realRoot)
	realDir = filepath.Clean(realDir)
	return realDir == realRoot || strings.HasPrefix(realDir, realRoot+string(filepath.Separator))
}
