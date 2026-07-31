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

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	waillinux "github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/mridul60214/skein/internal/app"
)

// version is stamped at build time via -ldflags.
var version = "dev"

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

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := app.Build(ctx)
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

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- a.Serve(ctx) }()

	target, err := url.Parse("http://" + a.Addr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: parse server address: %v\n", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	shutdown := func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), a.Config.ShutdownTimeout)
		defer drainCancel()
		if err := a.Shutdown(drainCtx); err != nil {
			a.Logger.Error("drain did not finish cleanly", "error", err.Error())
		}
		cancel()
	}

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
		OnShutdown: func(ctx context.Context) {
			// Wails calls OnShutdown once the window is already closing;
			// this is the only hook guaranteed to run on every close path
			// (titlebar X, Cmd/Alt+F4, OS session end), so it is where the
			// server actually stops rather than OnBeforeClose, which can be
			// skipped by SingleInstanceLock reactivation or vetoed by a
			// future confirmation dialog.
			shutdown()
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: %v\n", err)
		os.Exit(1)
	}

	// wails.Run returned normally: OnShutdown already stopped the server.
	// Still drain serveErrCh so a Serve error surfaced after window close
	// is not silently dropped.
	if err := <-serveErrCh; err != nil {
		fmt.Fprintf(os.Stderr, "skein-desktop: %v\n", err)
		os.Exit(1)
	}
}
