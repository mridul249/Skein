package handlers

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/capability"
	"github.com/mridul60214/skein/internal/files"
	"github.com/mridul60214/skein/internal/httpapi/httpx"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

// sniffLen is how many bytes http.DetectContentType looks at. Reading exactly
// this many and no more is what keeps the download path from buffering.
const sniffLen = 512

// Files serves the file, folder and content endpoints.
type Files struct {
	svc            *files.Service
	uploads        *middleware.ConcurrencyLimiter
	maxUploadBytes int64
	previewOrigin  string
	caps           *capability.Signer
}

// NewFiles builds the files handler group. caps may be nil, in which case
// minting a download URL is unavailable rather than unauthenticated.
func NewFiles(
	svc *files.Service,
	uploads *middleware.ConcurrencyLimiter,
	maxUploadBytes int64,
	previewOrigin string,
	caps *capability.Signer,
) *Files {
	return &Files{
		svc:            svc,
		uploads:        uploads,
		maxUploadBytes: maxUploadBytes,
		previewOrigin:  previewOrigin,
		caps:           caps,
	}
}

type fileResponse struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	FolderID    *string         `json:"folder_id"`
	SizeBytes   int64           `json:"size_bytes"`
	IsStriped   bool            `json:"is_striped"`
	IsEncrypted bool            `json:"is_encrypted"`
	Status      string          `json:"status"`
	SHA256      string          `json:"sha256,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	DeletedAt   *string         `json:"deleted_at,omitempty"`
	Shards      []shardResponse `json:"shards"`
}

type shardResponse struct {
	Index     int32   `json:"index"`
	AccountID *string `json:"account_id"`
	SizeBytes int64   `json:"size_bytes"`
	PlainSize int64   `json:"plain_size_bytes"`
	Offset    int64   `json:"plain_offset"`
}

type folderResponse struct {
	ID        string  `json:"id"`
	ParentID  *string `json:"parent_id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
}

// Upload handles POST /api/uploads.
//
// This is the endpoint the whole product is an argument about, so nothing here
// touches the body as a whole. multipart.Reader yields parts as a stream;
// NextPart returns when the part's headers have been read, and the part itself
// is an io.Reader that pulls from the socket on demand. The bytes go straight
// into the service, which copies them to the provider through a fixed buffer.
//
// Specifically absent: ParseMultipartForm, which spools the whole body to a
// temp file, and io.ReadAll, which spools it to RAM. Either would make a 2 GB
// upload cost 2 GB of something, and the memory-ceiling test exists to catch
// exactly that reappearing.
func (h *Files) Upload(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Rules.md §2.13: uploads are capped per user, so one client cannot
	// occupy every worker.
	release, ok := h.uploads.Acquire(userID.String())
	if !ok {
		httpx.WriteError(w, r, skerr.Public(skerr.ErrRateLimited,
			"Too many uploads at once. Wait for one to finish."))
		return
	}
	defer release()

	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
			"Send this as a multipart form."))
		return
	}

	// A declared Content-Length that already exceeds the ceiling is refused
	// before a single byte is read, so an oversized upload costs one round
	// trip rather than a full transfer.
	if r.ContentLength > 0 && r.ContentLength > h.maxUploadBytes+(1<<20) {
		httpx.WriteError(w, r, skerr.Public(skerr.ErrTooLarge,
			"That upload is larger than this server allows."))
		return
	}

	req := files.UploadRequest{UserID: userID}
	var uploaded *files.File

	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
				"That multipart body could not be read."))
			return
		}

		switch part.FormName() {
		case "folder_id":
			value, verr := readSmallPart(part)
			if verr != nil {
				httpx.WriteError(w, r, verr)
				return
			}
			if value != "" {
				id, uerr := uuid.Parse(value)
				if uerr != nil {
					httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
						"That is not a valid folder id."))
					return
				}
				req.FolderID = &id
			}

		case "size":
			value, verr := readSmallPart(part)
			if verr != nil {
				httpx.WriteError(w, r, verr)
				return
			}
			n, cerr := strconv.ParseInt(value, 10, 64)
			if cerr != nil || n < 0 {
				httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
					"Size must be a non-negative whole number."))
				return
			}
			req.Size = n

		case "name":
			value, verr := readSmallPart(part)
			if verr != nil {
				httpx.WriteError(w, r, verr)
				return
			}
			req.Name = value

		case "file":
			// The file part must come last: everything above has to be
			// known before the first content byte is read, because the
			// provider needs a declared size to open a session.
			if req.Name == "" {
				req.Name = part.FileName()
			}
			// The declared MIME is recorded as metadata and never
			// echoed into a response header. Rules.md §2.3.
			req.DeclaredMime = part.Header.Get("Content-Type")

			// Rules.md §2.7 and §2.13: the body is capped at the
			// configured ceiling regardless of what was declared, so a
			// client that lies about size still cannot write past it.
			limited := http.MaxBytesReader(w, part, h.maxUploadBytes+1)

			file, uerr := h.svc.Upload(r.Context(), req, limited)
			if uerr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(uerr, &maxErr) {
					httpx.WriteError(w, r, skerr.Public(skerr.ErrTooLarge,
						"That upload is larger than this server allows."))
					return
				}
				httpx.WriteError(w, r, uerr)
				return
			}
			uploaded = &file

		default:
			// Unknown parts are drained rather than ignored, or the
			// reader stalls on the next NextPart.
			if _, derr := io.Copy(io.Discard, io.LimitReader(part, 1<<20)); derr != nil {
				httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
					"That multipart body could not be read."))
				return
			}
		}

		if cerr := part.Close(); cerr != nil {
			middleware.LoggerFrom(r.Context()).Debug("closing multipart part",
				"error", cerr.Error())
		}
	}

	if uploaded == nil {
		httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation,
			"No file part was included."))
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, toFileResponse(*uploaded))
}

