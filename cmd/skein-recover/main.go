//go:build desktop

// Command skein-recover is the operator tool for sidecar manifests.
//
// It exists because the two things an operator most needs during a recovery
// were only reachable over HTTP, and over HTTP they are awkward exactly when
// it matters: the desktop server binds 127.0.0.1:0 so the port is different
// every run, and the manifest routes sit behind SKEIN_BACKUP_TOKEN, which a
// desktop install has no reason to have set. Asking someone to find a random
// port and invent an operator token in the middle of a disaster is a bad
// procedure.
//
// This talks to the SQLite database and the connected drives directly, using
// the same services the app does. Three subcommands:
//
//	status    what is on the drives and whether it is recoverable
//	rewrite   regenerate every manifest from the database
//	restore   rebuild database rows from the manifests
//
// Build with `-tags desktop` (it needs the SQLite stores). Nothing here is in
// a shipped binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mridul249/Skein/internal/app"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "for restore: report what WOULD be recovered, write nothing")
	email := flag.String("email", "", "the account to act on (required)")
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" || *email == "" {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := app.Build(ctx,
		app.WithSQLiteDatabase(""),
		app.WithDesktopOAuth(
			func() string { return os.Getenv("SKEIN_GOOGLE_DESKTOP_CLIENT_ID") },
			func() string { return os.Getenv("SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET") },
		))
	if err != nil {
		fatal("build: %v", err)
	}
	defer func() { _ = a.Shutdown(ctx) }()

	userID, err := a.Auth.UserIDForEmail(ctx, *email)
	if err != nil {
		fatal("no account for %q in this database: %v\n"+
			"If you are recovering, register that address in the app first, "+
			"reconnect your drives, then run this again.", *email, err)
	}

	switch strings.ToLower(cmd) {
	case "status":
		report, rerr := a.Files.ManifestCoverageForUser(ctx, userID)
		if rerr != nil {
			fatal("status: %v", rerr)
		}
		c := report.Coverage
		fmt.Printf("account:            %s\n", *email)
		fmt.Printf("drives scanned:     %d\n", len(report.Accounts))
		for _, s := range report.Accounts {
			state := "ok"
			if !s.Scanned {
				state = "NOT SCANNED: " + s.Reason
			}
			fmt.Printf("  %s  manifests=%d  %s\n", s.AccountID, s.ManifestsFound, state)
		}
		fmt.Printf("files:              %d\n", c.Files)
		fmt.Printf("  covered:          %d\n", c.Covered)
		fmt.Printf("  partial:          %d\n", c.PartiallyCovered)
		fmt.Printf("  uncovered:        %d\n", c.Uncovered)
		fmt.Printf("  damaged:          %d\n", c.Damaged)
		fmt.Printf("  indeterminate:    %d\n", c.Indeterminate)
		if !report.Complete {
			fmt.Printf("\nINCOMPLETE — some drives could not be read:\n")
			for _, r := range report.IncompleteReasons {
				fmt.Printf("  - %s\n", r)
			}
		}

	case "rewrite":
		// Regenerates EVERY manifest, including ones that already exist.
		// Ordinary backfill skips a file that has a manifest, which is right —
		// but a manifest missing a field added later cannot be repaired from
		// the drives, only rewritten from the database.
		report, rerr := a.Files.RewriteManifestsForUser(ctx, userID)
		if rerr != nil {
			fatal("rewrite: %v", rerr)
		}
		fmt.Printf("rewrote manifests for %s\n", *email)
		fmt.Printf("  files:     %d\n", report.Coverage.Files)
		fmt.Printf("  covered:   %d\n", report.Coverage.Covered)
		fmt.Printf("  damaged:   %d (skipped: a manifest cannot recover a file whose shards are gone)\n",
			report.Coverage.Damaged)
		if !report.Complete {
			fmt.Printf("\nINCOMPLETE — run it again once every drive is reachable:\n")
			for _, r := range report.IncompleteReasons {
				fmt.Printf("  - %s\n", r)
			}
		}

	case "restore":
		report, rerr := a.Files.ReconstructAll(ctx, userID, *dryRun)
		if rerr != nil {
			fatal("restore: %v", rerr)
		}
		if *dryRun {
			fmt.Println("DRY RUN — nothing was written.")
		}
		fmt.Printf("manifests found:      %d\n", report.ManifestsFound)
		fmt.Printf("  unreadable:         %d\n", report.ManifestsUnreadable)
		fmt.Printf("  other accounts:     %d\n", report.ManifestsForOtherUsers)
		fmt.Printf("files recovered:      %d\n", report.FilesRecovered)
		fmt.Printf("shards recovered:     %d\n", report.ShardsRecovered)
		fmt.Printf("folders recreated:    %d\n", report.FoldersRecovered)
		fmt.Printf("already present:      %d\n", report.FilesAlreadyPresent)
		if !report.Complete {
			fmt.Printf("\nINCOMPLETE — there may be more to recover. Running again is safe:\n")
			for _, r := range report.IncompleteReasons {
				fmt.Printf("  - %s\n", r)
			}
		}

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `skein-recover — sidecar manifest operations

  skein-recover -email you@example.com status
      What is on your drives, and whether your files are recoverable.
      Read-only.

  skein-recover -email you@example.com rewrite
      Regenerate every manifest from the database. Run this once after
      upgrading, so existing files gain fields added by later versions.

  skein-recover -email you@example.com restore -dry-run
      Report what a restore would recover. Writes nothing.

  skein-recover -email you@example.com restore
      Rebuild database rows from the manifests on your drives. Only adds
      what is missing; never changes or deletes anything already there.

The database is the one skein-desktop uses (SKEIN_SQLITE_PATH, or the
default under your config directory). Stop the app before running this.
`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "skein-recover: "+format+"\n", args...)
	os.Exit(1)
}
