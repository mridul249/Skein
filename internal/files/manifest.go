package files

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/storage"
)

// ManifestVersion is the format version carried in every manifest.
//
// Written and checked explicitly so a future format change is a decision
// rather than a surprise: a reader that meets an unknown version must refuse
// rather than guess at fields it does not understand.
const ManifestVersion = 1

// ManifestPrefix is the filename prefix every sidecar manifest carries.
//
// The full name is `.skein_manifest_<file_id>.enc`. The leading dot is
// cosmetic at Google Drive (which has no hidden-file convention) but matters
// on the local backend and to anyone browsing a synced folder.
//
// Reconstruction finds manifests by listing for this prefix, so it is part of
// the on-disk contract, not an implementation detail.
const ManifestPrefix = ".skein_manifest_"

// ManifestName returns the provider object name for one file's manifest.
func ManifestName(fileID uuid.UUID) string {
	return ManifestPrefix + fileID.String() + ".enc"
}

// IsManifestName reports whether a provider object name is a sidecar manifest.
func IsManifestName(name string) bool {
	return strings.HasPrefix(name, ManifestPrefix) && strings.HasSuffix(name, ".enc")
}

// ManifestShard is one shard's entry in a manifest.
//
// BOTH SIZES ARE CARRIED, and this is issue #9 applied rather than
// rediscovered: `size_bytes` on a shard row is CIPHERTEXT (the provider object
// size) while `plain_size_bytes` is PLAINTEXT. A reconstruction needs
// PlainOffset and PlainSize to rebuild the byte layout; a doctor needs
// CiphertextSize to compare against the provider object. Recording one and
// deriving the other is impossible once encryption is on, because the
// expansion depends on the frame count.
type ManifestShard struct {
	Index int32 `json:"index"`
	// AccountID is the connected account this shard lives on. A pointer
	// because a shard whose account was disconnected carries NULL, and a
	// manifest must record that honestly rather than inventing an owner.
	AccountID        *uuid.UUID `json:"account_id"`
	ProviderObjectID string     `json:"provider_object_id"`
	CiphertextSize   int64      `json:"ciphertext_size_bytes"`
	PlainSize        int64      `json:"plain_size_bytes"`
	PlainOffset      int64      `json:"plain_offset"`
	// SHA256 is hex of the shard digest, over the PLAINTEXT slice as built
	// (upload.go writes it that way; see issue #9). Hex rather than base64 so
	// a manifest read by a human during a recovery can be compared against
	// the database by eye.
	SHA256 string `json:"sha256,omitempty"`
}

// Manifest is the sidecar record of one file: everything needed to rebuild its
// database rows from the drives alone.
//
// WHAT THIS IS FOR, stated because it bounds what belongs in it: the database
// is the only record of which shard belongs to which file, in what order, and
// how to lay the bytes back out. Lose it and the shards are opaque objects.
// This is that record, written beside the shards themselves.
//
// WHAT IT IS NOT FOR: it is not a backup of the file, and it does NOT protect
// against losing SKEIN_MASTER_KEY. The manifest is encrypted under a key
// derived from that same master key, so a lost key loses the manifests too —
// which is exactly why docs/BACKUP.md insists the exported key must not live
// beside the data.
type Manifest struct {
	Version int       `json:"version"`
	FileID  uuid.UUID `json:"file_id"`
	// UserID is the owner's id AT THE TIME OF WRITING. It is NOT a durable
	// identity: user ids are random UUIDs minted at registration, so a
	// database rebuilt from scratch mints a different one and this can never
	// match again. Kept because it is the right check while the database
	// survives — two Skein users sharing one Google account see each other's
	// manifest objects, and this is what stops one adopting the other's files.
	UserID uuid.UUID `json:"user_id"`

	// UserEmail is the DURABLE identity anchor, and the reason real recovery
	// is possible at all.
	//
	// Found by the owner testing exactly the scenario that matters: fresh
	// database, re-registered the same address, reconnected the same drives —
	// and recovered NOTHING, because the new registration minted a new user id
	// and every manifest carried the old one. Enforcing isolation through a
	// value that dies with the database makes isolation and recoverability
	// mutually exclusive.
	//
	// Email is UNIQUE per instance (users.email is UNIQUE NOCASE) and is what
	// the user re-enters when rebuilding, so it survives the one event this
	// whole feature exists for.
	UserEmail string `json:"user_email,omitempty"`

	FileName       string    `json:"file_name"`
	PlainSizeBytes int64     `json:"plain_size_bytes"`
	MimeType       string    `json:"mime_type,omitempty"`
	FolderPath     []string  `json:"folder_path,omitempty"`
	CreatedAt      time.Time `json:"created_at"`

	// IsEncrypted is a PROPERTY OF THE STORED BYTES, so it must travel with
	// them rather than be inferred at recovery time.
	//
	// Found by audit before backfill, 2026-08-06. Reconstruction previously
	// took this from the server's current SKEIN_ENCRYPTION_ENABLED setting,
	// which is a setting that can differ between the instance that wrote the
	// shards and the one recovering them. The read path drives decryption from
	// the ROW (download.go: `encrypted: file.IsEncrypted`), so a recovered row
	// carrying the wrong value makes the file unreadable — and it would fail
	// during a recovery, which is the worst possible place to discover it.
	//
	// A pointer so a manifest written before this field existed decodes as
	// nil rather than as a confident `false`. Reconstruction falls back to the
	// server setting only in that case, and says so.
	IsEncrypted *bool `json:"is_encrypted,omitempty"`

	Shards []ManifestShard `json:"shards"`
}