// readSmallPart reads a metadata field. It is bounded: a multipart field is
// not a place a client gets to allocate memory on the server's behalf.
func readSmallPart(p *multipart.Part) (string, error) {
	const maxFieldBytes = 4096
	b, err := io.ReadAll(io.LimitReader(p, maxFieldBytes+1))
	if err != nil {
		return "", skerr.Public(skerr.ErrValidation, "That form field could not be read.")
	}
	if len(b) > maxFieldBytes {
		return "", skerr.Public(skerr.ErrValidation, "That form field is too long.")
	}
	return strings.TrimSpace(string(b)), nil
}

// contentURLResponse is the minted grant. The URL is relative: it is always
// this origin, and building an absolute one from configuration is a way to
// hand a user a link to somebody else's host when that configuration is wrong.
type contentURLResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// ContentURL handles POST /api/files/{id}/content-url.
//
// It mints a short-lived capability URL for one file, so the browser can fetch
// the bytes itself — an <a download> streams to disk and holds nothing in JS
// memory, which is the fix for known issue #15. Minting requires a session:
// this route sits behind Auth, and that is the property that keeps a capability
// from being a way to reach content without one.
func (h *Files) ContentURL(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if h.caps == nil {
		httpx.WriteError(w, r, skerr.Public(skerr.ErrNotFound, "Downloads are not available."))
		return
	}

	// Ownership is proved before a credential is handed out, not only when
	// it is spent. Get filters by user_id, so a file belonging to someone
	// else is a not-found here rather than a grant that fails later.
	if _, err := h.svc.Get(r.Context(), userID, fileID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	expires := time.Now().Add(capability.TTL)
	q := h.caps.Sign(fileID, userID, expires)
	httpx.WriteJSON(w, r, http.StatusOK, contentURLResponse{
		URL:       "/api/files/" + fileID.String() + "/content?" + q.Encode(),
		ExpiresAt: expires.UTC().Format(time.RFC3339),
	})
}

// Content handles GET /api/files/{id}/content, with Range support.
//
// The response body is io.Copy from the shard reader straight to the
// ResponseWriter. Backpressure is automatic: a slow client slows the copy,
// which slows the read from the provider. Cancellation is automatic too, since
// r.Context() reaches the outbound request — a client that closes the tab
// tears down the provider read instead of leaving it running.
func (h *Files) Content(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Metadata first, so the size is known before the Range header is
	// parsed against it.
	meta, err := h.svc.Get(r.Context(), userID, fileID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	rng, err := parseRange(r.Header.Get("Range"), meta.SizeBytes)
	if err != nil {
		writeRangeNotSatisfiable(w, meta.SizeBytes)
		return
	}

	content, err := h.svc.Open(r.Context(), userID, fileID, rng)
	if err != nil {
		if errors.Is(err, files.ErrRangeNotSatisfiable) {
			writeRangeNotSatisfiable(w, meta.SizeBytes)
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	// Rules.md §2.11: closed on every path, including the error paths below.
	defer func() {
		if cerr := content.Body.Close(); cerr != nil {
			middleware.LoggerFrom(r.Context()).Warn("closing content reader",
				"error", cerr.Error())
		}
	}()

	// Sniff from the actual bytes, never from the client's declared type.
	// Exactly sniffLen bytes are held, and they are replayed into the copy
	// rather than re-fetched.
	head := make([]byte, sniffLen)
	n, rerr := io.ReadFull(content.Body, head)
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		httpx.WriteError(w, r, rerr)
		return
	}
	head = head[:n]

	// For a range starting mid-file the leading bytes are not the file's
	// header, so sniffing them would be meaningless. Those are always
	// served as an opaque stream.
	sniffed := "application/octet-stream"
	if content.Start == 0 {
		sniffed = http.DetectContentType(head)
	}
	h.writeContentHeaders(w, meta, content, sniffed)

	if r.Method == http.MethodHead {
		w.WriteHeader(statusFor(rng))
		return
	}
	w.WriteHeader(statusFor(rng))

	if _, werr := w.Write(head); werr != nil {
		return // client hung up; the deferred Close tears down the read
	}

	// Fixed buffer. Nothing on this path grows with file size.
	buf := make([]byte, storage.CopyBufferSize)
	if _, werr := io.CopyBuffer(w, content.Body, buf); werr != nil {
		// The status is already written, so this can only be logged.
		middleware.LoggerFrom(r.Context()).Debug("download interrupted",
			"file_id", fileID.String(), "error", werr.Error())
	}
}

func statusFor(rng *storage.ByteRange) int {
	if rng == nil {
		return http.StatusOK
	}
	return http.StatusPartialContent
}

func (h *Files) writeContentHeaders(w http.ResponseWriter, meta files.File, content *files.Content, sniffed string) {
	contentType, inline := disposition(sniffed)

	kind := "attachment"
	if inline {
		kind = "inline"
	}

	head := w.Header()
	head.Set("Content-Type", contentType)
	head.Set("Content-Length", strconv.FormatInt(content.Length, 10))
	head.Set("Content-Disposition", contentDispositionHeader(kind, meta.Name))
	head.Set("Accept-Ranges", "bytes")
	// Rules.md §2.3: always, without exception. It is what stops a browser
	// deciding for itself that octet-stream is really HTML.
	head.Set("X-Content-Type-Options", "nosniff")
	// Architecture.md §10: content responses get the strictest policy there
	// is. Nothing user-supplied may load a script, a frame, or a
	// stylesheet, even when it is being rendered inline.
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	head.Set("Cache-Control", "private, max-age=0, no-cache")

	if content.Start != 0 || content.Length != content.TotalSize {
		head.Set("Content-Range", "bytes "+
			strconv.FormatInt(content.Start, 10)+"-"+
			strconv.FormatInt(content.Start+content.Length-1, 10)+"/"+
			strconv.FormatInt(content.TotalSize, 10))
	}
}

// List handles GET /api/files.
func (h *Files) List(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	p, err := listParamsFrom(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	list, err := h.svc.List(r.Context(), userID, p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := make([]fileResponse, 0, len(list))
	for _, f := range list {
		out = append(out, toFileResponse(f))
	}

	body := map[string]any{"files": out}
	if len(list) == int(p.Limit) {
		last := list[len(list)-1]
		body["next_cursor"] = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID.String()
	}
	httpx.WriteJSON(w, r, http.StatusOK, body)
}

func listParamsFrom(r *http.Request) (files.ListParams, error) {
	q := r.URL.Query()
	p := files.ListParams{Limit: 50}

	if raw := q.Get("folder_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return p, skerr.Public(skerr.ErrValidation, "That is not a valid folder id.")
		}
		p.FolderID = &id
	}
	if raw := q.Get("limit"); raw != "" {
		// Parsed at 32 bits and bounded to [1, 200] before the
		// conversion, so the narrowing cannot lose information.
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 || n > 200 {
			return p, skerr.Public(skerr.ErrValidation, "Limit must be between 1 and 200.")
		}
		p.Limit = int32(n)
	}
	if raw := q.Get("cursor"); raw != "" {
		tsPart, idPart, ok := strings.Cut(raw, ",")
		if !ok {
			return p, skerr.Public(skerr.ErrValidation, "That cursor is not valid.")
		}
		ts, err := time.Parse(time.RFC3339Nano, tsPart)
		if err != nil {
			return p, skerr.Public(skerr.ErrValidation, "That cursor is not valid.")
		}
		id, err := uuid.Parse(idPart)
		if err != nil {
			return p, skerr.Public(skerr.ErrValidation, "That cursor is not valid.")
		}
		p.CursorCreatedAt = &ts
		p.CursorID = &id
	}
	return p, nil
}

// Get handles GET /api/files/{id}.
func (h *Files) Get(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	file, err := h.svc.Get(r.Context(), userID, fileID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toFileResponse(file))
}

type updateFileRequest struct {
	Name *string `json:"name"`
	// FolderID uses a double pointer so "absent" and "explicitly null" are
	// distinguishable: the first means leave it alone, the second means move
	// the file to the root.
	FolderID **string `json:"folder_id"`
}

// Update handles PATCH /api/files/{id}.
func (h *Files) Update(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req updateFileRequest
	if derr := decodeJSON(r, &req); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}

	folder, err := parseOptionalFolderID(req.FolderID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	file, err := h.svc.Rename(r.Context(), userID, fileID, req.Name, folder)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toFileResponse(file))
}

// Delete handles DELETE /api/files/{id}. It trashes by default; ?permanent=true
// deletes the provider objects too.
func (h *Files) Delete(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	if r.URL.Query().Get("permanent") == "true" {
		if derr := h.svc.Delete(r.Context(), userID, fileID); derr != nil {
			httpx.WriteError(w, r, derr)
			return
		}
		httpx.WriteNoContent(w)
		return
	}

	if derr := h.svc.Trash(r.Context(), userID, fileID); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	httpx.WriteNoContent(w)
}

// Restore handles POST /api/files/{id}/restore.
func (h *Files) Restore(w http.ResponseWriter, r *http.Request) {
	userID, fileID, err := userAndFileID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if rerr := h.svc.Restore(r.Context(), userID, fileID); rerr != nil {
		httpx.WriteError(w, r, rerr)
		return
	}
	httpx.WriteNoContent(w)
}

// Trash handles GET /api/trash.
func (h *Files) Trash(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	list, err := h.svc.ListTrashed(r.Context(), userID, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]fileResponse, 0, len(list))
	for _, f := range list {
		out = append(out, toFileResponse(f))
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"files": out})
}

type createFolderRequest struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id"`
}

