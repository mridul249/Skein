package handlers

import (
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/storage"
)

// Rules.md §2.3 and the reference project's stored-XSS bug: a Content-Type
// that came from the client must never reach a response header, and only an
// explicit allowlist is ever rendered inline.
func TestDispositionAllowlist(t *testing.T) {
	tests := []struct {
		name        string
		sniffed     string
		wantType    string
		wantInline  bool
		description string
	}{
		{"png", "image/png", "image/png", true, ""},
		{"jpeg with params", "image/jpeg; charset=binary", "image/jpeg", true, ""},
		{"mp4", "video/mp4", "video/mp4", true, ""},
		{"pdf", "application/pdf", "application/pdf", true, ""},
		{"plain text gets a charset", "text/plain; charset=utf-8", "text/plain; charset=utf-8", true, ""},

		// The ones that matter. Each of these is a script execution
		// context; serving any of them inline from the app origin is
		// stored XSS with the victim's own session attached.
		{"html", "text/html", "application/octet-stream", false, "script execution context"},
		{"html with charset", "text/html; charset=utf-8", "application/octet-stream", false, ""},
		{"svg", "image/svg+xml", "application/octet-stream", false, "svg can carry script"},
		{"xhtml", "application/xhtml+xml", "application/octet-stream", false, ""},
		{"xml", "text/xml", "application/octet-stream", false, ""},
		{"javascript", "text/javascript", "application/octet-stream", false, ""},
		{"json", "application/json", "application/octet-stream", false, ""},
		{"wasm", "application/wasm", "application/octet-stream", false, ""},
		{"flash", "application/x-shockwave-flash", "application/octet-stream", false, ""},

		{"unknown", "application/octet-stream", "application/octet-stream", false, ""},
		{"garbage", "not a media type at all", "application/octet-stream", false, ""},
		{"empty", "", "application/octet-stream", false, ""},
		{"uppercase is normalised", "IMAGE/PNG", "image/png", true, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotInline := disposition(tc.sniffed)
			if gotType != tc.wantType {
				t.Errorf("type = %q, want %q", gotType, tc.wantType)
			}
			if gotInline != tc.wantInline {
				t.Errorf("inline = %v, want %v (%s)", gotInline, tc.wantInline, tc.description)
			}
		})
	}
}

func TestInlineAllowlistExcludesEveryScriptContext(t *testing.T) {
	forbidden := []string{
		"text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml", "text/javascript",
		"application/javascript", "application/wasm",
	}
	for _, ct := range forbidden {
		if inlineAllowlist[ct] {
			t.Errorf("%q is on the inline allowlist; it must never be", ct)
		}
	}
	// Nothing ending in +xml may creep in later either.
	for ct := range inlineAllowlist {
		if strings.HasSuffix(ct, "+xml") || strings.Contains(ct, "html") ||
			strings.Contains(ct, "javascript") || strings.Contains(ct, "script") {
			t.Errorf("%q was added to the inline allowlist and must not be", ct)
		}
	}
}

func TestContentDispositionHeaderIsInjectionSafe(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		reject   []string
	}{
		{"plain", "report.pdf", nil},
		{"quotes", `evil".pdf`, []string{`evil".pdf`}},
		{"backslash", `evil\".pdf`, []string{`\"`}},
		{"newline injection", "a\r\nX-Evil: 1", []string{"\r", "\n"}},
		{"unicode", "résumé.pdf", nil},
		{"only control characters", "\x00\x01\x02", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := contentDispositionHeader("attachment", tc.filename)

			// A header value must never contain a line break, or it
			// becomes header injection.
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("header contains a line break: %q", got)
			}
			for _, bad := range tc.reject {
				if strings.Contains(got, bad) {
					t.Errorf("header contains %q: %q", bad, got)
				}
			}
			if !strings.HasPrefix(got, "attachment; filename=") {
				t.Errorf("header = %q, want it to start with the disposition", got)
			}
			if !strings.Contains(got, "filename*=UTF-8''") {
				t.Errorf("header = %q, want an RFC 6266 filename*", got)
			}
		})
	}
}

