package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const canary = "super-secret-refresh-token"

func TestSecretNeverRendersItsValue(t *testing.T) {
	s := Secret(canary)

	tests := []struct {
		name string
		got  string
	}{
		{"String", s.String()},
		{"fmt %v", fmt.Sprintf("%v", s)},
		{"fmt %#v", fmt.Sprintf("%#v", s)},
		{"fmt %q", fmt.Sprintf("%q", s.String())},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.got, canary) {
				t.Fatalf("rendered secret: %s", tc.got)
			}
			if !strings.Contains(tc.got, "redacted") {
				t.Errorf("got %q, want it to mention redacted", tc.got)
			}
		})
	}

	b, err := json.Marshal(map[string]Secret{"token": s})
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if bytes.Contains(b, []byte(canary)) {
		t.Fatalf("JSON leaked the secret: %s", b)
	}

	if s.Reveal() != canary {
		t.Error("Reveal() did not return the underlying value")
	}
}

func TestSecretIsRedactedInLogOutput(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: redactWellKnownKeys,
	}))

	lg.Info("login", slog.Any("refresh", Secret(canary)))
	if strings.Contains(buf.String(), canary) {
		t.Fatalf("secret reached the log: %s", buf.String())
	}
}

func TestWellKnownKeysAreRedacted(t *testing.T) {
	keys := []string{"password", "token", "access_token", "refresh_token", "code", "authorization", "state"}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			var buf bytes.Buffer
			lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
				ReplaceAttr: redactWellKnownKeys,
			}))
			lg.Info("event", slog.String(k, canary))
			if strings.Contains(buf.String(), canary) {
				t.Fatalf("key %q leaked: %s", k, buf.String())
			}
		})
	}
}

func TestNewRespectsLevel(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "nonsense"} {
		if got := New(level, true); got == nil {
			t.Fatalf("New(%q) = nil", level)
		}
	}
}
