package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/httpapi/httpx"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
	"github.com/mridul60214/skein/internal/skerr"
)

// DesktopConnector runs a whole desktop OAuth attempt (system browser,
// loopback listener, PKCE exchange) and returns once it succeeds or fails.
// Satisfied by *desktopoauth.Connector; declared here rather than imported
// from that package so the server build never links it — cmd/skein imports
// this handlers package, and desktopoauth pulls in github.com/pkg/browser
// and an HTTP listener the headless server has no use for. Only
// cmd/skein-desktop constructs a concrete value and wires it in.
type DesktopConnector interface {
	Connect(ctx context.Context, userID uuid.UUID) (accounts.Account, error)
}

// Accounts serves the connected-drive endpoints.
type Accounts struct {
	svc *accounts.Service
	// appBaseURL is where the OAuth callback sends the browser back to. It
	// is server configuration, never a value taken from the request, so a
	// crafted callback cannot turn this into an open redirect.
	appBaseURL string
	// desktop is nil on the server build. When set, BeginGoogleConnect runs
	// the desktop flow (system browser + loopback listener) instead of
	// returning a URL for the SPA to navigate to.
	desktop DesktopConnector
}

// NewAccounts builds the accounts handler group.
func NewAccounts(svc *accounts.Service, appBaseURL string) *Accounts {
	return &Accounts{svc: svc, appBaseURL: strings.TrimRight(appBaseURL, "/")}
}

// NewDesktopAccounts builds the accounts handler group for the desktop
// build: BeginGoogleConnect runs desktop.Connect end to end instead of
// returning an authorize_url, and GoogleCallback is never mounted (there is
// no server-hosted callback route on this path).
func NewDesktopAccounts(svc *accounts.Service, desktop DesktopConnector) *Accounts {
	return &Accounts{svc: svc, desktop: desktop}
}

type accountResponse struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	LastError   string `json:"last_error,omitempty"`
	Ordinal     int32  `json:"ordinal"`
	CreatedAt   string `json:"created_at"`
}

type capacityResponse struct {
	accountResponse
	TotalBytes    int64   `json:"total_bytes"`
	UsedBytes     int64   `json:"used_bytes"`
	ReservedBytes int64   `json:"reserved_bytes"`
	FreeBytes     int64   `json:"free_bytes"`
	LastSyncedAt  *string `json:"last_synced_at"`
}

type quotaResponse struct {
	TotalBytes int64              `json:"total_bytes"`
	UsedBytes  int64              `json:"used_bytes"`
	FreeBytes  int64              `json:"free_bytes"`
	Drives     []capacityResponse `json:"drives"`
}

// List handles GET /api/accounts.
func (h *Accounts) List(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	list, err := h.svc.List(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := make([]accountResponse, 0, len(list))
	for _, a := range list {
		out = append(out, toAccountResponse(a))
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"accounts": out})
}

// Quota handles GET /api/quota: per-drive and pooled capacity.
func (h *Accounts) Quota(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pool, err := h.svc.PoolFor(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	drives := make([]capacityResponse, 0, len(pool.Accounts))
	for _, c := range pool.Accounts {
		var synced *string
		if c.LastSyncedAt != nil {
			s := c.LastSyncedAt.UTC().Format(time.RFC3339)
			synced = &s
		}
		drives = append(drives, capacityResponse{
			accountResponse: toAccountResponse(c.Account),
			TotalBytes:      c.TotalBytes,
			UsedBytes:       c.UsedBytes,
			ReservedBytes:   c.ReservedBytes,
			FreeBytes:       c.FreeBytes(),
			LastSyncedAt:    synced,
		})
	}

	httpx.WriteJSON(w, r, http.StatusOK, quotaResponse{
		TotalBytes: pool.TotalBytes,
		UsedBytes:  pool.UsedBytes,
		FreeBytes:  pool.FreeBytes,
		Drives:     drives,
	})
}

// BeginGoogleConnect handles POST /api/accounts/google/connect.
//
// On the server build it returns a URL rather than redirecting, because the
// caller is a fetch from the SPA and a 302 on an XHR is not something the
// page can act on; the frontend navigates the whole window to it and Google
// redirects back to GoogleCallback.
//
// On the desktop build (h.desktop set) there is no page navigation to send
// anywhere: the request itself runs the whole attempt — opens the system
// browser, waits on the loopback callback, completes the exchange — and
// blocks until it finishes, bounded by accounts.OAuthStateTTL. It responds
// with the connected drive instead of a URL, and authorize_url is absent
// rather than empty so the frontend can branch on its presence.
func (h *Accounts) BeginGoogleConnect(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	if h.desktop != nil {
		acct, err := h.desktop.Connect(r.Context(), userID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, r, http.StatusOK, toAccountResponse(acct))
		return
	}

	authURL, err := h.svc.BeginGoogleConnect(r.Context(), userID, "/settings")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"authorize_url": authURL})
}