// CreateFolder handles POST /api/folders.
func (h *Files) CreateFolder(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req createFolderRequest
	if derr := decodeJSON(r, &req); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}

	var parent *uuid.UUID
	if req.ParentID != nil && *req.ParentID != "" {
		id, perr := uuid.Parse(*req.ParentID)
		if perr != nil {
			httpx.WriteError(w, r, skerr.Public(skerr.ErrValidation, "That is not a valid folder id."))
			return
		}
		parent = &id
	}

	folder, err := h.svc.CreateFolder(r.Context(), userID, parent, req.Name)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, toFolderResponse(folder))
}

// ListFolders handles GET /api/folders.
func (h *Files) ListFolders(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	list, err := h.svc.ListFolders(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]folderResponse, 0, len(list))
	for _, f := range list {
		out = append(out, toFolderResponse(f))
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"folders": out})
}

type updateFolderRequest struct {
	Name     *string  `json:"name"`
	ParentID **string `json:"parent_id"`
}

// UpdateFolder handles PATCH /api/folders/{id}.
func (h *Files) UpdateFolder(w http.ResponseWriter, r *http.Request) {
	userID, folderID, err := userAndPathID(r, "That is not a valid folder id.")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req updateFolderRequest
	if derr := decodeJSON(r, &req); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}

	parent, err := parseOptionalFolderID(req.ParentID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	folder, err := h.svc.UpdateFolder(r.Context(), userID, folderID, req.Name, parent)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toFolderResponse(folder))
}

