//go:build desktop

package app

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi"
)

// wireDesktopDownloads installs the Go-side download path.
//
// Desktop build only — this file is not compiled into the server binary, so
// neither is files.DownloadManager or the route that reaches it.
func wireDesktopDownloads(deps *httpapi.Deps, filesSvc *files.Service, configured string) {
	deps.Downloads = files.NewDownloadManager(filesSvc)
	deps.DownloadDir = downloadDirResolver(configured)
}

// downloadDirResolver resolves the save directory at call time, so a Settings
// change takes effect without a restart.
//
// Default is the XDG Downloads directory, which is where WebKitGTK already
// writes today — the Go path deliberately does not change where files land,
// only how they get there.
func downloadDirResolver(configured string) func() string {
	var once sync.Once
	var fallback string

	return func() string {
		if configured != "" {
			return configured
		}
		once.Do(func() { fallback = defaultDownloadDir() })
		return fallback
	}
}

// defaultDownloadDir follows the XDG user-dirs convention, falling back to
// ~/Downloads and then the working directory.
func defaultDownloadDir() string {
	if dir := os.Getenv("XDG_DOWNLOAD_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	downloads := filepath.Join(home, "Downloads")
	if info, serr := os.Stat(downloads); serr == nil && info.IsDir() {
		return downloads
	}
	return home
}
