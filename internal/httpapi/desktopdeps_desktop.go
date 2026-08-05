//go:build desktop

package httpapi

import "github.com/mridul249/Skein/internal/files"

// desktopDeps carries the desktop-only dependencies.
//
// Split from Deps by build tag because files.DownloadManager itself only
// exists under the desktop tag — putting the field on the shared struct would
// drag the type, and the route it backs, into the server binary.
type desktopDeps struct {
	// Downloads runs Go-side downloads. Nil leaves the routes unmounted.
	Downloads *files.DownloadManager
	// DownloadDir resolves the configured save directory at call time, so a
	// Settings change takes effect without a restart.
	DownloadDir func() string
}
