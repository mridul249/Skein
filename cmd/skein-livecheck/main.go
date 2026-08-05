//go:build desktop && livecheck

// Command skein-livecheck runs a desktop-configured Skein server headlessly.
//
// It exists so the Go-side download path can be exercised against the real
// SQLite database and real connected Drives without launching a webview —
// the webview adds nothing to what is being verified here and a great deal of
// difficulty to observing it. Built only with `-tags desktop,livecheck`, so it
// is absent from every shipped binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/mridul249/Skein/internal/app"
)

func main() {
	// The desktop OAuth credentials, exactly as cmd/skein-desktop supplies
	// them. Without these, token refresh fails with provider_misconfigured and
	// every Drive read dies before a byte moves — which is Block 3b behaving
	// correctly, but it means this harness tests nothing.
	clientID := func() string { return os.Getenv("SKEIN_GOOGLE_DESKTOP_CLIENT_ID") }
	clientSecret := func() string { return os.Getenv("SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET") }

	a, err := app.Build(context.Background(),
		app.WithSQLiteDatabase(""),
		app.WithDesktopOAuth(clientID, clientSecret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	addr := os.Getenv("LIVECHECK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	if lerr := a.Listen(addr); lerr != nil {
		fmt.Fprintln(os.Stderr, "listen:", lerr)
		os.Exit(1)
	}
	fmt.Println("LISTENING", a.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if serr := a.Serve(ctx); serr != nil {
		fmt.Fprintln(os.Stderr, "serve:", serr)
	}
}
