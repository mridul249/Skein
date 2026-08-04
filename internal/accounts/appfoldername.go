package accounts

import (
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/storage/gdrive"
)

// appFolderSuffixLen is the number of hex characters appended to the folder
// name. Eight is enough that two users on one Drive will not collide in
// practice, and short enough that the folder still reads as a name rather than
// as a hash.
const appFolderSuffixLen = 8

// appFolderName returns the provider folder name for one Skein user:
//
//	Skein (a1b2c3d4)
//
// WHY PER-USER. drive.file scope is per-OAuth-client, not per-Skein-user, so
// two Skein users who connect the same Google account probe the same visible
// file set. With a bare "Skein" name the second user's probe finds and adopts
// the first user's folder — verified 2026-08-05, creates=1 for two users. Both
// users' shard objects then sit in one folder either can see in their own
// Drive UI. Contents stay encrypted and object names reveal nothing (they key
// on the file UUID), so this is a visibility leak rather than a
// confidentiality breach, but it is one worth closing.
//
// The suffix is HKDF over the user id with its own info string, so it is
// deterministic — the same user always resolves the same folder — and does not
// expose the user id itself in a folder name the user's Drive displays.
//
// No schema change: app_folder_id is already stored per connected_accounts
// row, so existing connections keep resolving by their stored id and never
// re-probe. This only affects connections made from here on.
func appFolderName(ring *skcrypto.Keyring, userID uuid.UUID) (string, error) {
	if ring == nil {
		// Without a keyring there is nothing to derive from. The bare name is
		// the pre-2026-08-05 behaviour and is still correct for a
		// single-user install.
		return gdrive.AppFolderName, nil
	}
	key, err := ring.Derive(skcrypto.InfoAppFolder, userID[:])
	if err != nil {
		return "", fmt.Errorf("derive app folder suffix: %w", err)
	}
	suffix := hex.EncodeToString(key)[:appFolderSuffixLen]
	return fmt.Sprintf("%s (%s)", gdrive.AppFolderName, suffix), nil
}
