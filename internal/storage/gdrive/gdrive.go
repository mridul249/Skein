// Package gdrive implements storage.Backend over Google Drive's resumable
// upload protocol.
//
// The Drive Go client is used for metadata and quota, but not for uploads:
// its media helpers want an io.Reader they can retry, which means either
// buffering or a seekable source, and neither is available when the bytes are
// arriving from a browser. The upload path here speaks the resumable protocol
// directly so that the body streams straight through.
package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mridul60214/skein/internal/storage"
)

// Scope is the only OAuth scope Skein ever asks for.
//
// drive.file limits Skein to files it created itself. It cannot see, let alone
// touch, anything already in the user's Drive. That is a deliberate privacy
// decision, and it also keeps the project out of Google's restricted-scope
// verification review.
const Scope = "https://www.googleapis.com/auth/drive.file"

const (
	uploadEndpoint = "https://www.googleapis.com/upload/drive/v3/files"
	filesEndpoint  = "https://www.googleapis.com/drive/v3/files"
	aboutEndpoint  = "https://www.googleapis.com/drive/v3/about"

	// requestTimeout bounds metadata calls. It is deliberately not applied
	// to uploads or downloads, whose duration is proportional to file size;
	// those are bounded by the request context instead.
	requestTimeout = 30 * time.Second
)

// Backend is one connected Google account.
type Backend struct {
	// httpClient carries the OAuth token. It has no Timeout because the
	// transfer calls are bounded by context; metadata calls set their own.
	httpClient *http.Client
	folderName string
}

// New builds a Drive backend over an already-authenticated client. Token
// refresh is the caller's problem, which is what oauth2.Config.Client does.
func New(client *http.Client) *Backend {
	return &Backend{httpClient: client, folderName: "Skein"}
}

// Kind identifies this implementation.
func (b *Backend) Kind() storage.Kind { return storage.KindGoogleDrive }

