// Command skein-desktop runs the same Skein server as cmd/skein, inside a
// native window instead of a browser tab.
//
// Per Phase7.md 4.3, this does not rewrite the frontend onto Wails bindings.
// The server binds 127.0.0.1:0, the OS assigns a port, and the webview's
// asset server reverse-proxies every request to that port — one frontend,
// two shells, byte-identical requests and responses either way.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	waillinux "github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/app"
	"github.com/mridul60214/skein/internal/desktopoauth"
	"github.com/mridul60214/skein/internal/httpapi/handlers"
)

// version is stamped at build time via -ldflags.
var version = "dev"

// desktopClientID is the compiled-in default Desktop app OAuth client id
// (RFC 8252). It is not a secret — desktop clients don't have one, and
// distributing a client id publicly is how Google's own documentation
// describes this client type working. Empty until a real Google Cloud
// client exists; stamped in via -ldflags -X the same way version is.
// SKEIN_GOOGLE_DESKTOP_CLIENT_ID overrides it at runtime, which is what lets
// anyone use their own client and API quota (Phase7 Task 4.4 point 6).
var desktopClientID = ""

// minWidth/minHeight keep the window above the frontend's lg breakpoint
// (1024px, web/src/components/FileDetail.tsx:111) at which the detail
// drawer collapses from a side panel to full width. Below lg the layout is
// not broken, just narrower; picking lg itself as the floor means the app
// never opens into the collapsed state by default. Height is a conventional
// minimum that keeps the two-pane layout usable.
const (
	minWidth  = 1024
	minHeight = 700
)

