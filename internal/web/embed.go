// Package web embeds the built frontend so `go build` produces one artifact.
//
// internal/web/dist is populated by `make web` and is gitignored; only a
// .gitkeep is committed. A binary built without running the frontend build
// still starts and serves the API — Handler reports that the UI is absent
// rather than failing at compile time.
package web

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// ErrNoUI reports that the binary was built without a frontend bundle.
var ErrNoUI = errors.New("web: no frontend bundle embedded; run `make web` before `go build`")

// FS returns the built frontend rooted at dist.
func FS() (fs.FS, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNoUI
	}
	return sub, nil
}

// Handler serves the SPA: hashed assets with a long cache, everything else
// falling back to index.html so client-side routing works on a deep link.
// It returns ErrNoUI when no bundle was embedded.
func Handler() (http.Handler, error) {
	sub, err := FS()
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		if _, err := fs.Stat(sub, upath); err != nil {
			// Unknown path: hand it to the SPA router. Never fall back
			// for asset requests, so a missing bundle chunk 404s
			// instead of returning HTML with a JS content type.
			if strings.HasPrefix(upath, "assets/") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, sub)
			return
		}

		// Vite emits content-hashed filenames under assets/, so those are
		// immutable. index.html never is.
		if strings.HasPrefix(upath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}), nil
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(b)
}
