package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mridul249/Skein/internal/storage"
)

// The Drive endpoints are package-level constants, so the tests point the
// backend's transport at a stub server instead of rewriting URLs. Every
// request the backend makes is answered locally: no test here touches the
// network.

type stubDrive struct {
	t *testing.T

	// objects holds committed uploads by id.
	objects map[string][]byte

	// sessionBody records what arrived on the resumable PUT.
	uploadedBytes int64

	// folderID is the app folder shards should be parented into.
	folderID string
	// sessionParents records the parents the resumable session declared.
	sessionParents []string

	// knobs
	sessionStatus  int
	uploadStatus   int
	omitLocation   bool
	reportSize     *int64 // overrides the size in the upload response
	quotaLimit     string
	quotaUsage     string
	quotaStatus    int
	deleteStatus   int
	getStatus      int
	forbiddenBody  string
	deleteRequests []string
}

func newStub(t *testing.T) *stubDrive {
	return &stubDrive{
		t:             t,
		objects:       map[string][]byte{},
		sessionStatus: http.StatusOK,
		uploadStatus:  http.StatusOK,
		quotaLimit:    "16106127360", // 15 GiB
		quotaUsage:    "1073741824",  // 1 GiB
		quotaStatus:   http.StatusOK,
		deleteStatus:  http.StatusNoContent,
		getStatus:     0, // 0 means "serve the object"
	}
}

// backend returns a Backend whose transport routes every request to the stub.
func (s *stubDrive) backend() *Backend {
	srv := httptest.NewServer(s)
	s.t.Cleanup(srv.Close)

	client := &http.Client{Transport: &rewriteTransport{base: srv.URL}}
	return New(client, s.folderID)
}

// rewriteTransport sends every request to the stub server, preserving the path
// and query so the handler can tell the endpoints apart.
type rewriteTransport struct{ base string }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := rt.base + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	u, err := req.URL.Parse(target)
	if err != nil {
		return nil, err
	}
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func (s *stubDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/about"):
		s.handleQuota(w)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/"):
		s.handleSessionStart(w, r)
	case r.Method == http.MethodPut:
		s.handleUpload(w, r)
	case r.Method == http.MethodDelete:
		s.handleDelete(w, r)
	case r.Method == http.MethodGet:
		s.handleGet(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *stubDrive) handleQuota(w http.ResponseWriter) {
	if s.quotaStatus != http.StatusOK {
		w.WriteHeader(s.quotaStatus)
		_, _ = w.Write([]byte(s.forbiddenBody))
		return
	}
	limit := ""
	if s.quotaLimit != "" {
		limit = fmt.Sprintf(`"limit":%q,`, s.quotaLimit)
	}
	_, _ = fmt.Fprintf(w, `{"storageQuota":{%s"usage":%q}}`, limit, s.quotaUsage)
}

func (s *stubDrive) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if s.sessionStatus != http.StatusOK {
		w.WriteHeader(s.sessionStatus)
		_, _ = w.Write([]byte(s.forbiddenBody))
		return
	}
	// The declared length must reach Drive as a header, or the resumable
	// protocol cannot bound the session.
	if r.Header.Get("X-Upload-Content-Length") == "" {
		s.t.Error("session start did not declare X-Upload-Content-Length")
	}
	// Capture the parents so a test can assert shards are filed, not
	// dumped at root.
	var meta struct {
		Parents []string `json:"parents"`
	}
	if body, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20)); rerr == nil {
		_ = json.Unmarshal(body, &meta)
	}
	s.sessionParents = meta.Parents

	if !s.omitLocation {
		w.Header().Set("Location", "/upload/session/abc")
	}
	w.WriteHeader(http.StatusOK)
}

func (s *stubDrive) handleUpload(w http.ResponseWriter, r *http.Request) {
	// Chunked bodies are rejected by the real endpoint; assert the backend
	// sets ContentLength so net/http does not fall back to chunked.
	if r.ContentLength < 0 {
		s.t.Error("upload body was sent with an unknown Content-Length")
	}

	body, err := io.ReadAll(r.Body)
	s.uploadedBytes = int64(len(body))
	if err != nil {
		// A body shorter than its declared Content-Length aborts the
		// request mid-read. Real Drive rejects it; so does the stub.
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if s.uploadStatus != http.StatusOK {
		w.WriteHeader(s.uploadStatus)
		_, _ = w.Write([]byte(s.forbiddenBody))
		return
	}

	id := "file-1"
	s.objects[id] = body

	size := int64(len(body))
	if s.reportSize != nil {
		size = *s.reportSize
	}
	_, _ = fmt.Fprintf(w, `{"id":%q,"size":%q}`, id, strconv.FormatInt(size, 10))
}

func (s *stubDrive) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	s.deleteRequests = append(s.deleteRequests, id)
	delete(s.objects, id)
	w.WriteHeader(s.deleteStatus)
}

func (s *stubDrive) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.getStatus != 0 {
		w.WriteHeader(s.getStatus)
		_, _ = w.Write([]byte(s.forbiddenBody))
		return
	}
	id := lastPathSegment(r.URL.Path)
	obj, ok := s.objects[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		var start, end int64
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if start >= int64(len(obj)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(obj)) {
			end = int64(len(obj)) - 1
		}
		slice := obj[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj)))
		w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(slice)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
	_, _ = w.Write(obj)
}

