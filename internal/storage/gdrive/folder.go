package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mridul249/Skein/internal/storage"
)

// AppFolderName is the folder shards are kept in at the provider.
const AppFolderName = "Skein"

// readmeName is the note left in the app folder for whoever finds it.
const readmeName = "README.txt"

// readmeBody explains the folder to someone browsing Drive who has never heard
// of Skein. It is the only thing standing between a tidy-minded user and
// permanent data loss, so it says what happens rather than merely asking.
const readmeBody = `Skein storage - do not delete

These .bin files are encrypted shards. Each is one part of a larger
file that Skein striped across your drives. Deleting any of them
permanently destroys the file it belongs to. Manage your files
through Skein, not here.
`

// folderMIME is Drive's marker for a container rather than a stored object.
const folderMIME = "application/vnd.google-apps.folder"

// FindFolder returns the id of the newest-safe app folder, or "" if none
// exists.
//
// Ordered by createdTime so that if duplicates already exist — from a race
// before this code, or from two processes racing outside one process's
// singleflight — every caller converges on the same one: the oldest. Picking
// arbitrarily would let two processes disagree forever and split a file's
// shards across two folders.
func (b *Backend) FindFolder(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	q := fmt.Sprintf(
		"name = %s and mimeType = %s "+
			"and 'root' in parents and trashed = false",
		driveQuote(name), driveQuote(folderMIME))

	endpoint := filesEndpoint + "?" + url.Values{
		"q":                         {q},
		"fields":                    {"files(id,createdTime)"},
		"orderBy":                   {"createdTime"},
		"pageSize":                  {"1"},
		"supportsAllDrives":         {"false"},
		"includeItemsFromAllDrives": {"false"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build folder list request: %w", err)
	}

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list app folder: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", b.apiError(resp, "list app folder")
	}

	var out struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode folder list: %w", err)
	}
	if len(out.Files) == 0 {
		return "", nil
	}
	return out.Files[0].ID, nil
}

