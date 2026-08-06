package desktopoauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// callbackResponse drives one request at a fresh listener's handler.
func callbackResponse(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	l := &LoopbackListener{resultCh: make(chan LoopbackResult, 1)}
	req := httptest.NewRequest(http.MethodGet, "/callback?"+query, nil)
	rec := httptest.NewRecorder()
	l.handleCallback(rec, req)
	return rec
}

func TestLandingPageSuccess(t *testing.T) {
	rec := callbackResponse(t, "code=4/real-authorization-code&state=abc123")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Drive connected") {
		t.Error("success page does not say the drive connected")
	}
	// The fallback is always in the markup, so a blocked window.close() still
	// leaves the user told what to do.
	if !strings.Contains(body, closeFallback) {
		t.Errorf("success page is missing the fallback copy %q", closeFallback)
	}
	if !strings.Contains(body, "window.close()") {
		t.Error("success page does not attempt to close itself")
	}
}

// THE SECURITY ASSERTION. The callback URL carries the authorization code and
// the state. Neither may appear anywhere in the rendered page — not in the
// body, not in a comment, not in the inline script.
func TestLandingPageNeverEchoesTheCodeOrState(t *testing.T) {
	const (
		code  = "4-this-is-the-authorization-code"
		state = "this-is-the-opaque-state-value"
	)
	rec := callbackResponse(t, "code="+code+"&state="+state+"&client_secret=SUPERSECRET")

	body := rec.Body.String()
	for _, secret := range []string{code, state, "SUPERSECRET"} {
		if strings.Contains(body, secret) {
			t.Errorf("the rendered page contains %q:\n%s", secret, body)
		}
	}
	// The whole response, headers included — a Location or Set-Cookie would
	// leak just as effectively.
	for k, vals := range rec.Header() {
		for _, v := range vals {
			for _, secret := range []string{code, state, "SUPERSECRET"} {
				if strings.Contains(v, secret) {
					t.Errorf("header %s leaked %q", k, secret)
				}
			}
		}
	}
}

// Failure text is server-composed from a fixed set. A hostile error parameter
// is a map key and nothing else.
func TestLandingPageFailureNeverEchoesTheErrorParameter(t *testing.T) {
	payloads := []string{
		`<script>alert(1)</script>`,
		`"><img src=x onerror=alert(1)>`,
		`javascript:alert(1)`,
		`{{.Heading}}`,
		`access_denied<script>`,
	}
	for _, p := range payloads {
		t.Run(p, func(t *testing.T) {
			rec := callbackResponse(t, "error="+url.QueryEscape(p))
			body := rec.Body.String()

			if strings.Contains(body, p) {
				t.Errorf("the raw error parameter was echoed into the page:\n%s", body)
			}
			// No fragment of an injection survives either.
			for _, frag := range []string{"<script>alert", "onerror=", "javascript:"} {
				if strings.Contains(body, frag) {
					t.Errorf("page contains %q after error=%q", frag, p)
				}
			}
			if !strings.Contains(body, "Could not connect") {
				t.Error("failure page does not present as a failure")
			}
			// An unrecognised code falls back to the generic message.
			if !strings.Contains(body, genericOAuthError) {
				t.Errorf("unrecognised error code did not use the generic message:\n%s", body)
			}
		})
	}
}

// A known error code shows its specific, fixed message — the page is useful,
// not merely safe.
func TestLandingPageShowsKnownErrorsSpecifically(t *testing.T) {
	rec := callbackResponse(t, "error=access_denied")
	body := rec.Body.String()

	if !strings.Contains(body, oauthErrorMessages["access_denied"]) {
		t.Errorf("access_denied did not render its specific message:\n%s", body)
	}
	if strings.Contains(body, genericOAuthError) {
		t.Error("a known error code fell back to the generic message")
	}
}