// GoogleCallback handles GET /api/accounts/google/callback.
//
// This endpoint is reached by a browser redirect from Google, so it is not
// behind the Auth middleware — the identity comes from the single-use state
// row, not from a bearer token the redirect could not carry.
func (h *Accounts) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// The user declined, or Google refused. Send them back with a message
	// rather than rendering a raw provider error.
	if e := q.Get("error"); e != "" {
		h.redirectToSettings(w, r, "error", "Connection cancelled.")
		return
	}

	_, _, err := h.svc.CompleteGoogleConnect(r.Context(), q.Get("state"), q.Get("code"))
	if err != nil {
		middleware.LoggerFrom(r.Context()).Warn("google connect failed")

		msg := "Could not connect that drive. Try again."
		var pub *skerr.PublicError
		if errors.As(err, &pub) && pub.Message != "" {
			msg = pub.Message
		}
		h.redirectToSettings(w, r, "error", msg)
		return
	}

	h.redirectToSettings(w, r, "connected", "")
}

// redirectToSettings sends the browser back into the SPA. The destination is
// built from server configuration and a fixed path, so nothing the caller
// supplies can steer it elsewhere.
func (h *Accounts) redirectToSettings(w http.ResponseWriter, r *http.Request, status, message string) {
	target := h.appBaseURL + "/settings"
	params := url.Values{}
	params.Set("drive", status)
	if message != "" {
		params.Set("message", message)
	}
	http.Redirect(w, r, target+"?"+params.Encode(), http.StatusSeeOther)
}

// Sync handles POST /api/accounts/{id}/sync.
func (h *Accounts) Sync(w http.ResponseWriter, r *http.Request) {
	userID, accountID, err := userAndAccountID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.svc.SyncUserAccount(r.Context(), userID, accountID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pool, err := h.svc.PoolFor(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	for _, c := range pool.Accounts {
		if c.Account.ID == accountID {
			httpx.WriteJSON(w, r, http.StatusOK, capacityResponse{
				accountResponse: toAccountResponse(c.Account),
				TotalBytes:      c.TotalBytes,
				UsedBytes:       c.UsedBytes,
				ReservedBytes:   c.ReservedBytes,
				FreeBytes:       c.FreeBytes(),
			})
			return
		}
	}
	httpx.WriteError(w, r, skerr.ErrNotFound)
}

// Disconnect handles DELETE /api/accounts/{id}.
func (h *Accounts) Disconnect(w http.ResponseWriter, r *http.Request) {
	userID, accountID, err := userAndAccountID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.svc.Disconnect(r.Context(), userID, accountID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteNoContent(w)
}

func userAndAccountID(r *http.Request) (userID, accountID uuid.UUID, err error) {
	userID, err = middleware.MustUserID(r.Context())
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	// Rules.md §2.6: a malformed path parameter is a 400, never a panic.
	accountID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		return uuid.Nil, uuid.Nil, skerr.Public(skerr.ErrValidation, "That is not a valid drive id.")
	}
	return userID, accountID, nil
}

func toAccountResponse(a accounts.Account) accountResponse {
	return accountResponse{
		ID:          a.ID.String(),
		Kind:        string(a.Kind),
		Email:       a.Email,
		DisplayName: a.DisplayName,
		Status:      a.Status,
		LastError:   a.LastError,
		Ordinal:     a.Ordinal,
		CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
	}
}