// Rules.md §2.6: a malformed Range header from the wire is an error return,
// never a panic.
func TestParseRange(t *testing.T) {
	const size = 1000

	tests := []struct {
		name      string
		header    string
		wantNil   bool
		wantErr   bool
		wantStart int64
		wantLen   int64
	}{
		{name: "absent", header: "", wantNil: true},
		{name: "first 500", header: "bytes=0-499", wantStart: 0, wantLen: 500},
		{name: "second 500", header: "bytes=500-999", wantStart: 500, wantLen: 500},
		{name: "open ended", header: "bytes=500-", wantStart: 500, wantLen: 500},
		{name: "suffix", header: "bytes=-200", wantStart: 800, wantLen: 200},
		{name: "suffix larger than the file", header: "bytes=-5000", wantStart: 0, wantLen: 1000},
		{name: "end past the file is clamped", header: "bytes=900-99999", wantStart: 900, wantLen: 100},
		{name: "single byte", header: "bytes=0-0", wantStart: 0, wantLen: 1},
		{name: "last byte", header: "bytes=999-999", wantStart: 999, wantLen: 1},
		{name: "whitespace tolerated", header: "bytes= 100 - 199 ", wantStart: 100, wantLen: 100},

		{name: "start past the end", header: "bytes=1000-1500", wantErr: true},
		{name: "end before start", header: "bytes=500-100", wantErr: true},
		{name: "negative start", header: "bytes=-", wantErr: true},
		{name: "wrong unit", header: "items=0-10", wantErr: true},
		{name: "no dash", header: "bytes=100", wantErr: true},
		{name: "garbage", header: "bytes=abc-def", wantErr: true},
		{name: "float", header: "bytes=1.5-2.5", wantErr: true},
		{name: "huge number", header: "bytes=99999999999999999999-", wantErr: true},
		{name: "multi-range refused", header: "bytes=0-99,200-299", wantErr: true},
		{name: "zero suffix", header: "bytes=-0", wantErr: true},
		{name: "empty spec", header: "bytes=", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRange(tc.header, size)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRange(%q) = %+v, want an error", tc.header, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRange(%q) = %v", tc.header, err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("parseRange(%q) = %+v, want nil", tc.header, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseRange(%q) = nil, want a range", tc.header)
			}
			if got.Start != tc.wantStart || got.Length != tc.wantLen {
				t.Errorf("parseRange(%q) = %+v, want start %d length %d",
					tc.header, got, tc.wantStart, tc.wantLen)
			}
		})
	}
}

// The exit criterion, at the header level: a request for 1000-2000 asks for
// exactly 1001 bytes.
func TestParseRangeIsInclusive(t *testing.T) {
	got, err := parseRange("bytes=1000-2000", 8192)
	if err != nil {
		t.Fatalf("parseRange() = %v", err)
	}
	if got.Length != 1001 {
		t.Errorf("length = %d, want 1001 (the wire range is inclusive)", got.Length)
	}
}

func TestParseRangeOnAnEmptyFile(t *testing.T) {
	for _, h := range []string{"bytes=0-", "bytes=0-0", "bytes=-1"} {
		if _, err := parseRange(h, 0); err == nil {
			t.Errorf("parseRange(%q, 0) succeeded; nothing is satisfiable on an empty file", h)
		}
	}
}

func TestParseRangeNeverPanics(t *testing.T) {
	inputs := []string{
		"bytes=" + strings.Repeat("9", 400),
		"bytes=-" + strings.Repeat("9", 400),
		"bytes=" + strings.Repeat("-", 100),
		"bytes=,,,,",
		"\x00\x00",
		strings.Repeat("bytes=", 50),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("parseRange(%q) panicked: %v", in, rec)
				}
			}()
			var out *storage.ByteRange
			out, _ = parseRange(in, 1000)
			_ = out
		}()
	}
}
