package files

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
)

// maxFolderDepth bounds how deep the tree may go. Without a bound, a
// pathological tree makes the recursive delete and the breadcrumb walk
// unbounded work driven by user input.
const maxFolderDepth = 32

// CreateFolder creates a folder under parent.
func (s *Service) CreateFolder(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (Folder, error) {
	cleaned, err := CleanName(name)
	if err != nil {
		return Folder{}, err
	}

	if parentID != nil {
		if _, gerr := s.store.GetFolder(ctx, userID, *parentID); gerr != nil {
			if errors.Is(gerr, skerr.ErrNotFound) {
				return Folder{}, skerr.Public(skerr.ErrNotFound, "That folder does not exist.")
			}
			return Folder{}, fmt.Errorf("check parent folder: %w", gerr)
		}
		depth, derr := s.depthOf(ctx, userID, *parentID)
		if derr != nil {
			return Folder{}, derr
		}
		if depth+1 >= maxFolderDepth {
			return Folder{}, skerr.Public(skerr.ErrValidation,
				"Folders can only nest %d deep.", maxFolderDepth)
		}
	}

	folder, err := s.store.CreateFolder(ctx, uuid.New(), userID, parentID, cleaned)
	if err != nil {
		if errors.Is(err, skerr.ErrConflict) {
			return Folder{}, skerr.Public(skerr.ErrConflict,
				"A folder called %q is already here.", cleaned)
		}
		return Folder{}, fmt.Errorf("create folder: %w", err)
	}
	return folder, nil
}

// ListFolders returns the caller's whole folder tree. It is one query: the
// tree is small enough to send in full, and paginating it would make the
// sidebar unable to draw a breadcrumb without extra round trips.
func (s *Service) ListFolders(ctx context.Context, userID uuid.UUID) ([]Folder, error) {
	out, err := s.store.ListFolders(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return out, nil
}

// UpdateFolder renames and/or moves a folder.
func (s *Service) UpdateFolder(ctx context.Context, userID, folderID uuid.UUID, newName *string, newParent **uuid.UUID) (Folder, error) {
	current, err := s.store.GetFolder(ctx, userID, folderID)
	if err != nil {
		return Folder{}, err
	}

	name := current.Name
	if newName != nil {
		cleaned, cerr := CleanName(*newName)
		if cerr != nil {
			return Folder{}, cerr
		}
		name = cleaned
	}

	parent := current.ParentID
	if newParent != nil {
		parent = *newParent
		if err := s.validateMove(ctx, userID, folderID, parent); err != nil {
			return Folder{}, err
		}
	}

	updated, err := s.store.UpdateFolder(ctx, userID, folderID, name, parent)
	if err != nil {
		if errors.Is(err, skerr.ErrConflict) {
			return Folder{}, skerr.Public(skerr.ErrConflict,
				"A folder called %q is already there.", name)
		}
		return Folder{}, fmt.Errorf("update folder: %w", err)
	}
	return updated, nil
}

// validateMove rejects a move that would detach a branch from the root.
func (s *Service) validateMove(ctx context.Context, userID, folderID uuid.UUID, newParent *uuid.UUID) error {
	if newParent == nil {
		return nil // moving to the root is always fine
	}
	if *newParent == folderID {
		return skerr.Public(skerr.ErrValidation, "A folder cannot contain itself.")
	}
	if _, err := s.store.GetFolder(ctx, userID, *newParent); err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			return skerr.Public(skerr.ErrNotFound, "That folder does not exist.")
		}
		return fmt.Errorf("check destination folder: %w", err)
	}

	// Moving a folder into its own subtree would orphan the whole branch:
	// it would be unreachable from the root and impossible to delete
	// through the UI.
	descendants, err := s.store.FolderDescendants(ctx, userID, folderID)
	if err != nil {
		return fmt.Errorf("check folder subtree: %w", err)
	}
	if slices.Contains(descendants, *newParent) {
		return skerr.Public(skerr.ErrValidation,
			"A folder cannot be moved inside itself.")
	}
	return nil
}

// TrashFolder soft-deletes a folder and everything under it.
func (s *Service) TrashFolder(ctx context.Context, userID, folderID uuid.UUID) error {
	if _, err := s.store.GetFolder(ctx, userID, folderID); err != nil {
		return err
	}
	// The store does this as two statements over the same subtree. Both are
	// scoped by user_id, and a partially trashed tree self-heals on retry
	// because both are idempotent.
	n, err := s.store.SoftDeleteFolderTree(ctx, userID, folderID)
	if err != nil {
		return fmt.Errorf("trash folder: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
}

// depthOf counts how many levels sit above a folder.
func (s *Service) depthOf(ctx context.Context, userID, folderID uuid.UUID) (int, error) {
	depth := 0
	cursor := &folderID

	for depth < maxFolderDepth {
		f, err := s.store.GetFolder(ctx, userID, *cursor)
		if err != nil {
			if errors.Is(err, skerr.ErrNotFound) {
				return depth, nil
			}
			return 0, fmt.Errorf("walk folder tree: %w", err)
		}
		if f.ParentID == nil {
			return depth, nil
		}
		cursor = f.ParentID
		depth++
	}
	// The bound was hit, which means the tree is deeper than allowed or a
	// cycle exists. Either way, reporting the maximum is what the caller
	// needs to refuse the operation.
	return maxFolderDepth, nil
}
