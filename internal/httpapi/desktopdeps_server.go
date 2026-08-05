//go:build !desktop

package httpapi

// desktopDeps is empty on the server build: there are no desktop-only
// dependencies, and files.DownloadManager does not exist in this binary.
type desktopDeps struct{}
