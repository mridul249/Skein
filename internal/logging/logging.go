// Package logging configures the process logger and provides the redaction
// helpers required by Rules.md §6.
package logging

import (
	"context"
	"log/slog"
	"os"
)

// Secret wraps a value that must never appear in a log line. Its String and
// LogValue methods both return a fixed placeholder, so redaction happens by
// construction rather than by remembering to omit the field.
type Secret string

// String implements fmt.Stringer and always returns the redaction placeholder.
func (Secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer so %#v cannot leak the value either.
func (Secret) GoString() string { return `"[redacted]"` }

// LogValue implements slog.LogValuer and always returns the placeholder.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalJSON keeps the value out of any JSON-encoded log payload.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Reveal returns the underlying value. Every call site is a place a secret
// leaves its wrapper, so the name is deliberately conspicuous in review.
func (s Secret) Reveal() string { return string(s) }

// New builds the process logger. level is one of debug, info, warn, error.
func New(level string, jsonOut bool) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redactWellKnownKeys}

	var h slog.Handler
	if jsonOut {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// sensitiveKeys is a backstop for attributes that reach the logger as plain
// strings despite Secret existing. Construction-time redaction is the primary
// defence; this catches the case someone forgot.
var sensitiveKeys = map[string]struct{}{
	"password":          {},
	"password_hash":     {},
	"token":             {},
	"access_token":      {},
	"refresh_token":     {},
	"refresh_hash":      {},
	"share_token":       {},
	"code":              {},
	"client_secret":     {},
	"authorization":     {},
	"cookie":            {},
	"set-cookie":        {},
	"master_key":        {},
	"jwt_secret":        {},
	"encryption_key":    {},
	"state":             {},
	"code_verifier":     {},
	"provider_response": {},
}

func redactWellKnownKeys(_ []string, a slog.Attr) slog.Attr {
	if _, bad := sensitiveKeys[a.Key]; bad {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

type ctxKey struct{}

// WithLogger returns a context carrying lg.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, lg)
}

// FromContext returns the logger stored in ctx, or the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}
