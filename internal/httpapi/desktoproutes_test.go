package httpapi_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE SERVER BINARY MUST NOT CONTAIN THE DESKTOP DOWNLOAD ROUTES.
//
// Verified by inspecting the BUILT BINARY, not by trusting the build tag. The
// tag is the mechanism; this is the check that the mechanism worked. A route
// that writes to an arbitrary path on the machine running the server is a
// file-write primitive on a hosted deployment, so "we set a build tag" is not
// evidence — the binary is.
//
// Skipped under -short: it shells out to the Go toolchain.
func TestServerBinaryHasNoDesktopRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the server binary")
	}

	bin := filepath.Join(t.TempDir(), "skein-server")
	build := exec.Command("go", "build", "-o", bin, "./cmd/skein")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	body := string(raw)

	// Route patterns and handler symbols that must not be present.
	forbidden := []string{
		"/api/desktop/downloads",
		"/api/desktop/capabilities",
		"DesktopDownloads",
		"NewDownloadManager",
	}
	for _, want := range forbidden {
		if strings.Contains(body, want) {
			t.Errorf("the server binary contains %q; the desktop download path "+
				"is compiled into a build that must not have it", want)
		}
	}
}

// And the desktop binary DOES contain them, so the test above cannot pass
// because the strings were renamed or the feature silently dropped.
func TestDesktopBuildContainsTheDownloadRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the desktop package")
	}

	bin := filepath.Join(t.TempDir(), "skein-desktop-probe")
	// Built as a library-linked test binary rather than the real desktop
	// binary: the latter needs cgo and webkit2gtk, which a test machine may
	// not have. The routes live in internal/httpapi either way.
	build := exec.Command("go", "test", "-tags", "desktop", "-c",
		"-o", bin, "./internal/httpapi")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("desktop build unavailable here: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if !strings.Contains(string(raw), "/api/desktop/downloads") {
		t.Error("the desktop build does not contain the download route; " +
			"the server-side assertion would pass vacuously")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}