// The page is self-contained: no CDN, no external stylesheet, no remote font.
// A loopback page that reaches out to the network on a callback would leak the
// fact and timing of a connection to a third party.
func TestLandingPageMakesNoExternalRequests(t *testing.T) {
	rec := callbackResponse(t, "code=abc&state=def")
	body := rec.Body.String()

	for _, forbidden := range []string{
		"http://", "https://", "//cdn", "<link", "@import", "fetch(", "XMLHttpRequest",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("landing page references %q; it must be fully self-contained:\n%s",
				forbidden, body)
		}
	}
}

// A duplicate callback is distinguishable from the first, and still safe.
func TestLandingPageDuplicateCallback(t *testing.T) {
	l := &LoopbackListener{resultCh: make(chan LoopbackResult, 1)}

	first := httptest.NewRecorder()
	l.handleCallback(first, httptest.NewRequest(http.MethodGet, "/callback?code=a&state=b", nil))
	if !strings.Contains(first.Body.String(), "Drive connected") {
		t.Fatal("first callback was not treated as a success")
	}

	second := httptest.NewRecorder()
	l.handleCallback(second, httptest.NewRequest(http.MethodGet, "/callback?code=a&state=b", nil))
	body := second.Body.String()
	if !strings.Contains(body, "Already connected") {
		t.Errorf("duplicate callback did not say so:\n%s", body)
	}
	if strings.Contains(body, "code=a") || strings.Contains(body, "state=b") {
		t.Error("the duplicate page echoed callback parameters")
	}
}

// errorMessageFor is the boundary itself: its output is always one of the
// fixed constants, whatever goes in.
func TestErrorMessageForReturnsOnlyFixedStrings(t *testing.T) {
	allowed := map[string]bool{genericOAuthError: true}
	for _, v := range oauthErrorMessages {
		allowed[v] = true
	}

	for _, in := range []string{
		"", "access_denied", "unknown_code", "<script>", strings.Repeat("a", 5000),
		"server_error ", " access_denied",
	} {
		if got := errorMessageFor(in); !allowed[got] {
			t.Errorf("errorMessageFor(%q) = %q, which is not one of the fixed messages", in, got)
		}
	}
}

// THE ONE FIELD THAT IS NOT ESCAPED TEXT.
//
// Styling the page added Tint, which interpolates into an SVG stroke
// attribute. Every other field lands in element content, where html/template
// escapes anything hostile into inert text; an attribute is the one context on
// this page where a crafted value could do more than look wrong.
//
// So it is a constant chosen by the constructor, never computed and never
// derived from the request. This asserts that for every path the page has,
// including the error path whose CODE comes from the provider — the only input
// that reaches these functions at all.
func TestLandingTintIsAlwaysAFixedColour(t *testing.T) {
	allowed := map[string]bool{tintSuccess: true, tintWarning: true, tintNeutral: true}

	models := []landingModel{
		successLanding(),
		duplicateLanding(),
		failureLanding("access_denied"),
		failureLanding("server_error"),
		// Hostile codes: an unknown code must not influence the tint any more
		// than it influences the body text.
		failureLanding(`" onload="alert(1)`),
		failureLanding("</svg><script>alert(1)</script>"),
		failureLanding(""),
	}
	for i, m := range models {
		if !allowed[m.Tint] {
			t.Errorf("model %d has tint %q, which is not one of the three constants; "+
				"this value is interpolated into an SVG attribute", i, m.Tint)
		}
	}
}

// And the rendered page must not carry a hostile code into the markup, tint or
// otherwise. The existing echo tests cover the body; this covers the attribute
// context the mark introduced.
func TestLandingPageAttributesSurviveAHostileErrorCode(t *testing.T) {
	rec := callbackResponse(t, `error=%22+onload%3D%22alert(1)`)
	body := rec.Body.String()

	for _, forbidden := range []string{"onload=", "alert(1)", "</svg><script"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("landing page emitted %q from a provider error code:\n%s", forbidden, body)
		}
	}
	// The mark still renders, with one of the fixed tints.
	if !strings.Contains(body, tintWarning) {
		t.Errorf("the failure page did not render its mark with the warning tint:\n%s", body)
	}
}
