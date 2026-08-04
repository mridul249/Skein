// Package handlers holds the HTTP handlers. Each one decodes a request, calls
// a service, and encodes the result. No business logic lives here, and no
// service below here knows what an http.Request is.
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mridul249/Skein/internal/auth"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
	"github.com/mridul249/Skein/internal/skerr"
)

// RefreshCookieName is the httpOnly cookie the refresh token lives in.
// Rules.md §2.15: it never reaches JavaScript, and the access token never
// reaches storage.
const RefreshCookieName = "skein_refresh"

// refreshCookiePath scopes the cookie to the endpoints that consume it, so it
// is not attached to every file download.
const refreshCookiePath = "/api/auth"

// Auth serves the authentication endpoints.
type Auth struct {
	svc    *auth.Service
	secure bool
}

// NewAuth builds the auth handler group. secure controls the Secure attribute
// on the refresh cookie; it is false only for plain-HTTP local development,
// where the browser would otherwise drop the cookie silently.
func NewAuth(svc *auth.Service, secure bool) *Auth {
	return &Auth{svc: svc, secure: secure}
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	CreatedAt     string `json:"created_at"`
}

type sessionResponse struct {
	AccessToken  string       `json:"access_token"`
	ExpiresAt    string       `json:"expires_at"`
	ExpiresInSec int          `json:"expires_in"`
	User         userResponse `json:"user"`
}

// Register handles POST /api/auth/register.
func (h *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pair, err := h.svc.Register(r.Context(), req.Email, req.Password, metaFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.writeSession(w, r, http.StatusCreated, pair)
}

// Login handles POST /api/auth/login.
func (h *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pair, err := h.svc.Login(r.Context(), req.Email, req.Password, metaFrom(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.writeSession(w, r, http.StatusOK, pair)
}

// Refresh handles POST /api/auth/refresh. The token is read from the cookie
// only — accepting it from a JSON body would mean it had to live somewhere
// JavaScript can reach.
func (h *Auth) Refresh(w http.ResponseWriter, r *http.Request) {
	token := refreshCookieValue(r)
	pair, err := h.svc.Refresh(r.Context(), token, metaFrom(r))
	if err != nil {
		// Whatever the reason, this browser's cookie is now worthless.
		// Clearing it stops a revoked family from being replayed on
		// every subsequent page load.
		h.clearRefreshCookie(w)
		httpx.WriteError(w, r, err)
		return
	}
	h.writeSession(w, r, http.StatusOK, pair)
}

// Logout handles POST /api/auth/logout. It always succeeds: signing out twice,
// or with an already-dead token, is not an error worth surfacing.
func (h *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(r.Context(), refreshCookieValue(r), metaFrom(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.clearRefreshCookie(w)
	httpx.WriteNoContent(w)
}

// Me handles GET /api/auth/me.
func (h *Auth) Me(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, err := h.svc.Me(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toUserResponse(user))
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles POST /api/auth/change-password.
//
// Mounted behind the credential rate limiter rather than the general API one:
// it verifies a password, which makes it an online guessing oracle, and it
// shares that budget with register and login so an attacker cannot spread
// guesses across endpoints.
//
// 204 rather than a session payload — the current session is unaffected, and
// no other device is signed out (see auth.ChangePassword and known issue #18).
func (h *Auth) ChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var req changePasswordRequest
	if derr := decodeJSON(r, &req); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	if cerr := h.svc.ChangePassword(r.Context(), userID,
		req.CurrentPassword, req.NewPassword, metaFrom(r)); cerr != nil {
		httpx.WriteError(w, r, cerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Auth) writeSession(w http.ResponseWriter, r *http.Request, status int, pair auth.TokenPair) {
	h.setRefreshCookie(w, pair.RefreshToken, pair.RefreshTTL)
	httpx.WriteJSON(w, r, status, sessionResponse{
		AccessToken:  pair.AccessToken,
		ExpiresAt:    pair.AccessExpiry.UTC().Format(time.RFC3339),
		ExpiresInSec: int(time.Until(pair.AccessExpiry).Seconds()),
		User:         toUserResponse(pair.User),
	})
}

func (h *Auth) setRefreshCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   h.secure,
		// Strict, not Lax: nothing about this app is reached by
		// following a link from elsewhere, so there is no flow that a
		// cross-site GET needs to keep working.
		SameSite: http.SameSiteStrictMode,
	})
}

func (h *Auth) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func refreshCookieValue(r *http.Request) string {
	c, err := r.Cookie(RefreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func toUserResponse(u auth.User) userResponse {
	return userResponse{
		ID:            u.ID.String(),
		Email:         u.Email,
		EmailVerified: u.EmailVerifiedAt != nil,
		CreatedAt:     u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func metaFrom(r *http.Request) auth.RequestMeta {
	return auth.RequestMeta{
		IP:        auth.ParseIP(middleware.RealIPFrom(r.Context())),
		UserAgent: r.UserAgent(),
	}
}

// decodeJSON reads exactly one JSON object from the body. Unknown fields are
// rejected so a typo in a client field name fails loudly instead of silently
// defaulting, and trailing content is rejected so a smuggled second object
// cannot ride along.
func decodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType := strings.TrimSpace(strings.Split(ct, ";")[0])
		if !strings.EqualFold(mediaType, "application/json") {
			return skerr.Public(skerr.ErrValidation, "Send this as application/json.")
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return skerr.Public(skerr.ErrTooLarge, "That request body is too large.")
		}
		return skerr.Public(skerr.ErrValidation, "The request body is not valid JSON.")
	}
	if dec.More() {
		return skerr.Public(skerr.ErrValidation, "Send a single JSON object.")
	}
	return nil
}
