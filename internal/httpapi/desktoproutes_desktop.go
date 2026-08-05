//go:build desktop

package httpapi

import (
	"github.com/go-chi/chi/v5"

	"github.com/mridul249/Skein/internal/httpapi/handlers"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// mountDesktop mounts the Go-side download routes.
//
// This file is compiled ONLY into the desktop binary. The server build gets
// the no-op in desktoproutes_server.go, so the routes are absent from that
// binary entirely rather than merely unreachable — POST /api/desktop/downloads
// writes to a path on the machine running the server, which on a hosted
// deployment is not the caller's machine.
//
// Verified by TestServerBinaryHasNoDesktopRoutes, which greps the built server
// binary rather than trusting this comment or the tag.
func (s *Server) mountDesktop(api chi.Router) {
	if s.deps.Downloads == nil || s.deps.Auth == nil {
		return
	}
	h := handlers.NewDesktopDownloads(s.deps.Downloads, s.deps.DownloadDir)

	api.Route("/desktop", func(g chi.Router) {
		g.Use(middleware.Auth(s.deps.Auth, httpx.WriteError))
		g.Use(middleware.RateLimit(middleware.NewLimiter(apiRatePerMin)))

		g.Get("/capabilities", h.Capability)
		g.Post("/downloads", h.Start)
		g.Get("/downloads", h.List)
		g.Delete("/downloads/{id}", h.Cancel)
		// The SSE stream. Long-lived by design, so it sits outside the JSON
		// body limit and carries its own heartbeat.
		g.Get("/downloads/{id}/events", h.Events)
	})
}
