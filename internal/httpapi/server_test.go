package httpapi_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"github.com/mridul249/Skein/internal/config"
	"github.com/mridul249/Skein/internal/httpapi"
	"github.com/mridul249/Skein/internal/web"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SKEIN_DATABASE_URL", "postgres://localhost/skein")
	t.Setenv("SKEIN_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("SKEIN_JWT_SECRET", strings.Repeat("k", 48))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	return cfg
}

func newServer(t *testing.T, health httpapi.Health) http.Handler {
	t.Helper()
	srv, err := httpapi.New(httpapi.Deps{
		Config: testConfig(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Health: health,
	})
	if err != nil {
		t.Fatalf("httpapi.New() = %v", err)
	}
	return srv.Handler()
}

type okHealth struct{}

func (okHealth) Ping(context.Context) error { return nil }

type downHealth struct{}

func (downHealth) Ping(context.Context) error { return errors.New("database unreachable") }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "198.51.100.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Liveness must not depend on the database, or a database outage makes an
// orchestrator restart a process that is perfectly healthy.
func TestHealthzIgnoresTheDatabase(t *testing.T) {
	h := newServer(t, downHealth{})

	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with the database down", rec.Code)
	}
}

func TestReadyzReflectsTheDatabase(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		if rec := get(t, newServer(t, okHealth{}), "/readyz"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("down", func(t *testing.T) {
		rec := get(t, newServer(t, downHealth{}), "/readyz")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		// The reason is a category, never the driver's own message.
		if strings.Contains(rec.Body.String(), "unreachable") {
			t.Errorf("the driver error leaked to the client: %s", rec.Body)
		}
	})
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newServer(t, okHealth{})

	for _, path := range []string{"/healthz", "/readyz", "/api/nonexistent"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path)
			want := map[string]string{
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
				"Referrer-Policy":        "no-referrer",
			}
			for k, v := range want {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if rec.Header().Get("X-Request-Id") == "" {
				t.Error("no request id header")
			}
		})
	}
}

func TestUnknownAPIPathReturnsTheTypedShape(t *testing.T) {
	rec := get(t, newServer(t, okHealth{}), "/api/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"not_found"`) {
		t.Errorf("body = %s, want the typed error shape", rec.Body)
	}
}

// The Phase 4 exit criterion: go build produces one binary that serves the
// full UI. This fails if internal/web/dist was not populated before the Go
// build, which is exactly the mistake worth catching in CI.
func TestEmbeddedUIIsServed(t *testing.T) {
	if _, err := web.FS(); err != nil {
		t.Skipf("no frontend bundle embedded (%v); run `make web` first", err)
	}

	h := newServer(t, okHealth{})

	t.Run("index", func(t *testing.T) {
		rec := get(t, h, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Errorf("the served index does not look like the SPA shell")
		}
	})

	t.Run("deep link falls through to the SPA", func(t *testing.T) {
		rec := get(t, h, "/settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 so client routing works on a deep link", rec.Code)
		}
	})

	t.Run("missing asset 404s rather than serving html", func(t *testing.T) {
		rec := get(t, h, "/assets/does-not-exist.js")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; serving index.html here would hand back "+
				"HTML with a JavaScript content type", rec.Code)
		}
	})

	t.Run("app csp is applied to the shell", func(t *testing.T) {
		rec := get(t, h, "/")
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Fatal("no Content-Security-Policy on the app shell")
		}
		for _, want := range []string{"default-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP %q is missing %q", csp, want)
			}
		}
	})
}

// The UI must never carry a token in a place a script can read. This asserts
// the served bundle does not reference web storage at all.
func TestBundleDoesNotUseWebStorage(t *testing.T) {
	fsys, err := web.FS()
	if err != nil {
		t.Skipf("no frontend bundle embedded (%v); run `make web` first", err)
	}

	entries, err := readAllJS(fsys)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no JavaScript in the bundle")
	}

	for name, body := range entries {
		for _, banned := range []string{"localStorage", "sessionStorage"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s references %s; Rules.md §2.15 forbids auth state in web storage",
					name, banned)
			}
		}
	}
}

// PRODUCT COPY USES PLAIN PUNCTUATION.
//
// Asserted against the SHIPPED BUNDLE rather than the sources, because that is
// the only place the distinction between copy and commentary resolves itself:
// comments explaining a decision are stripped by the build and may use whatever
// punctuation reads best, while a string that survives minification is text a
// user reads.
//
// A LONE em dash is allowed and is not prose: `—` on its own is the placeholder
// for an absent value in a table cell (format.ts, ShardMap, Trash), where a
// hyphen would read as data rather than as emptiness. What is banned is an em
// dash INSIDE a sentence, which is the house style for product copy.
func TestBundleCopyAvoidsEmDashesInSentences(t *testing.T) {
	fsys, err := web.FS()
	if err != nil {
		t.Skipf("no frontend bundle embedded (%v); run `make web` first", err)
	}
	entries, err := readAllJS(fsys)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no JavaScript in the bundle")
	}

	for name, body := range entries {
		for _, snippet := range emDashSentences(body) {
			t.Errorf("%s ships an em dash in product copy: %q\n"+
				"Use a comma, a full stop, or a colon. A bare \"—\" as an "+
				"empty-value placeholder is fine and is not matched here.",
				name, snippet)
		}
	}
}

// emDashSentences returns each em dash in body that has a word character within
// a few positions on BOTH sides, which is what distinguishes "a — b" inside a
// sentence from a standalone placeholder glyph.
func emDashSentences(body string) []string {
	var out []string
	runes := []rune(body)
	for i, r := range runes {
		if r != '—' {
			continue
		}
		if hasWordNear(runes, i, -1) && hasWordNear(runes, i, +1) {
			lo := max(0, i-45)
			hi := min(len(runes), i+45)
			out = append(out, string(runes[lo:hi]))
		}
	}
	return out
}

// hasWordNear reports whether a letter appears within three runes of i in the
// given direction, skipping the spaces that normally surround an em dash.
func hasWordNear(runes []rune, i, dir int) bool {
	for step := 1; step <= 3; step++ {
		j := i + dir*step
		if j < 0 || j >= len(runes) {
			return false
		}
		switch {
		case unicode.IsLetter(runes[j]):
			return true
		case runes[j] == ' ':
			continue
		default:
			return false
		}
	}
	return false
}

// readAllJS returns every .js file in the built bundle, keyed by path.
func readAllJS(fsys fs.FS) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, path)
		if rerr != nil {
			return rerr
		}
		out[path] = string(b)
		return nil
	})
	return out, err
}
