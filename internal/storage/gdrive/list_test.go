package gdrive

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// THE REGRESSION THIS FILE EXISTS FOR (2026-08-06).
//
// List used to query "'<folderID>' in parents". The app folder is named with a
// suffix derived from the user id, and a rebuilt database mints a NEW user id,
// so a recovering install computed a name matching nothing, created an empty
// folder, listed THAT, and reported "0 manifests" over seven intact files.
//
// The whole failure was invisible to the suite because List had no test at
// all. These are those tests.

// The scope of the query is the bug. Asserting only on the returned objects
// would pass against the broken version too, because a stub answers whatever
// it is asked; what has to be checked is that the request does not name a
// parent folder.
func TestListIsNotScopedToTheAppFolder(t *testing.T) {
	s := newStub(t)
	s.folderID = "app-folder-123"
	s.listFiles = []stubFile{
		{ID: "o1", Name: ".skein_manifest_x.enc", Size: "10", Parents: []string{"some-other-folder"}},
	}
	b := s.backend()

	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List() = %v", err)
	}

	if len(s.listQueries) != 1 {
		t.Fatalf("list requests = %d, want 1", len(s.listQueries))
	}
	q := s.listQueries[0]
	if strings.Contains(q, "in parents") {
		t.Errorf("list query scopes to a parent folder, which is the recovery bug: %q", q)
	}
	if strings.Contains(q, "app-folder-123") {
		t.Errorf("list query names the app folder, which a rebuilt database cannot resolve: %q", q)
	}

	// An object in a folder this backend has never heard of must still come
	// back. That object is exactly what recovery is looking for.
	if len(got) != 1 || got[0].ProviderID != "o1" {
		t.Fatalf("List() = %+v, want the object from the unrelated folder", got)
	}
}

// A backend with no app folder resolved must not report an empty account. It
// has not looked and found nothing; it has no folder id, which is a different
// statement, and returning nil made the two indistinguishable to a caller
// whose whole job is telling them apart.
func TestListWithNoAppFolderStillLooks(t *testing.T) {
	s := newStub(t)
	s.folderID = ""
	s.listFiles = []stubFile{
		{ID: "o1", Name: "skein-abc-0000.bin", Size: "7", Parents: []string{"f1"}},
	}
	b := s.backend()

	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(s.listQueries) == 0 {
		t.Fatal("List() returned without asking the provider anything")
	}
	if len(got) != 1 {
		t.Fatalf("List() returned %d objects, want 1", len(got))
	}
}

// ParentID is what lets recovery repoint the account at the folder the
// recovered data actually lives in. Without it the files come back and every
// subsequent upload goes somewhere else.
func TestListReportsTheParentFolder(t *testing.T) {
	s := newStub(t)
	s.listFiles = []stubFile{
		{ID: "o1", Name: ".skein_manifest_x.enc", Size: "10", Parents: []string{"real-folder"}},
		{ID: "o2", Name: "skein-abc-0000.bin", Size: "5", Parents: nil},
	}
	b := s.backend()

	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() returned %d objects, want 2", len(got))
	}
	if got[0].ParentID != "real-folder" {
		t.Errorf("ParentID = %q, want %q", got[0].ParentID, "real-folder")
	}
	// No parents at all is an object at root, and must read as "unknown"
	// rather than inventing a folder.
	if got[1].ParentID != "" {
		t.Errorf("ParentID = %q for a parentless object, want empty", got[1].ParentID)
	}
}

// The app folder is itself an object this client created, so an unscoped query
// would return it. A container is not something Skein stored, and counting it
// would inflate every listing by one per folder.
func TestListExcludesFolders(t *testing.T) {
	s := newStub(t)
	b := s.backend()

	if _, err := b.List(context.Background()); err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(s.listQueries) != 1 {
		t.Fatalf("list requests = %d, want 1", len(s.listQueries))
	}
	if !strings.Contains(s.listQueries[0], "mimeType != "+driveQuote(folderMIME)) {
		t.Errorf("list query does not exclude folders: %q", s.listQueries[0])
	}
}

// A listing that fails must return an error, never an empty slice. Recovery
// classifies an unreadable account as indeterminate, and it can only do that
// if "I could not look" is distinguishable from "there is nothing here".
func TestListFailureIsAnErrorNotAnEmptyResult(t *testing.T) {
	s := newStub(t)
	s.listStatus = http.StatusForbidden
	s.forbiddenBody = `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`
	b := s.backend()

	got, err := b.List(context.Background())
	if err == nil {
		t.Fatalf("List() = %v, nil; want an error", got)
	}
	if got != nil {
		t.Errorf("List() returned %d objects alongside an error, want none", len(got))
	}
}
