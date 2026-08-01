// Command skein runs the Skein server: one binary, migrations and frontend
// included. It validates configuration, migrates, listens, and drains.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mridul249/Skein/internal/app"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	// Subcommands are a flat switch rather than a flag package: there are
	// two of them, and the default is the one everybody runs.
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "serve":
		err = run()
	case "migrate-folders":
		err = runMigrateFolders()
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "skein: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		// The logger may not exist yet if config failed, so this one
		// write goes to stderr directly.
		fmt.Fprintf(os.Stderr, "skein: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `skein — your free cloud accounts, pooled, striped and encrypted

  skein [serve]        run the server (default)
  skein migrate-folders  move shards left at drive root into the Skein folder

Configuration comes from the environment; see .env.example.
`)
}

func run() error {
	// Signal context first, so a Ctrl-C during migration is honoured.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Build(ctx)
	if err != nil {
		return err
	}
	a.Logger.Info("version", "version", version)

	if err := a.Listen(a.Config.Addr); err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.Serve(ctx) }()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-ctx.Done():
		stop() // restore default handling: a second signal kills immediately
		a.Logger.Info("shutdown signal received; draining",
			"timeout", a.Config.ShutdownTimeout)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), a.Config.ShutdownTimeout)
	defer cancel()

	if err := a.Shutdown(drainCtx); err != nil {
		a.Logger.Error("drain did not finish cleanly", "error", err.Error())
		return err
	}

	a.Logger.Info("drained cleanly; exiting")
	return nil
}
