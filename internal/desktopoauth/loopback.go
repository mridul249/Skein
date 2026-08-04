// Package desktopoauth is the desktop-only half of the OAuth flow: the
// ephemeral loopback listener and the browser-launching connect attempt. It
// depends on internal/accounts for the shared PKCE-aware exchange logic, but
// nothing in internal/accounts depends back on it — the server build
// (cmd/skein) never imports this package, so github.com/pkg/browser and the
// loopback HTTP server never link into it.
package desktopoauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mridul249/Skein/internal/accounts"
)

// loopbackCallbackTimeout bounds how long the ephemeral listener waits for
// Google to redirect back. It matches accounts.OAuthStateTTL: there is no
// point holding the port open longer than the state row it is waiting on
// remains valid, and a listener outliving its state cannot complete an
// exchange anyway.
const loopbackCallbackTimeout = accounts.OAuthStateTTL

// LoopbackResult is what the ephemeral callback listener captured.
type LoopbackResult struct {
	State string
	Code  string
	// Err is the provider's own error parameter (e.g. "access_denied" when
	// the user cancels consent). Code is empty whenever Err is set.
	Err string
}

// LoopbackListener is an ephemeral HTTP listener bound to 127.0.0.1 that
// waits for exactly one OAuth callback. Open it, use Addr to build the
// RedirectURL and AuthCodeURL, open the system browser, then call Await.
//
// Close is always safe to call more than once and must be called on every
// path — Await calls it internally before returning, so a caller that always
// reaches Await never needs to call Close itself. It exists separately only
// for a caller that fails between Open and Await (e.g. the browser could not
// be launched) and needs to release the port without waiting out the full
// timeout.
type LoopbackListener struct {
	ln  net.Listener
	srv *http.Server

	resultCh   chan LoopbackResult
	serveErrCh chan error
	closeOnce  sync.Once
}

// OpenLoopbackListener binds 127.0.0.1:0 and starts serving /callback in the
// background. The OS-assigned port is available immediately via Addr, before
// any callback has arrived — the caller needs it to construct the
// RedirectURL before it can even build the AuthCodeURL to send the browser
// to.
func OpenLoopbackListener() (*LoopbackListener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open loopback listener: %w", err)
	}

	l := &LoopbackListener{
		ln:         ln,
		resultCh:   make(chan LoopbackResult, 1),
		serveErrCh: make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", l.handleCallback)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { l.serveErrCh <- l.srv.Serve(ln) }()
	return l, nil
}

// Addr is the loopback address this listener is bound to, host:port.
func (l *LoopbackListener) Addr() string { return l.ln.Addr().String() }

// RedirectURL is the full callback URL to register as the OAuth config's
// RedirectURL and to send to the provider in the authorisation request.
func (l *LoopbackListener) RedirectURL() string {
	return "http://" + l.Addr() + "/callback"
}

func (l *LoopbackListener) handleCallback(w http.ResponseWriter, r *http.Request) {
	oauthErr := r.URL.Query().Get("error")

	select {
	case l.resultCh <- LoopbackResult{
		State: r.URL.Query().Get("state"),
		Code:  r.URL.Query().Get("code"),
		Err:   oauthErr,
	}:
		// The page says which happened. Note that oauthErr is passed as a
		// LOOKUP KEY, never as text: failureLanding maps it to one of a fixed
		// set of messages. This URL carries the authorization code and the
		// state, so echoing any part of it into the HTML would be reflected
		// XSS on the desktop app's own loopback origin.
		if oauthErr != "" {
			writeLanding(w, http.StatusOK, failureLanding(oauthErr))
			return
		}
		writeLanding(w, http.StatusOK, successLanding())
	default:
		// resultCh already holds its one buffered value: this is a second
		// request, a duplicate tab or a retried redirect. Nothing more to
		// record and nothing to unblock a second time.
		writeLanding(w, http.StatusOK, duplicateLanding())
	}
}

// Await blocks until exactly one callback request arrives, ctx is
// cancelled, or loopbackCallbackTimeout elapses — whichever is first. It
// always closes the listener before returning, on every path, so a failed
// or abandoned attempt never leaves the port held.
func (l *LoopbackListener) Await(ctx context.Context) (LoopbackResult, error) {
	// Close deliberately does not take ctx: it must run its own bounded
	// shutdown even when ctx is already cancelled or expired, which is
	// exactly the state it is called in on the timeout and cancellation
	// paths below. Threading ctx through would defeat that guarantee.
	defer l.Close() //nolint:contextcheck

	timeoutCtx, cancel := context.WithTimeout(ctx, loopbackCallbackTimeout)
	defer cancel()

	select {
	case result := <-l.resultCh:
		return result, nil
	case <-timeoutCtx.Done():
		if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return LoopbackResult{}, fmt.Errorf("await loopback callback: timed out after %s", loopbackCallbackTimeout)
		}
		return LoopbackResult{}, fmt.Errorf("await loopback callback: %w", ctx.Err())
	}
}

// Close shuts down the listener. Safe to call more than once — the actual
// shutdown runs only on the first call; later calls return immediately.
// (http.Server.Shutdown alone is safe to call twice, but serveErrCh is only
// ever sent to once by the single Serve goroutine, so waiting on it a second
// time would block forever without this guard.)
func (l *LoopbackListener) Close() {
	l.closeOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.srv.Shutdown(shutdownCtx)
		<-l.serveErrCh
	})
}
