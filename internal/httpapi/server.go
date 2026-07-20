// Package httpapi wires the router, the middleware chain, and the handlers.
// Handlers here are thin: decode, call a service, encode. Business logic lives
// in the domain packages and knows nothing about HTTP.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5"

	"github.com/mridul60214/skein/internal/config"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
)

// Health reports process and dependency liveness.
type Health interface {
	// Ping returns nil when the database is reachable and migrated.
	Ping(ctx context.Context) error
}

// Deps is everything the HTTP layer needs. Wiring happens in main.go;
// nothing here reaches for a package-level singleton.
type Deps struct {
	Config *config.Config
	Logger *slog.Logger
	Health Health
}

// Server owns the router and the middleware chain.
type Server struct {
	deps    Deps
	router  chi.Router
	trusted []netip.Prefix
}

// New builds the server. It returns an error when configuration that the
// router depends on cannot be parsed, so a bad trusted-proxy list fails at
// boot rather than on the first request.
func New(d Deps) (*Server, error) {
	trusted, err := d.Config.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	s := &Server{deps: d, router: chi.NewRouter(), trusted: trusted}
	s.routes()
	return s, nil
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.router }

// routes installs the middleware chain and mounts every route group.
// Order, outermost first, per Architecture.md §9.
func (s *Server) routes() {
	r := s.router

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(s.trusted))
	r.Use(middleware.Recoverer)
	r.Use(middleware.AccessLog(s.deps.Logger))
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(s.deps.Config.CORSOrigins))

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusNotFound, ErrorBody{
			Error:     "not_found",
			Message:   "Not found.",
			RequestID: middleware.RequestIDFrom(r.Context()),
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusMethodNotAllowed, ErrorBody{
			Error:     "method_not_allowed",
			Message:   "That method is not allowed here.",
			RequestID: middleware.RequestIDFrom(r.Context()),
		})
	})
}

// handleHealthz answers process liveness only. It touches no dependency, so a
// database outage does not cause an orchestrator to restart a healthy process.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers readiness: the database is reachable and migrated.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Health == nil {
		WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if err := s.deps.Health.Ping(r.Context()); err != nil {
		middleware.LoggerFrom(r.Context()).WarnContext(r.Context(), "not ready",
			slog.String("error", err.Error()))
		WriteJSON(w, r, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database",
		})
		return
	}
	WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
}
