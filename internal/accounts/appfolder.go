package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage/gdrive"
)

// folderKeyPrefix namespaces the app-folder singleflight so it cannot collide
// with the quota-sync group, which is keyed on the bare account id.
const folderKeyPrefix = "appfolder:"

// ensureAppFolder returns the provider folder id this account's shards belong
// in, creating it on first use.
//
// The race this guards is narrow and expensive to get wrong: two concurrent
// first-uploads both look, both find nothing, both create a folder, and the
// file's shards end up split across two folders that neither the UI nor the
// user can tell apart. Three defences, in order of how far they reach:
//
//  1. singleflight, keyed per account. Collapses concurrent callers inside one
//     process to a single attempt.
//  2. A re-read of the stored id inside the group. The goroutine that waited
//     may find the winner already persisted it, and must use that rather than
//     proceed to create its own.
//  3. A Drive-side list ordered by createdTime, oldest first, and a
//     conditional UPDATE that only writes when the column is still NULL. Two
//     separate *processes* have no shared singleflight, so this is what makes
//     them converge: both see the same oldest folder, and the loser of the
//     UPDATE re-reads the winner's id instead of overwriting it.
func (s *Service) ensureAppFolder(ctx context.Context, acct StoredAccount, client *http.Client) (string, error) {
	if acct.AppFolderID != "" {
		return acct.AppFolderID, nil
	}

	v, err, _ := s.flight.Do(folderKeyPrefix+acct.ID.String(), func() (any, error) {
		// (2) Another goroutine may have won while this one waited.
		fresh, err := s.store.GetAppFolderID(ctx, acct.ID)
		if err != nil && !errors.Is(err, skerr.ErrNotFound) {
			return nil, fmt.Errorf("re-read app folder: %w", err)
		}
		if fresh != "" {
			return fresh, nil
		}

		// A backend with no folder of its own, used only for the folder
		// calls below.
		probe := gdrive.New(client, "")

		// The per-user name. drive.file scope is per-OAuth-client, so two
		// Skein users on one Google account see the same folders; without a
		// per-user name the second adopts the first's folder. See
		// appFolderName.
		wantName, err := appFolderName(s.keyring, acct.UserID)
		if err != nil {
			return nil, err
		}

		// (3) Oldest wins, so separate processes converge on one folder.
		// FindFolder matches the name exactly, so this can only ever find
		// THIS user's folder.
		id, err := probe.FindFolder(ctx, wantName)
		if err != nil {
			return nil, fmt.Errorf("find app folder: %w", err)
		}

		created := false
		if id == "" {
			// No folder of this user's own. Before creating one, fall back to
			// a bare "Skein" folder if there is one — that is an install
			// predating per-user names (2026-08-05), and adopting it is what
			// keeps every shard already inside it reachable.
			//
			// Safe against the case this whole change exists to prevent: a
			// SECOND user reaches here only when the first user has already
			// claimed the bare folder by storing its id on their own
			// connected_accounts row. The bare folder is adopted at most once
			// per Drive, by whoever gets there first; everyone else creates
			// their own. That is why the claim below is conditional on the
			// store write winning.
			if wantName != gdrive.AppFolderName {
				legacy, lerr := probe.FindFolder(ctx, gdrive.AppFolderName)
				if lerr != nil {
					return nil, fmt.Errorf("find legacy app folder: %w", lerr)
				}
				if legacy != "" && !s.legacyFolderClaimed(ctx, acct, legacy) {
					id = legacy
				}
			}
		}
		if id == "" {
			id, err = probe.CreateFolder(ctx, wantName)
			if err != nil {
				return nil, fmt.Errorf("create app folder: %w", err)
			}
			created = true
		}

		// The UPDATE only writes when app_folder_id IS NULL. Losing it
		// means another process got there first; its id is authoritative.
		stored, err := s.store.SetAppFolderID(ctx, acct.ID, id)
		if err != nil {
			if errors.Is(err, skerr.ErrNotFound) {
				winner, rerr := s.store.GetAppFolderID(ctx, acct.ID)
				if rerr != nil {
					return nil, fmt.Errorf("read winning app folder: %w", rerr)
				}
				if winner != "" {
					return winner, nil
				}
			}
			return nil, fmt.Errorf("persist app folder: %w", err)
		}

		if created {
			// Best-effort. A missing README makes the folder less
			// self-explanatory; failing the upload over it would turn a
			// cosmetic problem into a functional one.
			if werr := probe.WriteReadme(ctx, stored); werr != nil {
				s.log.WarnContext(ctx, "could not write the app folder README",
					slog.String("account_id", acct.ID.String()),
					slog.String("error", werr.Error()))
			}
			s.log.InfoContext(ctx, "created provider app folder",
				slog.String("account_id", acct.ID.String()),
				slog.String("folder_id", stored))
		}
		return stored, nil
	})
	if err != nil {
		return "", err
	}

	id, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("accounts: app folder id had unexpected type %T", v)
	}
	return id, nil
}