// DeleteFolder handles DELETE /api/folders/{id}.
func (h *Files) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	userID, folderID, err := userAndPathID(r, "That is not a valid folder id.")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if derr := h.svc.TrashFolder(r.Context(), userID, folderID); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	httpx.WriteNoContent(w)
}

// parseOptionalFolderID turns the three-state JSON field into the three-state
// Go value the service expects.
func parseOptionalFolderID(raw **string) (**uuid.UUID, error) {
	if raw == nil {
		return nil, nil // absent: leave the folder alone
	}
	if *raw == nil || **raw == "" {
		var root *uuid.UUID // explicit null: move to the root
		return &root, nil
	}
	id, err := uuid.Parse(**raw)
	if err != nil {
		return nil, skerr.Public(skerr.ErrValidation, "That is not a valid folder id.")
	}
	target := &id
	return &target, nil
}

func userAndFileID(r *http.Request) (userID, fileID uuid.UUID, err error) {
	return userAndPathID(r, "That is not a valid file id.")
}

func userAndPathID(r *http.Request, msg string) (userID, id uuid.UUID, err error) {
	userID, err = middleware.MustUserID(r.Context())
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		return uuid.Nil, uuid.Nil, skerr.Public(skerr.ErrValidation, "%s", msg)
	}
	return userID, id, nil
}

