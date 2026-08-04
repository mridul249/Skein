package desktopoauth

import (
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"sync"
)

//go:embed landing.html
var landingHTML string

var landingTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("landing").Parse(landingHTML))
})

// landingModel is everything the page can say. Every field is server-composed
// from the fixed sets below.
//
// NOTHING FROM THE REQUEST REACHES THIS STRUCT. The page renders on
// 127.0.0.1:<port> immediately after the OAuth callback, so the URL that
// produced it carries the authorization code and the state. Echoing any query
// parameter into the HTML would be reflected XSS on a loopback origin — an
// origin that, unlike a normal site, is the desktop app's own callback
// listener. The provider's error string is not echoed either: it is mapped to
// one of the messages below, and an unrecognised value falls back to the
// generic one.
type landingModel struct {
	Heading  string
	Body     string
	Fallback string
}

// The fallback line is always rendered, not injected by script. A blocked
// window.close() must leave a page that tells the user what to do.
const closeFallback = "Authentication complete — you can close this window."

// oauthErrorMessages maps the OAuth 2.0 error codes (RFC 6749 §4.1.2.1) to
// text safe to render. A code outside this set renders the generic message,
// which is what makes "never echo the parameter" enforceable rather than
// aspirational.
var oauthErrorMessages = map[string]string{
	"access_denied":             "You declined the permission request. Nothing was connected.",
	"invalid_request":           "The authorisation request was malformed. Try connecting again from Skein.",
	"unauthorized_client":       "This Skein build is not authorised to make that request.",
	"unsupported_response_type": "The provider rejected the request type. Try connecting again from Skein.",
	"invalid_scope":             "The requested permissions were rejected. Try connecting again from Skein.",
	"server_error":              "Google reported a server error. Try again in a moment.",
	"temporarily_unavailable":   "Google is temporarily unavailable. Try again in a moment.",
}

const genericOAuthError = "Authorisation did not complete. Try connecting again from Skein."

// errorMessageFor maps a provider error code to fixed text.
//
// The lookup is the security boundary: the returned string is always one of
// the constants above, never any part of the input.
func errorMessageFor(code string) string {
	if msg, ok := oauthErrorMessages[strings.TrimSpace(code)]; ok {
		return msg
	}
	return genericOAuthError
}

// writeLanding renders the success or failure page.
func writeLanding(w http.ResponseWriter, status int, m landingModel) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The page is generated per request and contains a one-time outcome;
	// nothing about it should be reused from cache.
	w.Header().Set("Cache-Control", "no-store")
	// Defence in depth: even if something were echoed, nothing external
	// loads and no inline handler runs beyond the close timer.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)

	// Rendered to the response only after a successful execute, so a template
	// failure cannot emit a half-page with a 200 already committed.
	var buf strings.Builder
	if err := landingTmpl().Execute(&buf, m); err != nil {
		_, _ = w.Write([]byte("<!doctype html><title>Skein</title><p>" +
			template.HTMLEscapeString(closeFallback) + "</p>"))
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

// successLanding is shown when the callback carried a code.
func successLanding() landingModel {
	return landingModel{
		Heading:  "Drive connected",
		Body:     "Skein has what it needs. You can return to the app.",
		Fallback: closeFallback,
	}
}

// failureLanding is shown when the provider reported an error. code is a
// provider error code and is used only as a map key.
func failureLanding(code string) landingModel {
	return landingModel{
		Heading:  "Could not connect",
		Body:     errorMessageFor(code),
		Fallback: "You can close this window and try again from Skein.",
	}
}

// duplicateLanding is shown when a callback arrives after the first was
// already recorded — a duplicate tab or a retried redirect.
func duplicateLanding() landingModel {
	return landingModel{
		Heading:  "Already connected",
		Body:     "This authorisation was already received.",
		Fallback: closeFallback,
	}
}
