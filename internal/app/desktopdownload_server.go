//go:build !desktop

package app

import (
	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi"
)

// wireDesktopDownloads is a no-op on the server build: the download manager
// does not exist in this binary.
func wireDesktopDownloads(*httpapi.Deps, *files.Service, string) {}