func toFileResponse(f files.File) fileResponse {
	out := fileResponse{
		ID:          f.ID.String(),
		Name:        f.Name,
		SizeBytes:   f.SizeBytes,
		IsStriped:   f.IsStriped,
		IsEncrypted: f.IsEncrypted,
		Status:      f.Status,
		CreatedAt:   f.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   f.UpdatedAt.UTC().Format(time.RFC3339),
		Shards:      make([]shardResponse, 0, len(f.Shards)),
	}
	if f.FolderID != nil {
		s := f.FolderID.String()
		out.FolderID = &s
	}
	if f.DeletedAt != nil {
		s := f.DeletedAt.UTC().Format(time.RFC3339)
		out.DeletedAt = &s
	}
	if len(f.ContentSHA) > 0 {
		out.SHA256 = hexString(f.ContentSHA)
	}
	for _, sh := range f.Shards {
		item := shardResponse{
			Index:     sh.Index,
			SizeBytes: sh.SizeBytes,
			PlainSize: sh.PlainSize,
			Offset:    sh.PlainOffset,
		}
		if sh.AccountID != nil {
			s := sh.AccountID.String()
			item.AccountID = &s
		}
		out.Shards = append(out.Shards, item)
	}
	return out
}

func toFolderResponse(f files.Folder) folderResponse {
	out := folderResponse{
		ID:        f.ID.String(),
		Name:      f.Name,
		CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339),
	}
	if f.ParentID != nil {
		s := f.ParentID.String()
		out.ParentID = &s
	}
	return out
}

func hexString(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
}
