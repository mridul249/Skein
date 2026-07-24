// Package httpapi wires the router, the middleware chain, and the handlers.
// Handlers are thin: decode, call a service, encode. Business logic lives in
// the domain packages and knows nothing about HTTP.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/config"
	"github.com/mridul60214/skein/internal/httpapi/handlers"
	"github.com/mridul60214/skein/internal/httpapi/httpx"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
	"github.com/mridul60214/skein/internal/web"
)

// jsonBodyLimit caps every JSON request body. Rules.md §2.13.
const jsonBodyLimit = 1 << 20 // 1 MiB

// Rate budgets, per minute per key. Rules.md §2.13.
const (
	authRatePerMin   = 5
	apiRatePerMin    = 300
	publicRatePerMin = 30
)

// Health reports process and dependency liveness.
type Health interface {
	// Ping returns nil when the database is reachable.
	Ping(ctx context.Context) error
}

// Deps is everything the HTTP layer needs. Wiring happens in main.go; nothing
// here reaches for a package-level singleton.
type Deps struct {
	Config   *config.Config
	Logger   *slog.Logger
	Health   Health
	Auth     *auth.Service
	Accounts *accounts.Service
}

// Server owns the router and the middleware chain.
type Server struct {
	deps    Deps
	router  chi.Router
	trusted []netip.Prefix
}

// New builds the server. It returns an error when configuration the router
// depends on cannot be parsed, so a bad trusted-proxy list fails at boot
// rather than on the first request.
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

	// The OAuth callback is mounted outside the JSON-body group and outside
	// the Auth middleware: it is a browser redirect from Google, so it can
	// carry neither a bearer token nor a JSON body. Its identity comes from
	// the single-use state row instead.
	s.mountOAuthCallback(r)

	r.Route("/api", func(api chi.Router) {
		api.Use(middleware.MaxJSONBody(jsonBodyLimit))
		s.mountAuth(api)
		s.mountAccounts(api)
	})

	s.mountUI(r)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, r, http.StatusNotFound, httpx.ErrorBody{
			Error:     "not_found",
			Message:   "Not found.",
			RequestID: middleware.RequestIDFrom(r.Context()),
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, r, http.StatusMethodNotAllowed, httpx.ErrorBody{
			Error:     "method_not_allowed",
			Message:   "That method is not allowed here.",
			RequestID: middleware.RequestIDFrom(r.Context()),
		})
	})
}

func (s *Server) mountAuth(api chi.Router) {
	if s.deps.Auth == nil {
		return
	}
	// Secure cookies require https. Development over plain http would have
	// the browser drop the refresh cookie without saying why.
	secureCookies := s.deps.Config.IsProduction() ||
		len(s.deps.Config.PublicURL) >= 5 && s.deps.Config.PublicURL[:5] == "https"

	h := handlers.NewAuth(s.deps.Auth, secureCookies)
	authLimiter := middleware.NewLimiter(authRatePerMin)

	api.Route("/auth", func(a chi.Router) {
		// Credential endpoints share one budget per client so an
		// attacker cannot spread guesses across register and login.
		a.Group(func(pub chi.Router) {
			pub.Use(middleware.RateLimit(authLimiter))
			pub.Post("/register", h.Register)
			pub.Post("/login", h.Login)
			pub.Post("/refresh", h.Refresh)
		})

		a.Post("/logout", h.Logout)

		a.Group(func(priv chi.Router) {
			priv.Use(middleware.Auth(s.deps.Auth, httpx.WriteError))
			priv.Use(middleware.RateLimit(middleware.NewLimiter(apiRatePerMin)))
			priv.Get("/me", h.Me)
		})
	})
}

func (s *Server) mountAccounts(api chi.Router) {
	if s.deps.Accounts == nil || s.deps.Auth == nil {
		return
	}
	h := handlers.NewAccounts(s.deps.Accounts, s.deps.Config.PublicURL)

	api.Group(func(g chi.Router) {
		g.Use(middleware.Auth(s.deps.Auth, httpx.WriteError))
		g.Use(middleware.RateLimit(middleware.NewLimiter(apiRatePerMin)))

		g.Get("/accounts", h.List)
		g.Get("/quota", h.Quota)
		g.Post("/accounts/google/connect", h.BeginGoogleConnect)
		g.Post("/accounts/{id}/sync", h.Sync)
		g.Delete("/accounts/{id}", h.Disconnect)
	})
}

// mountOAuthCallback mounts the provider redirect target. It carries its own
// rate limit because it is reachable without authentication.
func (s *Server) mountOAuthCallback(r chi.Router) {
	if s.deps.Accounts == nil {
		return
	}
	h := handlers.NewAccounts(s.deps.Accounts, s.deps.Config.PublicURL)
	r.Group(func(g chi.Router) {
		g.Use(middleware.RateLimit(middleware.NewLimiter(publicRatePerMin)))
		g.Get("/api/accounts/google/callback", h.GoogleCallback)
	})
}

// mountUI serves the embedded frontend. A binary built without the frontend
// bundle still serves the API; the UI routes are simply absent and the reason
// is logged once at boot rather than per request.
func (s *Server) mountUI(r chi.Router) {
	ui, err := web.Handler()
	if err != nil {
		s.deps.Logger.Warn("frontend bundle not embedded; serving API only",
			slog.String("reason", err.Error()))
		return
	}
	r.Group(func(g chi.Router) {
		g.Use(middleware.AppCSP)
		g.Handle("/*", ui)
	})
}

// handleHealthz answers process liveness only. It touches no dependency, so a
// database outage does not cause an orchestrator to restart a healthy process.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers readiness: the database is reachable.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Health == nil {
		httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if err := s.deps.Health.Ping(r.Context()); err != nil {
		middleware.LoggerFrom(r.Context()).WarnContext(r.Context(), "not ready",
			slog.String("error", err.Error()))
		httpx.WriteJSON(w, r, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database",
		})
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
}