// desktopDrainTimeout bounds the shutdown drain, deliberately much shorter
// than cfg.ShutdownTimeout (30s default). That figure is sized for the
// headless server's SIGTERM path, where nothing is waiting on the process to
// visibly respond. Here a human is looking at a window they just clicked
// close on: OS window managers report "not responding" well before 30s, and
// a user who sees nothing happen force-quits, which is the exact outcome a
// graceful drain exists to avoid. 4s clears comfortably under Ubuntu's
// not-responding dialog while still giving in-flight requests and the DB
// pool a real chance to close cleanly.
const desktopDrainTimeout = 4 * time.Second

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := app.Build(ctx, app.WithDesktopConnect(newDesktopConnector))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: %v\n", err)
		os.Exit(1)
	}
	a.Logger.Info("version", "version", version)

	// 127.0.0.1:0: the OS assigns a free port. A fixed port would collide
	// with `skein serve` running on the same machine for development, which
	// is exactly the setup this was built and tested against.
	if err := a.Listen("127.0.0.1:0"); err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: %v\n", err)
		os.Exit(1)
	}
	a.Logger.Info("desktop server bound", slog.String("addr", a.Addr()))

	go func() {
		if err := a.Serve(ctx); err != nil {
			a.Logger.Error("serve exited", "error", err.Error())
		}
	}()

	target, err := url.Parse("http://" + a.Addr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: parse server address: %v\n", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// drainDone is what main() actually waits on before returning — not
	// a.Serve's return, which fires as soon as httpSrv.Shutdown's first line
	// runs and races the rest of a.Shutdown (workers.Wait, pool.Close) that
	// still has to complete afterwards. Waiting on a.Serve instead of on
	// this let main() return, and the process exit, while that tail end of
	// Shutdown was still running in a goroutine nothing was watching — the
	// DB pool close was a race, not a guarantee. This channel closes only
	// once drainInBackground has actually finished one of its two exits.
	drainDone := make(chan struct{})

	err = wails.Run(&options.App{
		Title:     "Skein",
		Width:     minWidth,
		Height:    minHeight,
		MinWidth:  minWidth,
		MinHeight: minHeight,
		AssetServer: &assetserver.Options{
			Handler: proxy,
		},
		Linux: &waillinux.Options{
			ProgramName: "skein-desktop",
		},
		OnShutdown: func(_ context.Context) {
			// Wails calls OnShutdown once the window is already closing, on
			// the UI thread, and the window does not finish closing until
			// this returns. It must therefore return immediately — never
			// block on the drain, not even briefly, or the window stays
			// visibly frozen for however long that takes and the OS offers
			// to force-quit. Draining synchronously here (the original bug)
			// froze the window for up to cfg.ShutdownTimeout (30s default):
			// cancel() ran only after a.Shutdown returned, but a.Shutdown's
			// own a.workers.Wait() blocks on ctx (the outer, Serve-scoped
			// context) being cancelled — the two were sequenced backwards,
			// so the drain could in fact never finish on its own and every
			// close was actually a hang, not just a slow drain.
			//
			// cancel() runs synchronously and returns instantly (it does
			// not wait for anything to react to the cancellation); the
			// bounded drain runs entirely in a background goroutine that
			// this function does not wait on in any way.
			cancel()
			go drainInBackground(a, drainDone) //nolint:contextcheck // deliberately detached from OnShutdown's ctx; see drainInBackground's own doc comment
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: %v\n", err)
		os.Exit(1)
	}

	// wails.Run returned normally: OnShutdown already fired and started the
	// drain. Wait for it to actually finish — see drainDone's own comment
	// above for why this must not be a.Serve's return instead.
	<-drainDone
}

// drainInBackground runs the bounded shutdown drain and closes done when it
// is finished. It is itself launched via `go` from OnShutdown, so blocking
// here does not block the UI thread — see the reasoning in OnShutdown for
// why OnShutdown itself must not. Two ways this function ends: a.Shutdown
// returns within desktopDrainTimeout (the normal case, done closes), or it
// does not, and the process exits directly rather than letting a.Shutdown
// keep running unobserved (done never closes, because nothing is left to
// observe it). A desktop app that keeps running after its window is gone is
// worse than one that leaks a DB connection.
func drainInBackground(a *app.App, done chan<- struct{}) {
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		drainCtx, drainCancel := context.WithTimeout(context.Background(), desktopDrainTimeout)
		defer drainCancel()
		if err := a.Shutdown(drainCtx); err != nil {
			a.Logger.Error("drain did not finish cleanly", "error", err.Error())
		}
	}()

	// The extra desktopDrainTimeout margin here, on top of the identical
	// deadline already passed to a.Shutdown via drainCtx above, covers the
	// gap between a context deadline elapsing and a.Shutdown actually
	// observing it and returning (Shutdown's own internal steps — closing
	// the pool, waiting on workers — are not themselves ctx-aware once the
	// HTTP drain step has returned). Without it, a slow-but-not-hung
	// Shutdown could lose this race by a few milliseconds and trigger an
	// unnecessary hard exit.
	select {
	case <-shutdownDone:
		a.Logger.Info("drained cleanly")
		close(done)
	case <-time.After(2 * desktopDrainTimeout):
		a.Logger.Warn("drain did not finish within the deadline; exiting anyway",
			slog.Duration("timeout", 2*desktopDrainTimeout))
		os.Exit(0)
	}
}

// newDesktopConnector builds the desktop OAuth connector for
// app.WithDesktopConnect. resolveDesktopClientID is re-evaluated on every
// connect attempt (see desktopoauth.NewConnector's clientID parameter), not
// just once here, so an operator can set SKEIN_GOOGLE_DESKTOP_CLIENT_ID and
// retry a connect without restarting the app.
func newDesktopConnector(svc *accounts.Service, log *slog.Logger) handlers.DesktopConnector {
	return desktopoauth.NewConnector(svc, log, resolveDesktopClientID)
}

// resolveDesktopClientID prefers an operator-set override — "use my own
// Google client" — over the id compiled into the binary at build time. Read
// directly from the environment rather than config.Config: the factory this
// feeds runs inside app.Build, before Build has returned a Config to read.
func resolveDesktopClientID() string {
	if v := os.Getenv("SKEIN_GOOGLE_DESKTOP_CLIENT_ID"); v != "" {
		return v
	}
	return desktopClientID
}