func lastPathSegment(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return parts[len(parts)-1]
}

func TestPutStreamsAndReturnsARef(t *testing.T) {
	s := newStub(t)
	b := s.backend()

	data := bytes.Repeat([]byte("skein"), 5000)
	ref, err := b.Put(context.Background(), bytes.NewReader(data), storage.ObjectSpec{
		Name: "skein-abc-0000.bin",
		Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if ref.ProviderID != "file-1" {
		t.Errorf("ProviderID = %q", ref.ProviderID)
	}
	if ref.Size != int64(len(data)) {
		t.Errorf("ref.Size = %d, want %d", ref.Size, len(data))
	}
	if s.uploadedBytes != int64(len(data)) {
		t.Errorf("drive received %d bytes, want %d", s.uploadedBytes, len(data))
	}
	if !bytes.Equal(s.objects["file-1"], data) {
		t.Error("the bytes stored at drive differ from what was sent")
	}
}

// Rules.md §2.7: a declared size that does not match what arrived fails, and
// the half-written object is deleted rather than recorded.
func TestPutRejectsAndCleansUpOnSizeMismatch(t *testing.T) {
	s := newStub(t)
	wrong := int64(999999)
	s.reportSize = &wrong
	b := s.backend()

	data := []byte("twelve bytes")
	_, err := b.Put(context.Background(), bytes.NewReader(data), storage.ObjectSpec{
		Name: "obj.bin",
		Size: int64(len(data)),
	})
	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put() = %v, want ErrSizeMismatch", err)
	}
	if len(s.deleteRequests) != 1 || s.deleteRequests[0] != "file-1" {
		t.Errorf("the mismatched object was not deleted: %v", s.deleteRequests)
	}
}

func TestPutRejectsShortBody(t *testing.T) {
	s := newStub(t)
	b := s.backend()

	// Declare 100 bytes, supply 10.
	_, err := b.Put(context.Background(), bytes.NewReader(make([]byte, 10)),
		storage.ObjectSpec{Name: "obj.bin", Size: 100})
	if err == nil {
		t.Fatal("Put() succeeded with a short body")
	}
}

func TestPutRejectsNegativeSize(t *testing.T) {
	b := newStub(t).backend()
	_, err := b.Put(context.Background(), bytes.NewReader(nil),
		storage.ObjectSpec{Name: "obj.bin", Size: -1})
	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put() = %v, want ErrSizeMismatch", err)
	}
}