// CreateFolder creates the app folder at Drive root and returns its id.
func (b *Backend) CreateFolder(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": folderMIME,
		"parents":  []string{"root"},
	})
	if err != nil {
		return "", fmt.Errorf("encode folder metadata: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		filesEndpoint+"?fields=id&supportsAllDrives=false", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build folder create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create app folder: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", b.apiError(resp, "create app folder")
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		return "", fmt.Errorf("decode created folder: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("gdrive: folder create returned no id")
	}
	return created.ID, nil
}

// WriteReadme drops the explanatory note into the app folder.
//
// Best-effort by contract: the caller logs a failure and carries on. A missing
// README makes the folder less self-explanatory; failing the upload over it
// would make a cosmetic problem into a functional one.
func (b *Backend) WriteReadme(ctx context.Context, folderID string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	meta, err := json.Marshal(map[string]any{
		"name":     readmeName,
		"mimeType": "text/plain",
		"parents":  []string{folderID},
	})
	if err != nil {
		return fmt.Errorf("encode readme metadata: %w", err)
	}

	// Multipart upload: metadata part then content part, in one request.
	const boundary = "skein-readme-boundary"
	var body strings.Builder
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Type: application/json; charset=UTF-8\r\n\r\n")
	body.Write(meta)
	body.WriteString("\r\n--" + boundary + "\r\n")
	body.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	body.WriteString(readmeBody)
	body.WriteString("\r\n--" + boundary + "--\r\n")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		uploadEndpoint+"?uploadType=multipart&fields=id&supportsAllDrives=false",
		strings.NewReader(body.String()))
	if err != nil {
		return fmt.Errorf("build readme request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("write readme: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return b.apiError(resp, "write readme")
	}
	return nil
}

// ListRootShards returns the ids and names of skein-*.bin objects still
// sitting at Drive root. It backs the one-shot folder migration.
func (b *Backend) ListRootShards(ctx context.Context) ([]RootObject, error) {
	var out []RootObject
	pageToken := ""

	for {
		batch, next, err := b.listRootShardPage(ctx, pageToken)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		pageToken = next
	}
}

// RootObject is a provider object found at Drive root.
type RootObject struct {
	ID   string
	Name string
	Size int64
}

func (b *Backend) listRootShardPage(ctx context.Context, pageToken string) ([]RootObject, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// drive.file scope already limits this to objects Skein created, so the
	// name prefix is a second filter rather than the only one.
	values := url.Values{
		"q":                 {"'root' in parents and trashed = false"},
		"fields":            {"nextPageToken,files(id,name,size)"},
		"pageSize":          {"1000"},
		"supportsAllDrives": {"false"},
	}
	if pageToken != "" {
		values.Set("pageToken", pageToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("build root list request: %w", err)
	}

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("list root objects: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, "", b.apiError(resp, "list root objects")
	}

	var page struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Size string `json:"size"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("decode root list: %w", err)
	}

	out := make([]RootObject, 0, len(page.Files))
	for _, f := range page.Files {
		if !strings.HasPrefix(f.Name, "skein-") || !strings.HasSuffix(f.Name, ".bin") {
			continue
		}
		out = append(out, RootObject{ID: f.ID, Name: f.Name, Size: parseInt64(f.Size)})
	}
	return out, page.NextPageToken, nil
}

// MoveToFolder reparents an object into the app folder.
func (b *Backend) MoveToFolder(ctx context.Context, objectID, folderID string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	endpoint := filesEndpoint + "/" + objectID + "?" + url.Values{
		"addParents":        {folderID},
		"removeParents":     {"root"},
		"fields":            {"id"},
		"supportsAllDrives": {"false"},
	}.Encode()

	// An empty JSON body: the reparenting is entirely in the query string.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("build move request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("move object into app folder: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return b.apiError(resp, "move object into app folder")
	}
	return nil
}

// driveQuote escapes a value for a Drive query string, where a literal is
// single-quoted and a single quote inside it is backslash-escaped.
//
// The folder name is a constant today, but a query built by concatenation is a
// query injection waiting for the day it is not.
func driveQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// List enumerates every object this client can see in the account, satisfying
// storage.Lister.
//
// DELIBERATELY NOT SCOPED TO b.folderID. See storage.Lister for why: a folder
// name derived from the user id is unresolvable after the rebuild that
// recovery exists to survive, and scoping here turned seven intact files into
// "0 manifests" on 2026-08-06. drive.file scope already bounds the result to
// objects this OAuth client created, so an unscoped list is still only ever
// Skein's own objects — including any left at root before the app folder
// existed, which the folder-scoped query silently excluded.
//
// It also no longer returns empty when no app folder is resolved. That was a
// silent lie: "I have no folder id" is not "this account is empty", and the
// caller cannot tell the two apart from a nil slice.
//
// No name filter. Reconstruction asks for everything Skein wrote and decides
// for itself what a manifest is; filtering here would put that knowledge in
// two places.
func (b *Backend) List(ctx context.Context) ([]storage.ListedObject, error) {
	var out []storage.ListedObject
	pageToken := ""
	for {
		batch, next, err := b.listFolderPage(ctx, pageToken)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		pageToken = next
	}
}

func (b *Backend) listFolderPage(ctx context.Context, pageToken string) ([]storage.ListedObject, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	values := url.Values{
		// No parent clause: everything this client created, anywhere.
		// Folders are excluded because the app folder itself is one, and a
		// container is not an object Skein stored.
		"q":                 {"trashed = false and mimeType != " + driveQuote(folderMIME)},
		"fields":            {"nextPageToken,files(id,name,size,parents)"},
		"pageSize":          {"1000"},
		"supportsAllDrives": {"false"},
	}
	if pageToken != "" {
		values.Set("pageToken", pageToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("build folder list request: %w", err)
	}

	//nolint:bodyclose // drainAndClose below closes it on every path.
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("list folder objects: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, "", b.apiError(resp, "list folder objects")
	}

	var page struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Size    string   `json:"size"`
			Parents []string `json:"parents"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("decode folder list: %w", err)
	}

	out := make([]storage.ListedObject, 0, len(page.Files))
	for _, f := range page.Files {
		// Drive models parents as a list, but an object Skein wrote has
		// exactly one: uploads name a single parent and the folder migration
		// moves rather than adds. Taking the first is the honest reading of
		// "where does this live", and an object at root reports none.
		parent := ""
		if len(f.Parents) > 0 {
			parent = f.Parents[0]
		}
		out = append(out, storage.ListedObject{
			ProviderID: f.ID,
			Name:       f.Name,
			Size:       parseInt64(f.Size),
			ParentID:   parent,
		})
	}
	return out, page.NextPageToken, nil
}
