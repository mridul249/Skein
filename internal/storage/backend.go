// Package storage defines the provider-agnostic Backend interface and the
// types it speaks in. Everything downstream of this — striping, encryption,
// the upload and download paths — is written against the interface and knows
// nothing about Google Drive.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Kind identifies a backend implementation.
type Kind string

// The backend kinds. Only gdrive and local exist today; s3 is Phase 6 and is
// named here so the database enum does not have to change to add it.
const (
	KindGoogleDrive Kind = "gdrive"
	KindLocal       Kind = "local"
	KindS3          Kind = "s3"
)

// Errors a backend may return. Callers match on these rather than on
// provider-specific status codes.
var (
	ErrObjectNotFound = errors.New("storage: object not found")
	ErrSizeMismatch   = errors.New("storage: byte count did not match the declared size")
	ErrUnauthorized   = errors.New("storage: provider rejected the credentials")
	ErrQuota          = errors.New("storage: provider is out of space")
	ErrRangeNotSat    = errors.New("storage: requested range is not satisfiable")

	// ErrRateLimited is the provider asking the caller to slow down. It is
	// deliberately distinct from ErrUnauthorized: Drive reports both as 403,
	// but a rate limit is transient and retryable while a revoked grant needs
	// the user to re-authorise. Collapsing the two marks a working account
	// needs_reauth the first time a burst is throttled.
	ErrRateLimited = errors.New("storage: provider is rate limiting")
)

// ObjectSpec describes an object about to be written.
type ObjectSpec struct {
	// Name is what the object is called at the provider. It is never the
	// user's filename: see NameForShard.
	Name string

	// Size is authoritative. An implementation that receives a different
	// number of bytes must fail rather than store a truncated object.
	Size int64

	// ContentType is what is declared to the provider. It is always
	// application/octet-stream for content Skein wrote, because the bytes
	// are ciphertext and describing them otherwise would be a lie.
	ContentType string
}

// ObjectRef locates a stored object.
type ObjectRef struct {
	// ProviderID is the provider's own identifier — a Drive file id, an S3
	// key. Opaque to everything above this package.
	ProviderID string
	Size       int64
}

// ByteRange is a half-open request for [Start, Start+Length).
type ByteRange struct {
	Start  int64
	Length int64
}

// Quota is a provider's reported capacity.
type Quota struct {
	TotalBytes int64
	UsedBytes  int64
}

// FreeBytes returns the unused capacity, never negative.
func (q Quota) FreeBytes() int64 {
	free := q.TotalBytes - q.UsedBytes
	if free < 0 {
		return 0
	}
	return free
}

// Backend is one storage provider account.
//
// Implementations must stream. Put may not read its reader into memory, and
// Get may not buffer a response body; the memory ceiling in Phases.md §3 is
// enforced against the whole path, and a buffering backend breaks it no matter
// how careful the layers above are.
type Backend interface {
	// Put streams r to the provider. It must not buffer the full body.
	// spec.Size is authoritative: an implementation rejects a mismatched
	// byte count and leaves no partial object behind.
	Put(ctx context.Context, r io.Reader, spec ObjectSpec) (ObjectRef, error)

	// Get returns a reader over the object. rng == nil means the whole
	// object. The returned int64 is the number of bytes the reader will
	// produce. The caller always closes the reader.
	Get(ctx context.Context, ref ObjectRef, rng *ByteRange) (io.ReadCloser, int64, error)

	// Delete removes an object. Deleting something already gone is not an
	// error: cleanup paths run more than once.
	Delete(ctx context.Context, ref ObjectRef) error

	// Quota reports the account's capacity.
	Quota(ctx context.Context) (Quota, error)

	// Kind identifies the implementation.
	Kind() Kind
}

// ListedObject is one object found by Lister.List.
type ListedObject struct {
	// ProviderID is the provider's own identifier, the same value an
	// ObjectRef carries.
	ProviderID string
	// Name is the provider-side object name, e.g. a NameForShard result or a
	// files.ManifestName one.
	Name string
	Size int64
}

// Lister enumerates the objects Skein has written to one account.
//
// AN OPTIONAL CAPABILITY, NOT PART OF Backend, and deliberately so. Every
// other operation in this package addresses an object Skein already knows
// about, by an id the database holds; listing is the one operation that exists
// to discover objects the database does NOT know about, which is a different
// job with a different failure mode. Only reconstruction needs it.
//
// A backend that cannot list is not broken. Reconstruction must treat an
// account it cannot enumerate as INDETERMINATE — "I could not look here" — and
// never as "there is nothing here", because concluding absence from an
// unavailable listing is exactly how a recovery decides files are gone when
// they are not.
type Lister interface {
	// List returns every object Skein wrote to this account, scoped to its
	// app folder. Implementations page internally and return the whole set.
	List(ctx context.Context) ([]ListedObject, error)
}

// NameForShard returns the provider-side object name for one shard.
//
// The user's filename never reaches the provider. It is metadata Skein holds
// and the provider does not need, and leaking it would undo much of the point
// of encrypting the content.
func NameForShard(fileID string, index int) string {
	return fmt.Sprintf("skein-%s-%04d.bin", fileID, index)
}

// CopyBufferSize is the fixed buffer every streaming copy in Skein uses.
//
// 256 KiB is large enough that syscall overhead is irrelevant at gigabit
// speeds and small enough that ten concurrent uploads cost 2.5 MB, not 2.5 GB.
// This constant is the reason file size does not influence RSS.
const CopyBufferSize = 256 * 1024