func TestPutFailsWhenSessionCannotOpen(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*stubDrive)
		wantErr error
	}{
		{
			name:    "unauthorized",
			setup:   func(s *stubDrive) { s.sessionStatus = http.StatusUnauthorized },
			wantErr: storage.ErrUnauthorized,
		},
		{
			name: "quota exceeded reported as 403",
			setup: func(s *stubDrive) {
				s.sessionStatus = http.StatusForbidden
				s.forbiddenBody = `{"error":{"errors":[{"reason":"storageQuotaExceeded"}]}}`
			},
			wantErr: storage.ErrQuota,
		},
		{
			name:    "insufficient storage",
			setup:   func(s *stubDrive) { s.sessionStatus = http.StatusInsufficientStorage },
			wantErr: storage.ErrQuota,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			tc.setup(s)
			b := s.backend()

			_, err := b.Put(context.Background(), bytes.NewReader([]byte("x")),
				storage.ObjectSpec{Name: "obj.bin", Size: 1})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Put() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPutFailsWhenSessionURIIsMissing(t *testing.T) {
	s := newStub(t)
	s.omitLocation = true
	b := s.backend()

	_, err := b.Put(context.Background(), bytes.NewReader([]byte("x")),
		storage.ObjectSpec{Name: "obj.bin", Size: 1})
	if err == nil || !strings.Contains(err.Error(), "no Location") {
		t.Fatalf("Put() = %v, want a missing-Location error", err)
	}
}

func TestPutIsCancelledByContext(t *testing.T) {
	s := newStub(t)
	b := s.backend()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.Put(ctx, bytes.NewReader([]byte("x")),
		storage.ObjectSpec{Name: "obj.bin", Size: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put() = %v, want context.Canceled", err)
	}
}

func TestGetWholeObjectAndRange(t *testing.T) {
	s := newStub(t)
	b := s.backend()

	data := []byte("0123456789abcdefghij")
	ref, err := b.Put(context.Background(), bytes.NewReader(data),
		storage.ObjectSpec{Name: "obj.bin", Size: int64(len(data))})
	if err != nil {
		t.Fatalf("Put() = %v", err)
	}

	t.Run("whole object", func(t *testing.T) {
		rc, n, err := b.Get(context.Background(), ref, nil)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		defer func() { _ = rc.Close() }()
		if n != int64(len(data)) {
			t.Errorf("length = %d, want %d", n, len(data))
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Error("bytes differ")
		}
	})

	t.Run("range", func(t *testing.T) {
		rc, n, err := b.Get(context.Background(), ref, &storage.ByteRange{Start: 10, Length: 5})
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		defer func() { _ = rc.Close() }()
		if n != 5 {
			t.Errorf("length = %d, want 5", n)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "abcde" {
			t.Errorf("got %q, want %q", got, "abcde")
		}
	})

	t.Run("range past the end", func(t *testing.T) {
		_, _, err := b.Get(context.Background(), ref, &storage.ByteRange{Start: 1000, Length: 5})
		if !errors.Is(err, storage.ErrRangeNotSat) {
			t.Fatalf("Get() = %v, want ErrRangeNotSat", err)
		}
	})

	t.Run("invalid range", func(t *testing.T) {
		for _, rng := range []storage.ByteRange{{Start: -1, Length: 5}, {Start: 0, Length: 0}} {
			if _, _, err := b.Get(context.Background(), ref, &rng); !errors.Is(err, storage.ErrRangeNotSat) {
				t.Errorf("Get(%+v) = %v, want ErrRangeNotSat", rng, err)
			}
		}
	})
}

func TestGetMissingObject(t *testing.T) {
	b := newStub(t).backend()

	_, _, err := b.Get(context.Background(), storage.ObjectRef{ProviderID: "nope"}, nil)
	if !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("Get() = %v, want ErrObjectNotFound", err)
	}

	_, _, err = b.Get(context.Background(), storage.ObjectRef{}, nil)
	if !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("Get(empty ref) = %v, want ErrObjectNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newStub(t)
	s.deleteStatus = http.StatusNotFound
	b := s.backend()

	if err := b.Delete(context.Background(), storage.ObjectRef{ProviderID: "gone"}); err != nil {
		t.Fatalf("Delete() of a missing object = %v, want nil", err)
	}
	if err := b.Delete(context.Background(), storage.ObjectRef{}); err != nil {
		t.Fatalf("Delete(empty ref) = %v, want nil", err)
	}
}

func TestQuota(t *testing.T) {
	tests := []struct {
		name      string
		limit     string
		usage     string
		wantTotal int64
		wantUsed  int64
	}{
		{"normal account", "16106127360", "1073741824", 16106127360, 1073741824},
		{"unlimited reports headroom", "", "1073741824", 1073741824 + (1 << 40), 1073741824},
		{"unparseable usage is zero", "16106127360", "not-a-number", 16106127360, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.quotaLimit, s.quotaUsage = tc.limit, tc.usage
			b := s.backend()

			q, err := b.Quota(context.Background())
			if err != nil {
				t.Fatalf("Quota() = %v", err)
			}
			if q.TotalBytes != tc.wantTotal {
				t.Errorf("TotalBytes = %d, want %d", q.TotalBytes, tc.wantTotal)
			}
			if q.UsedBytes != tc.wantUsed {
				t.Errorf("UsedBytes = %d, want %d", q.UsedBytes, tc.wantUsed)
			}
		})
	}
}

func TestQuotaMapsRevokedGrant(t *testing.T) {
	s := newStub(t)
	s.quotaStatus = http.StatusUnauthorized
	b := s.backend()

	if _, err := b.Quota(context.Background()); !errors.Is(err, storage.ErrUnauthorized) {
		t.Fatalf("Quota() = %v, want ErrUnauthorized", err)
	}
}

func TestKind(t *testing.T) {
	if got := newStub(t).backend().Kind(); got != storage.KindGoogleDrive {
		t.Errorf("Kind() = %q, want %q", got, storage.KindGoogleDrive)
	}
}

func TestScopeIsDriveFileOnly(t *testing.T) {
	// The narrow scope is a product decision, not an implementation detail:
	// widening it would change what Skein can see and would drag the
	// project into Google's restricted-scope review.
	if Scope != "https://www.googleapis.com/auth/drive.file" {
		t.Errorf("Scope = %q, want drive.file only", Scope)
	}
}

// Shards must be parented into the app folder, not left at Drive root where
// they look like junk and get deleted. The resumable session declares the
// parent, and it is a separate code path from any other create call.
func TestPutParentsShardsIntoTheAppFolder(t *testing.T) {
	s := newStub(t)
	s.folderID = "app-folder-123"
	b := s.backend()

	if _, err := b.Put(context.Background(), bytes.NewReader([]byte("shard")),
		storage.ObjectSpec{Name: "skein-abc-0000.bin", Size: 5}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	if len(s.sessionParents) != 1 || s.sessionParents[0] != "app-folder-123" {
		t.Errorf("resumable session declared parents %v, want [app-folder-123]", s.sessionParents)
	}
}

// With no folder established the shard still uploads, to root, exactly as
// before. A folder that could not be created must not block an upload.
func TestPutFallsBackToRootWithNoFolder(t *testing.T) {
	s := newStub(t)
	s.folderID = ""
	b := s.backend()

	if _, err := b.Put(context.Background(), bytes.NewReader([]byte("shard")),
		storage.ObjectSpec{Name: "skein-abc-0000.bin", Size: 5}); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if len(s.sessionParents) != 0 {
		t.Errorf("parents = %v, want none when no folder is set", s.sessionParents)
	}
}
