package desktopoauth

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// The listener must release its port on every path — success, a client
// error redirect, a timeout, and context cancellation — not only the happy
// path. A listener left open after a failed attempt is a port held for no
// reason and, over repeated failed connect attempts, a slow leak.
func TestLoopbackListenerAlwaysReleasesThePort(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, l *LoopbackListener)
	}{
		{
			name: "success",
			run: func(t *testing.T, l *LoopbackListener) {
				go func() {
					_, _ = http.Get("http://" + l.Addr() + "/callback?state=s&code=c")
				}()
				if _, err := l.Await(context.Background()); err != nil {
					t.Fatalf("Await() = %v", err)
				}
			},
		},
		{
			name: "provider error redirect",
			run: func(t *testing.T, l *LoopbackListener) {
				go func() {
					_, _ = http.Get("http://" + l.Addr() + "/callback?error=access_denied")
				}()
				result, err := l.Await(context.Background())
				if err != nil {
					t.Fatalf("Await() = %v", err)
				}
				if result.Err != "access_denied" || result.Code != "" {
					t.Fatalf("result = %+v, want Err=access_denied and empty Code", result)
				}
			},
		},
		{
			name: "context cancelled before any callback",
			run: func(t *testing.T, l *LoopbackListener) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := l.Await(ctx); err == nil {
					t.Fatal("Await() succeeded on an already-cancelled context")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := OpenLoopbackListener()
			if err != nil {
				t.Fatalf("OpenLoopbackListener() = %v", err)
			}
			addr := l.Addr()

			tc.run(t, l)

			// Await already called Close internally; a listener still
			// bound to addr means the port was not actually released.
			ln2, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("port %s still held after Await: %v", addr, err)
			}
			_ = ln2.Close()
		})
	}
}

// Close must be safe to call more than once. A caller that fails between
// Open and Await (e.g. the browser could not be launched) calls Close
// directly; Await also calls Close internally, so any path that does both
// must not deadlock or panic.
func TestLoopbackListenerCloseIsIdempotent(t *testing.T) {
	l, err := OpenLoopbackListener()
	if err != nil {
		t.Fatalf("OpenLoopbackListener() = %v", err)
	}

	done := make(chan struct{})
	go func() {
		l.Close()
		l.Close()
		l.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() deadlocked on a repeat call")
	}
}

// Exactly one callback is captured. A second request — a duplicate tab, a
// retried redirect — must not block, panic, or overwrite the first result;
// Await must still return the first one.
func TestLoopbackListenerCapturesOnlyTheFirstCallback(t *testing.T) {
	l, err := OpenLoopbackListener()
	if err != nil {
		t.Fatalf("OpenLoopbackListener() = %v", err)
	}

	first := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + l.Addr() + "/callback?state=first&code=c1")
		if err == nil {
			_ = resp.Body.Close()
		}
		close(first)
	}()
	<-first

	// A duplicate arriving after the first must not hang the client or the
	// server, and must not replace the buffered result.
	resp, err := http.Get("http://" + l.Addr() + "/callback?state=second&code=c2")
	if err != nil {
		t.Fatalf("duplicate callback request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("duplicate callback status = %d, want 200 (it must not error, just be ignored)", resp.StatusCode)
	}

	result, err := l.Await(context.Background())
	if err != nil {
		t.Fatalf("Await() = %v", err)
	}
	if result.State != "first" || result.Code != "c1" {
		t.Errorf("result = %+v, want the first callback (state=first, code=c1)", result)
	}
}

// RedirectURL must point at /callback on the bound loopback address — that
// exact string is what gets registered as the OAuth RedirectURL and sent to
// the provider, so a mismatch here means the provider rejects the request
// before any of this package's own logic runs.
func TestLoopbackListenerRedirectURL(t *testing.T) {
	l, err := OpenLoopbackListener()
	if err != nil {
		t.Fatalf("OpenLoopbackListener() = %v", err)
	}
	defer l.Close()

	want := "http://" + l.Addr() + "/callback"
	if got := l.RedirectURL(); got != want {
		t.Errorf("RedirectURL() = %q, want %q", got, want)
	}
}