// ManifestFor builds the manifest describing one committed file.
//
// folderPath is the file's folder chain from the root, outermost first, so a
// reconstruction can recreate the tree. Empty for a file at the root.
func ManifestFor(f File, folderPath []string, userEmail string) Manifest {
	encrypted := f.IsEncrypted
	m := Manifest{
		Version:        ManifestVersion,
		FileID:         f.ID,
		UserID:         f.UserID,
		UserEmail:      userEmail,
		FileName:       f.Name,
		PlainSizeBytes: f.SizeBytes,
		MimeType:       f.DeclaredMime,
		FolderPath:     folderPath,
		CreatedAt:      f.CreatedAt,
		IsEncrypted:    &encrypted,
		Shards:         make([]ManifestShard, 0, len(f.Shards)),
	}
	for _, sh := range f.Shards {
		m.Shards = append(m.Shards, ManifestShard{
			Index:            sh.Index,
			AccountID:        sh.AccountID,
			ProviderObjectID: sh.ProviderID,
			CiphertextSize:   sh.SizeBytes,
			PlainSize:        sh.PlainSize,
			PlainOffset:      sh.PlainOffset,
			SHA256:           hex.EncodeToString(sh.SHA256),
		})
	}
	return m
}

// SealManifest encrypts a manifest under a key derived for this file.
//
// The salt is the file id, so one file's manifest key cannot open another's,
// exactly as the file content key is derived. InfoManifest keeps it separate
// from the content key entirely — see kdf.go.
func SealManifest(ring *skcrypto.Keyring, m Manifest) ([]byte, error) {
	if ring == nil {
		return nil, fmt.Errorf("manifest: no keyring")
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	sealed, err := ring.Seal(skcrypto.InfoManifest, m.FileID[:], plain)
	if err != nil {
		return nil, fmt.Errorf("seal manifest: %w", err)
	}
	return sealed, nil
}

// OpenManifest decrypts and validates a sealed manifest.
//
// fileID is the id the caller expects, taken from the object NAME. It is
// checked against the id inside the ciphertext, so a manifest cannot be
// renamed to impersonate another file's — the same structural argument the
// capability URL makes by binding the file id into the signature.
func OpenManifest(ring *skcrypto.Keyring, fileID uuid.UUID, sealed []byte) (Manifest, error) {
	if ring == nil {
		return Manifest{}, fmt.Errorf("manifest: no keyring")
	}
	plain, err := ring.Open(skcrypto.InfoManifest, fileID[:], sealed)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest: %w", err)
	}

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&m); derr != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", derr)
	}

	// An unknown version is refused rather than partially understood. A reader
	// that guesses at a format it does not know is how a "recovery" silently
	// produces wrong byte offsets.
	if m.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf(
			"manifest: version %d is not supported by this build (expected %d)",
			m.Version, ManifestVersion)
	}
	if m.FileID != fileID {
		return Manifest{}, fmt.Errorf(
			"manifest: contents describe file %s but it was stored as %s",
			m.FileID, fileID)
	}
	return m, nil
}