// MigrateFolders moves shards that predate the app folder out of Drive root.
//
// Idempotent: a second run finds nothing left at root and reports zero moved.
type MigrateReport struct {
	AccountID uuid.UUID
	Email     string
	FolderID  string
	Moved     int
	Failed    int
	Err       error
}

// MigrateFolders ensures every active account has an app folder and reparents
// any stray root shards into it.
func (s *Service) MigrateFolders(ctx context.Context) ([]MigrateReport, error) {
	accts, err := s.store.ListAccountsForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}

	reports := make([]MigrateReport, 0, len(accts))
	for _, acct := range accts {
		rep := MigrateReport{AccountID: acct.ID, Email: acct.Email}

		if acct.Kind != "gdrive" {
			continue
		}

		backend, berr := s.backendFor(ctx, acct)
		if berr != nil {
			rep.Err = berr
			reports = append(reports, rep)
			continue
		}
		drive, ok := backend.(*gdrive.Backend)
		if !ok {
			rep.Err = fmt.Errorf("account %s is not a drive backend", acct.ID)
			reports = append(reports, rep)
			continue
		}
		rep.FolderID = drive.FolderID()

		strays, lerr := drive.ListRootShards(ctx)
		if lerr != nil {
			rep.Err = lerr
			reports = append(reports, rep)
			continue
		}

		for _, obj := range strays {
			if merr := drive.MoveToFolder(ctx, obj.ID, rep.FolderID); merr != nil {
				s.log.WarnContext(ctx, "could not move a stray shard",
					slog.String("account_id", acct.ID.String()),
					slog.String("object", obj.Name),
					slog.String("error", merr.Error()))
				rep.Failed++
				continue
			}
			rep.Moved++
		}
		reports = append(reports, rep)
	}
	return reports, nil
}

// legacyFolderClaimed reports whether some OTHER connected account already
// records this bare-"Skein" folder as its own.
//
// This is what stops a second Skein user inheriting the first user's folder
// while still letting a pre-2026-08-05 single-user install adopt the folder it
// has been using all along. The bare folder is adoptable exactly once per
// Drive: whoever stores its id first claims it, and everyone afterwards sees
// it claimed and creates their own per-user folder.
//
// Deliberately built on ListAccountsForSync, which is already in the Store
// interface and already used by this file. A dedicated
// "who owns folder X" query would be more direct but means new sqlc queries
// and regeneration, which this session is not permitted to do — and the scan
// is over one user's handful of connected drives, not a hot path: it runs only
// when an account has no stored folder AND a bare folder exists.
func (s *Service) legacyFolderClaimed(ctx context.Context, acct StoredAccount, folderID string) bool {
	accts, err := s.store.ListAccountsForSync(ctx)
	if err != nil {
		// Fail SAFE: if the claim cannot be established, assume it is taken
		// and create a fresh per-user folder. A redundant folder is a cosmetic
		// problem; wrongly adopting another user's folder is the leak this
		// change exists to close.
		s.log.WarnContext(ctx, "could not check whether the legacy app folder is claimed; "+
			"creating a per-user folder instead of adopting",
			slog.String("account_id", acct.ID.String()),
			slog.String("error", err.Error()))
		return true
	}
	for _, other := range accts {
		if other.ID != acct.ID && other.AppFolderID == folderID {
			return true
		}
	}
	return false
}