// Put streams r to Drive through a resumable session.
//
// Two requests: one to open the session and receive a session URI, one to PUT
// the bytes. The body of the second is the caller's reader, wrapped only to
// count bytes — never buffered, so a 30 GB shard costs the same memory as a
// 30 KB one.
func (b *Backend) Put(ctx context.Context, r io.Reader, spec storage.ObjectSpec) (storage.ObjectRef, error) {
	if spec.Size < 0 {
		return storage.ObjectRef{}, fmt.Errorf("%w: negative size", storage.ErrSizeMismatch)
	}

	sessionURI, err := b.startResumableSession(ctx, spec)
	if err != nil {
		return storage.ObjectRef{}, err
	}

	counter := &countingReader{r: r}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURI, counter)
	if err != nil {
		return storage.ObjectRef{}, fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// ContentLength must be set explicitly: with a plain io.Reader body,
	// net/http would otherwise use chunked encoding, which the resumable
	// endpoint rejects.
	req.ContentLength = spec.Size

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		// A cancelled context here means the client hung up. Drive
		// abandons the session on its own after a week; there is nothing
		// to clean up because nothing was committed.
		return storage.ObjectRef{}, fmt.Errorf("upload body: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return storage.ObjectRef{}, b.apiError(resp, "upload body")
	}

	// Rules.md §2.7: what the client declared is checked against what
	// actually arrived, before anything is recorded as complete.
	if counter.n != spec.Size {
		return storage.ObjectRef{}, fmt.Errorf("%w: declared %d, sent %d",
			storage.ErrSizeMismatch, spec.Size, counter.n)
	}

	var created struct {
		ID   string `json:"id"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		return storage.ObjectRef{}, fmt.Errorf("decode upload response: %w", err)
	}
	if created.ID == "" {
		return storage.ObjectRef{}, errors.New("gdrive: upload response carried no file id")
	}

	// Drive reports the stored size. If it disagrees with what was sent the
	// object is not what we think it is, so it is deleted rather than
	// recorded.
	if created.Size != "" {
		stored, perr := strconv.ParseInt(created.Size, 10, 64)
		if perr == nil && stored != spec.Size {
			ref := storage.ObjectRef{ProviderID: created.ID, Size: stored}
			if derr := b.Delete(ctx, ref); derr != nil {
				return storage.ObjectRef{}, fmt.Errorf(
					"%w: drive stored %d of %d, and cleanup failed: %v",
					storage.ErrSizeMismatch, stored, spec.Size, derr)
			}
			return storage.ObjectRef{}, fmt.Errorf("%w: drive stored %d of %d",
				storage.ErrSizeMismatch, stored, spec.Size)
		}
	}

	return storage.ObjectRef{ProviderID: created.ID, Size: spec.Size}, nil
}

// startResumableSession creates the file metadata and returns the session URI
// the bytes are PUT to.
func (b *Backend) startResumableSession(ctx context.Context, spec storage.ObjectSpec) (string, error) {
	meta := map[string]any{
		"name": spec.Name,
		// The content is ciphertext. Declaring anything other than
		// octet-stream would be describing bytes we cannot describe.
		"mimeType": "application/octet-stream",
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("encode upload metadata: %w", err)
	}

	url := uploadEndpoint + "?uploadType=resumable&supportsAllDrives=false&fields=id,size"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(spec.Size, 10))

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("open resumable session: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", b.apiError(resp, "open resumable session")
	}
	uri := resp.Header.Get("Location")
	if uri == "" {
		return "", errors.New("gdrive: resumable session response carried no Location")
	}
	return uri, nil
}

// Get returns a reader over the object's bytes, optionally a range.
func (b *Backend) Get(ctx context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, int64, error) {
	if ref.ProviderID == "" {
		return nil, 0, storage.ErrObjectNotFound
	}
	url := filesEndpoint + "/" + ref.ProviderID + "?alt=media&supportsAllDrives=false"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build download request: %w", err)
	}
	if rng != nil {
		if rng.Start < 0 || rng.Length <= 0 {
			return nil, 0, storage.ErrRangeNotSat
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rng.Start, rng.Start+rng.Length-1))
	}

	// The success branch hands resp.Body to the caller, who closes it; every
	// other branch drains and closes here.
	//nolint:bodyclose // ownership transfers to the caller on 200/206.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("download object: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// The caller owns resp.Body from here and closes it.
		return resp.Body, resp.ContentLength, nil
	case http.StatusNotFound:
		drainAndClose(resp.Body)
		return nil, 0, storage.ErrObjectNotFound
	case http.StatusRequestedRangeNotSatisfiable:
		drainAndClose(resp.Body)
		return nil, 0, storage.ErrRangeNotSat
	default:
		err := b.apiError(resp, "download object")
		drainAndClose(resp.Body)
		return nil, 0, err
	}
}

// Delete removes an object. A missing object is not an error, because cleanup
// paths run more than once by design.
func (b *Backend) Delete(ctx context.Context, ref storage.ObjectRef) error {
	if ref.ProviderID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := filesEndpoint + "/" + ref.ProviderID + "?supportsAllDrives=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent ||
		(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return b.apiError(resp, "delete object")
}

// Quota reports the account's storage capacity.
func (b *Backend) Quota(ctx context.Context) (storage.Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		aboutEndpoint+"?fields=storageQuota", nil)
	if err != nil {
		return storage.Quota{}, fmt.Errorf("build quota request: %w", err)
	}

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return storage.Quota{}, fmt.Errorf("read quota: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return storage.Quota{}, b.apiError(resp, "read quota")
	}

	var about struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&about); err != nil {
		return storage.Quota{}, fmt.Errorf("decode quota: %w", err)
	}

	// An unlimited account reports no limit at all. Reporting zero would
	// make the shard planner think there is no space; reporting a huge
	// number would make it think there is infinite space. Neither is true,
	// so unlimited is treated as a fixed large ceiling and the atomic
	// reservation catches the real limit if one exists.
	used := parseInt64(about.StorageQuota.Usage)
	total := parseInt64(about.StorageQuota.Limit)
	if about.StorageQuota.Limit == "" {
		total = used + (1 << 40) // 1 TiB of assumed headroom
	}

	return storage.Quota{TotalBytes: total, UsedBytes: used}, nil
}

// apiError turns a Drive response into one of the package sentinels. The
// provider's own message is included in the wrapped error for the log, and
// never reaches the client: httpapi maps sentinels to generic text.
func (b *Backend) apiError(resp *http.Response, what string) error {
	// Bounded read: an error body is not a place to spend memory.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := strings.TrimSpace(string(snippet))

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// 403 is overloaded at Drive: it covers both a revoked grant and
		// a quota refusal, distinguishable only by the reason string.
		if strings.Contains(detail, "storageQuotaExceeded") ||
			strings.Contains(detail, "quotaExceeded") {
			return fmt.Errorf("%s: %w", what, storage.ErrQuota)
		}
		return fmt.Errorf("%s: %w (%s)", what, storage.ErrUnauthorized, detail)
	case http.StatusNotFound:
		return fmt.Errorf("%s: %w", what, storage.ErrObjectNotFound)
	case http.StatusInsufficientStorage:
		return fmt.Errorf("%s: %w", what, storage.ErrQuota)
	default:
		return fmt.Errorf("%s: drive returned %d: %s", what, resp.StatusCode, detail)
	}
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// drainAndClose consumes a bounded amount of the remaining body so the
// connection can be reused, then closes it. Rules.md §2.11: every ReadCloser
// is closed on every path.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64*1024))
	_ = body.Close()
}

// countingReader counts bytes without holding them.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Compile-time check.
var _ storage.Backend = (*Backend)(nil)
