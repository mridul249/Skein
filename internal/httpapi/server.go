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
	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/capability"
	"github.com/mridul60214/skein/internal/config"
	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/files"
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

	// contentRatePerMin governs the byte-serving routes only, and is separate
	// from apiRatePerMin because the traffic shape is different (known issue
	// #25). A download is one request; a <video> being scrubbed is many range
	// requests, and a page of image previews opens one per thumbnail.
	//
	// Note the limiter's burst equals its per-minute figure, so a full bucket
	// already absorbs a scrub burst outright — the value below is really the
	// *sustained* floor, 10 requests per second. That is what a preview needs
	// and it is still a real bound: the route is scoped to one file per
	// capability grant, and a grant lives 15 minutes.
	contentRatePerMin = 600
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
	Files    *files.Service
	// Keyring signs content capability URLs. Without it the download
	// endpoint still works for a bearer token; only minting is unavailable.
	Keyring *skcrypto.Keyring
}

// Server owns the router and the middleware chain.
type Server struct {
	deps    Deps
	router  chi.Router
	trusted []netip.Prefix

	// uploadSlots is shared by every route that starts an upload, so the
	// per-user concurrency budget is one budget rather than one per mount.
	uploadSlots *middleware.ConcurrencyLimiter

	// caps signs and verifies content capability URLs. Nil when no keyring
	// was wired.
	caps *capability.Signer
}

// New builds the server. It returns an error when configuration the router
// depends on cannot be parsed, so a bad trusted-proxy list fails at boot
// rather than on the first request.
func New(d Deps) (*Server, error) {
	trusted, err := d.Config.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	s := &Server{
		deps:        d,
		router:      chi.NewRouter(),
		trusted:     trusted,
		uploadSlots: middleware.NewConcurrencyLimiter(d.Config.MaxUploadsPerUser),
	}
	if d.Keyring != nil {
		signer, serr := capability.NewSigner(d.Keyring)
		if serr != nil {
			return nil, serr
		}
		s.caps = signer
	}
	s.routes()
	return s, nil
}

// capVerifier returns the capability verifier as an interface, or a genuinely
// nil one. Assigning a nil *capability.Signer straight into the interface would
// produce a non-nil interface holding a nil pointer, and the middleware's
// nil check would pass while every call panicked.
func (s *Server) capVerifier() middleware.CapabilityVerifier {
	if s.caps == nil {
		return nil
	}
	return s.caps
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

	// Streaming routes are mounted first and outside the JSON group. The
	// 1 MiB MaxBytesReader that every JSON endpoint wants would truncate a
	// file upload at the first megabyte, so the two cannot share a group;
	// the streaming routes enforce the configured upload ceiling instead.
	s.mountStreaming(r)

	r.Route("/api", func(api chi.Router) {
		api.Use(middleware.MaxJSONBody(jsonBodyLimit))
		s.mountAuth(api)
		s.mountAccounts(api)
		s.mountFiles(api)
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

func (s *Server) mountFiles(api chi.Router) {
	if s.deps.Files == nil || s.deps.Auth == nil {
		return
	}
	h := s.filesHandler()

	api.Group(func(g chi.Router) {
		g.Use(middleware.Auth(s.deps.Auth, httpx.WriteError))
		g.Use(middleware.RateLimit(middleware.NewLimiter(apiRatePerMin)))

		g.Get("/files", h.List)
		g.Get("/files/{id}", h.Get)
		// Minting sits here, inside the authenticated JSON group, and not
		// beside the streaming routes: requiring a session to obtain a
		// capability is what stops the capability being a way around one.
		g.Post("/files/{id}/content-url", h.ContentURL)
		g.Patch("/files/{id}", h.Update)
		g.Delete("/files/{id}", h.Delete)
		g.Post("/files/{id}/restore", h.Restore)
		g.Get("/trash", h.Trash)

		g.Get("/folders", h.ListFolders)
		g.Post("/folders", h.CreateFolder)
		g.Patch("/folders/{id}", h.UpdateFolder)
		g.Delete("/folders/{id}", h.DeleteFolder)
	})

}

// mountStreaming mounts the routes that carry file bytes. They live outside
// the JSON group so no body cap applies; the upload ceiling and the per-user
// concurrency limit are enforced inside the handler.
//
// Content and upload no longer share a middleware. Content accepts a signed
// capability URL as well as a bearer token, because a browser-managed download
// cannot set a header; upload stays bearer-only, so the second credential can
// never write anything.
func (s *Server) mountStreaming(r chi.Router) {
	if s.deps.Files == nil || s.deps.Auth == nil {
		return
	}
	h := s.filesHandler()

	r.Group(func(g chi.Router) {
		g.Use(middleware.ContentAuth(s.deps.Auth, s.capVerifier(), contentFileID, httpx.WriteError))
		// Rate limited, unlike before: a capability URL makes this route
		// reachable without a session, and the limiter keys on the user
		// the grant resolves to. Sized for range traffic rather than for
		// JSON calls — see contentRatePerMin.
		g.Use(middleware.RateLimit(middleware.NewLimiter(contentRatePerMin)))
		g.Get("/api/files/{id}/content", h.Content)
		g.Head("/api/files/{id}/content", h.Content)
	})

	r.Group(func(g chi.Router) {
		g.Use(middleware.Auth(s.deps.Auth, httpx.WriteError))
		g.Post("/api/uploads", h.Upload)
	})
}

// contentFileID pulls the requested file from the route so the capability
// signature is checked against the file actually being served.
func contentFileID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) filesHandler() *handlers.Files {
	return handlers.NewFiles(
		s.deps.Files,
		s.uploadSlots,
		s.deps.Config.MaxUploadBytes,
		s.deps.Config.PreviewOrigin,
		s.caps,
	)
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
