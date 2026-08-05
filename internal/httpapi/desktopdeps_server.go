//go:build !desktop

package httpapi

// desktopDeps is empty on the server build: there are no desktop-only
// dependencies, and files.DownloadManager does not exist in this binary.
//
// It exists so Deps has the SAME SHAPE in both builds, which is what lets
// server.go embed it unconditionally. `unused` flags it here because in this
// build nothing reads it — correct, and precisely the point: the desktop
// build's counterpart in desktopdeps_desktop.go carries the real fields and is
// read by mountDesktop. `make lint` runs both tag sets (issue #44), so this is
// the one place the two configurations legitimately disagree.
//
//nolint:unused // empty by design in this build; the desktop build uses it
type desktopDeps struct{}
