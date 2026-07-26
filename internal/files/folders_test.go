package files_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/files"
	"github.com/mridul60214/skein/internal/skerr"
)

func TestCreateFolder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	root, err := f.svc.CreateFolder(ctx, f.userID, nil, "projects")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	if root.Name != "projects" || root.ParentID != nil {
		t.Errorf("root folder = %+v", root)
	}

	child, err := f.svc.CreateFolder(ctx, f.userID, &root.ID, "skein")
	if err != nil {
		t.Fatalf("CreateFolder(child) = %v", err)
	}
	if child.ParentID == nil || *child.ParentID != root.ID {
		t.Errorf("child parent = %v, want %v", child.ParentID, root.ID)
	}
}

func TestCreateFolderRejectsDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.svc.CreateFolder(ctx, f.userID, nil, "docs"); err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	if _, err := f.svc.CreateFolder(ctx, f.userID, nil, "docs"); !errors.Is(err, skerr.ErrConflict) {
		t.Fatalf("duplicate = %v, want ErrConflict", err)
	}

	// The same name under a different parent is fine.
	parent, err := f.svc.CreateFolder(ctx, f.userID, nil, "archive")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	if _, err := f.svc.CreateFolder(ctx, f.userID, &parent.ID, "docs"); err != nil {
		t.Errorf("same name under another parent = %v, want nil", err)
	}
}

// Moving a folder into its own subtree would detach the branch from the root:
// unreachable in the UI and impossible to delete.
func TestFolderCannotBeMovedIntoItself(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a, err := f.svc.CreateFolder(ctx, f.userID, nil, "a")
	if err != nil {
		t.Fatalf("CreateFolder(a) = %v", err)
	}
	b, err := f.svc.CreateFolder(ctx, f.userID, &a.ID, "b")
	if err != nil {
		t.Fatalf("CreateFolder(b) = %v", err)
	}
	c, err := f.svc.CreateFolder(ctx, f.userID, &b.ID, "c")
	if err != nil {
		t.Fatalf("CreateFolder(c) = %v", err)
	}

	tests := []struct {
		name   string
		folder uuid.UUID
		into   uuid.UUID
	}{
		{"into itself", a.ID, a.ID},
		{"into its direct child", a.ID, b.ID},
		{"into its grandchild", a.ID, c.ID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := &tc.into
			_, err := f.svc.UpdateFolder(ctx, f.userID, tc.folder, nil, &target)
			if !errors.Is(err, skerr.ErrValidation) {
				t.Fatalf("UpdateFolder() = %v, want ErrValidation", err)
			}
		})
	}

	// Moving a child up to the root is fine.
	var root *uuid.UUID
	if _, err := f.svc.UpdateFolder(ctx, f.userID, c.ID, nil, &root); err != nil {
		t.Errorf("move to root = %v, want nil", err)
	}
}

func TestTrashFolderTakesItsContentsWithIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	parent, err := f.svc.CreateFolder(ctx, f.userID, nil, "parent")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	child, err := f.svc.CreateFolder(ctx, f.userID, &parent.ID, "child")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}

	file, err := f.svc.Upload(ctx, files.UploadRequest{
		UserID: f.userID, FolderID: &child.ID, Name: "note.txt", Size: 5,
	}, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Upload() = %v", err)
	}

	if err := f.svc.TrashFolder(ctx, f.userID, parent.ID); err != nil {
		t.Fatalf("TrashFolder() = %v", err)
	}

	// A partially trashed tree is not a state that may exist: the whole
	// subtree and its files go together.
	list, err := f.svc.ListFolders(ctx, f.userID)
	if err != nil {
		t.Fatalf("ListFolders() = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("%d folders survived the trash", len(list))
	}
	if _, err := f.svc.Get(ctx, f.userID, file.ID); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("the file inside the trashed folder is still visible: %v", err)
	}
}

func TestFoldersAreScopedToTheirOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stranger := uuid.New()

	folder, err := f.svc.CreateFolder(ctx, f.userID, nil, "mine")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}

	name := "yours"
	if _, uerr := f.svc.UpdateFolder(ctx, stranger, folder.ID, &name, nil); !errors.Is(uerr, skerr.ErrNotFound) {
		t.Errorf("a stranger renamed another user's folder: %v", uerr)
	}
	if terr := f.svc.TrashFolder(ctx, stranger, folder.ID); !errors.Is(terr, skerr.ErrNotFound) {
		t.Errorf("a stranger trashed another user's folder: %v", terr)
	}

	list, err := f.svc.ListFolders(ctx, stranger)
	if err != nil {
		t.Fatalf("ListFolders() = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a stranger listed %d folders, want 0", len(list))
	}
}

func TestFolderNestingIsBounded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var parent *uuid.UUID
	var lastErr error
	for i := 0; i < 40; i++ {
		folder, err := f.svc.CreateFolder(ctx, f.userID, parent, "level")
		if err != nil {
			lastErr = err
			break
		}
		parent = &folder.ID
	}
	if lastErr == nil {
		t.Fatal("folder nesting is unbounded")
	}
	if !errors.Is(lastErr, skerr.ErrValidation) {
		t.Fatalf("deep nesting = %v, want ErrValidation", lastErr)
	}
}

func TestTrashAndRestoreFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	file := f.upload(t, "note.txt", []byte("hello"))

	if err := f.svc.Trash(ctx, f.userID, file.ID); err != nil {
		t.Fatalf("Trash() = %v", err)
	}
	if _, err := f.svc.Get(ctx, f.userID, file.ID); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("a trashed file is still readable: %v", err)
	}

	trashed, err := f.svc.ListTrashed(ctx, f.userID, 50)
	if err != nil {
		t.Fatalf("ListTrashed() = %v", err)
	}
	if len(trashed) != 1 {
		t.Fatalf("trash holds %d files, want 1", len(trashed))
	}

	if err := f.svc.Restore(ctx, f.userID, file.ID); err != nil {
		t.Fatalf("Restore() = %v", err)
	}
	// The bytes were never deleted, so the file reads back intact.
	if got := f.readAll(t, file.ID, nil); string(got) != "hello" {
		t.Errorf("restored file = %q, want hello", got)
	}
}

func TestPermanentDeleteRemovesTheObjects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	file := f.upload(t, "gone.bin", []byte("bytes to remove"))
	if f.store.ShardCount(file.ID) == 0 {
		t.Fatal("no shards were recorded")
	}

	if err := f.svc.Delete(ctx, f.userID, file.ID); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := f.svc.Get(ctx, f.userID, file.ID); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("Get() after delete = %v, want ErrNotFound", err)
	}
	if n := f.store.ShardCount(file.ID); n != 0 {
		t.Errorf("%d shard rows survived a permanent delete", n)
	}
}

func TestRenameAndMoveFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	folder, err := f.svc.CreateFolder(ctx, f.userID, nil, "dest")
	if err != nil {
		t.Fatalf("CreateFolder() = %v", err)
	}
	file := f.upload(t, "old.txt", []byte("content"))

	newName := "new.txt"
	target := &folder.ID
	updated, err := f.svc.Rename(ctx, f.userID, file.ID, &newName, &target)
	if err != nil {
		t.Fatalf("Rename() = %v", err)
	}
	if updated.Name != newName {
		t.Errorf("name = %q, want %q", updated.Name, newName)
	}
	if updated.FolderID == nil || *updated.FolderID != folder.ID {
		t.Errorf("folder = %v, want %v", updated.FolderID, folder.ID)
	}

	// Moving back to the root is an explicit null, distinguishable from
	// "leave it alone".
	var root *uuid.UUID
	moved, err := f.svc.Rename(ctx, f.userID, file.ID, nil, &root)
	if err != nil {
		t.Fatalf("Rename(to root) = %v", err)
	}
	if moved.FolderID != nil {
		t.Errorf("folder = %v, want root", moved.FolderID)
	}
	if moved.Name != newName {
		t.Errorf("a move changed the name to %q", moved.Name)
	}
}

func TestRenameRejectsABadName(t *testing.T) {
	f := newFixture(t)
	file := f.upload(t, "ok.txt", []byte("x"))

	bad := "../escape"
	_, err := f.svc.Rename(context.Background(), f.userID, file.ID, &bad, nil)
	if !errors.Is(err, skerr.ErrValidation) {
		t.Fatalf("Rename() = %v, want ErrValidation", err)
	}
}
