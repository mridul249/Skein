package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// A TRUNCATED TRANSFER MUST NOT LOOK LIKE A SMALL SUCCESSFUL ONE.
//
// The status is already committed by the time the copy fails, so the access
// log records `status=200 bytes=65536` — identical in shape to a small file
// served perfectly. The only distinguishing detail was logged at DEBUG, and
// the app runs at info, so it was invisible.
//
// That is exactly what happened on 2026-08-05: a 5,909,666-byte file logged
// `status=200 bytes=65536` after 1.08s (healthy reads of the same file took
// ~6s), and ruling out a range request took a full investigation. It was
// benign — a preview element abandoning an image load — but nothing in the
// log said so, and next time it may not be.
//
// Warn, with bytes written, bytes expected, and the reason.
func TestATruncatedTransferIsLoggedLoudlyEnough(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logTruncatedTransfer(lg, "file-123", 65536, 5909666, errors.New("connection reset by peer"))

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged; a truncated transfer must not be silent")
	}

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, out)
	}

	if lvl, _ := line["level"].(string); lvl != "WARN" {
		t.Errorf("level = %q, want WARN; DEBUG is invisible at the level the app runs at", lvl)
	}
	if got := line["bytes_written"]; got != float64(65536) {
		t.Errorf("bytes_written = %v, want 65536", got)
	}
	if got := line["bytes_expected"]; got != float64(5909666) {
		t.Errorf("bytes_expected = %v, want 5909666; without it a short read cannot be "+
			"told from a small file", got)
	}
	if !strings.Contains(out, "connection reset by peer") {
		t.Errorf("the abort reason is missing from the log line: %s", out)
	}
	if !strings.Contains(out, "file-123") {
		t.Errorf("the file id is missing from the log line: %s", out)
	}
}

// A COMPLETE transfer must stay quiet. Otherwise the Warn is noise and gets
// filtered, which puts us back where we started.
func TestACompleteTransferLogsNothingExtra(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logTruncatedTransfer(lg, "file-123", 5909666, 5909666, nil)

	if buf.Len() != 0 {
		t.Fatalf("a complete transfer logged something: %s", buf.String())
	}
}

// A client that hangs up having received everything is not a truncation: the
// bytes all arrived. Only a SHORT transfer is worth a Warn.
func TestAFullTransferWithALateErrorIsNotTruncation(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logTruncatedTransfer(lg, "file-123", 5909666, 5909666, errors.New("broken pipe"))

	if buf.Len() != 0 {
		t.Fatalf("a fully-delivered transfer was reported as truncated: %s", buf.String())
	}
}

// An unknown expected size (a range read where the total is not carried, or a
// zero-length file) must not produce a bogus "short by everything" warning.
func TestAnUnknownExpectedSizeDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logTruncatedTransfer(lg, "file-123", 0, 0, errors.New("connection reset"))

	if buf.Len() != 0 {
		t.Fatalf("a transfer with no known expected size warned anyway: %s", buf.String())
	}
}
