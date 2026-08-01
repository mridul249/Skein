package main

import "testing"

// The desktop OAuth credentials are a pair: Google's token endpoint checks
// the secret against the client id, so an id from one client with the secret
// of another fails at exchange with an opaque invalid_client. The compiled-in
// defaults make that easy to hit by accident — someone overriding only
// SKEIN_GOOGLE_DESKTOP_CLIENT_ID would otherwise be silently paired with the
// shipped secret of Skein's own client.
func TestResolveDesktopCredentials(t *testing.T) {
	const (
		builtInID     = "built-in-id"
		builtInSecret = "built-in-secret"
		ownID         = "my-own-id"
		ownSecret     = "my-own-secret"
	)

	tests := []struct {
		name       string
		envID      string
		envSecret  string
		wantID     string
		wantSecret string
	}{{
		name:       "no override uses the compiled-in pair",
		wantID:     builtInID,
		wantSecret: builtInSecret,
	}, {
		name:       "both overridden uses the caller's pair",
		envID:      ownID,
		envSecret:  ownSecret,
		wantID:     ownID,
		wantSecret: ownSecret,
	}, {
		// The case this guard exists for. Returning the built-in secret here
		// would pair credentials from two different Google clients.
		name:       "id only yields no secret, never the built-in one",
		envID:      ownID,
		wantID:     ownID,
		wantSecret: "",
	}, {
		// The mirror case is harmless: the secret is overridden but the id
		// still identifies the built-in client, and Connect reports the
		// mismatch through Google rather than guessing. Recorded so a change
		// in this direction is a deliberate one.
		name:       "secret only keeps the built-in id",
		envSecret:  ownSecret,
		wantID:     builtInID,
		wantSecret: ownSecret,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// These are package-level build-stamp vars, so restore them
			// rather than leaking test values into later tests.
			origID, origSecret := desktopClientID, desktopClientSecret
			t.Cleanup(func() { desktopClientID, desktopClientSecret = origID, origSecret })
			desktopClientID = builtInID
			desktopClientSecret = builtInSecret
			t.Setenv("SKEIN_GOOGLE_DESKTOP_CLIENT_ID", tt.envID)
			t.Setenv("SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET", tt.envSecret)

			if got := resolveDesktopClientID(); got != tt.wantID {
				t.Errorf("resolveDesktopClientID() = %q, want %q", got, tt.wantID)
			}
			if got := resolveDesktopClientSecret(); got != tt.wantSecret {
				t.Errorf("resolveDesktopClientSecret() = %q, want %q", got, tt.wantSecret)
			}
		})
	}
}
