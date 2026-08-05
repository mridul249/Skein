// Compile-time proof that both shipped backends satisfy Lister.
//
// Lister is an OPTIONAL capability, so nothing else forces these to keep
// implementing it: reconstruction type-asserts at runtime and degrades to
// "indeterminate" when the assertion fails. That degradation is correct
// behaviour for a backend that genuinely cannot list, and it is also exactly
// how a real backend silently dropping List would look. This makes the
// difference a compile error instead.
package storage_test

import (
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/gdrive"
	"github.com/mridul249/Skein/internal/storage/local"
)

var _ storage.Lister = (*local.Backend)(nil)
var _ storage.Lister = (*gdrive.Backend)(nil)
