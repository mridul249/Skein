//go:build !desktop

package httpapi

import "github.com/go-chi/chi/v5"

// mountDesktop is a no-op on the server build.
//
// The desktop download routes are not merely disabled here — the handler and
// the manager that back them are not compiled into this binary at all, so
// there is nothing to reach even with a crafted request. See
// desktoproutes_desktop.go.
func (s *Server) mountDesktop(chi.Router) {}