// emailFor resolves the owner's durable identity for a manifest.
//
// Best effort: a manifest without an email is still worth writing — it stays
// recoverable for as long as the database survives. But it is warned about,
// because it is NOT recoverable after a rebuild, which is the case manifests
// exist for.
func (s *Service) emailFor(ctx context.Context, userID uuid.UUID) string {
	if s.users == nil {
		return ""
	}
	email, err := s.users.EmailForUser(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "could not resolve the owner's email for a manifest; "+
			"it will not be recoverable after a database rebuild",
			slog.String("user_id", userID.String()),
			slog.String("error", err.Error()))
		return ""
	}
	return email
}

// ManifestWriter writes sidecar manifests.
//
// A NARROW INTERFACE CALLED FROM EXACTLY ONE PLACE, and that is deliberate
// rather than tidiness: manifests are written at upload commit, which is the
// path the reservation rewrite replaces next. Keeping this to one call at one
// point means that rewrite moves a call rather than untangling manifest logic
// interleaved with reservation logic.
type ManifestWriter interface {
	// WriteManifest stores one file's manifest on every account holding one
	// of its shards. It must never return an error that fails the upload —
	// see Service.writeManifest.
	WriteManifest(ctx context.Context, f File, folderPath []string)
}

// writeManifest stores the manifest for a committed file.
//
// ONE COPY PER ACCOUNT HOLDING A SHARD, NOT ONE COPY TOTAL. A single-copy
// scheme means losing that one account loses the map to every other account —
// so any single surviving drive must be enough to bootstrap discovery of all
// the rest. N copies of a few hundred bytes is a trivial price for that.
//
// A FAILURE HERE NEVER FAILS THE UPLOAD. The manifest is a redundancy layer;
// letting it break the primary path inverts the entire point of adding it.
// Every failure is logged and the file remains committed and readable.
func (s *Service) writeManifest(ctx context.Context, f File, folderPath []string) {
	if s.keyring == nil {
		return // encryption disabled in tests; nothing to derive a key from
	}

	sealed, err := SealManifest(s.keyring, ManifestFor(f, folderPath, s.emailFor(ctx, f.UserID)))
	if err != nil {
		s.log.WarnContext(ctx, "could not build sidecar manifest",
			slog.String("file_id", f.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	// One copy per DISTINCT account. A file with three shards on two drives
	// gets two manifests, not three.
	accounts := make([]uuid.UUID, 0, len(f.Shards))
	seen := map[uuid.UUID]bool{}
	for _, sh := range f.Shards {
		if sh.AccountID == nil || seen[*sh.AccountID] {
			continue
		}
		seen[*sh.AccountID] = true
		accounts = append(accounts, *sh.AccountID)
	}

	name := ManifestName(f.ID)
	var written int
	for _, acct := range accounts {
		backend, berr := s.backends.For(ctx, f.UserID, &acct)
		if berr != nil {
			s.log.WarnContext(ctx, "could not resolve a drive for a sidecar manifest",
				slog.String("file_id", f.ID.String()),
				slog.String("account_id", acct.String()),
				slog.String("error", berr.Error()))
			continue
		}

		// Through the shared pool: this is N extra provider calls per upload,
		// and the pool is where 429 handling and bounded concurrency already
		// live. New bulk traffic must not reinvent either.
		perr := s.runPooled(ctx, func(ctx context.Context) error {
			_, werr := backend.Put(ctx, bytes.NewReader(sealed), storage.ObjectSpec{
				Name:        name,
				Size:        int64(len(sealed)),
				ContentType: "application/octet-stream",
			})
			return werr
		})
		if perr != nil {
			s.log.WarnContext(ctx, "could not write a sidecar manifest",
				slog.String("file_id", f.ID.String()),
				slog.String("account_id", acct.String()),
				slog.String("error", perr.Error()))
			continue
		}
		written++
	}

	if written == 0 && len(accounts) > 0 {
		// Worth its own line: the file is fine, but it is now protected by the
		// database alone, which is precisely the single point of failure
		// manifests exist to remove.
		s.log.WarnContext(ctx, "no sidecar manifest was written for this file",
			slog.String("file_id", f.ID.String()),
			slog.Int("accounts", len(accounts)))
		return
	}
	s.log.InfoContext(ctx, "sidecar manifests written",
		slog.String("file_id", f.ID.String()),
		slog.Int("copies", written))
}
