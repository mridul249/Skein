package files

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
)

// MemoryStore is an in-memory Store for tests, in the same spirit as
// auth.MemoryStore and storage/local: the whole suite runs with no database
// and no network.
//
// It reproduces the constraints the service actually leans on — ownership
// scoping on every read, name uniqueness within a folder, and soft delete —
// and nothing else.
type MemoryStore struct {
	mu      sync.Mutex
	folders map[uuid.UUID]Folder
	files   map[uuid.UUID]File
	shards  map[uuid.UUID][]Shard
	clock   func() time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		folders: map[uuid.UUID]Folder{},
		files:   map[uuid.UUID]File{},
		shards:  map[uuid.UUID][]Shard{},
		clock:   time.Now,
	}
}

// CreateFolder inserts a folder, rejecting a duplicate name in the same parent.
func (m *MemoryStore) CreateFolder(_ context.Context, id, userID uuid.UUID, parentID *uuid.UUID, name string) (Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// folders_name_not_blank, in both dialects.
	if strings.TrimSpace(name) == "" {
		return Folder{}, skerr.ErrValidation
	}
	for _, f := range m.folders {
		if f.UserID == userID && f.DeletedAt == nil && f.Name == name && samePtr(f.ParentID, parentID) {
			return Folder{}, skerr.ErrConflict
		}
	}
	now := m.clock()
	f := Folder{ID: id, UserID: userID, ParentID: parentID, Name: name, CreatedAt: now, UpdatedAt: now}
	m.folders[id] = f
	return f, nil
}

// GetFolder loads one live folder the user owns.
func (m *MemoryStore) GetFolder(_ context.Context, userID, id uuid.UUID) (Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.folders[id]
	if !ok || f.UserID != userID || f.DeletedAt != nil {
		return Folder{}, skerr.ErrNotFound
	}
	return f, nil
}

// ListFolders returns the caller's live folders.
func (m *MemoryStore) ListFolders(_ context.Context, userID uuid.UUID) ([]Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Folder{}
	for _, f := range m.folders {
		if f.UserID == userID && f.DeletedAt == nil {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateFolder renames and/or moves a folder.
func (m *MemoryStore) UpdateFolder(_ context.Context, userID, id uuid.UUID, name string, parentID *uuid.UUID) (Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.folders[id]
	if !ok || f.UserID != userID || f.DeletedAt != nil {
		return Folder{}, skerr.ErrNotFound
	}
	for oid, other := range m.folders {
		if oid == id {
			continue
		}
		if other.UserID == userID && other.DeletedAt == nil &&
			other.Name == name && samePtr(other.ParentID, parentID) {
			return Folder{}, skerr.ErrConflict
		}
	}
	f.Name = name
	f.ParentID = parentID
	f.UpdatedAt = m.clock()
	m.folders[id] = f
	return f, nil
}

// FolderDescendants returns a subtree's folder ids, including the root.
func (m *MemoryStore) FolderDescendants(_ context.Context, userID, id uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.descendantsLocked(userID, id), nil
}

func (m *MemoryStore) descendantsLocked(userID, id uuid.UUID) []uuid.UUID {
	out := []uuid.UUID{id}
	frontier := []uuid.UUID{id}

	for len(frontier) > 0 {
		var next []uuid.UUID
		for _, parent := range frontier {
			for cid, c := range m.folders {
				if c.UserID == userID && c.ParentID != nil && *c.ParentID == parent {
					out = append(out, cid)
					next = append(next, cid)
				}
			}
		}
		frontier = next
	}
	return out
}

// SoftDeleteFolderTree trashes a folder, its subfolders and their files.
func (m *MemoryStore) SoftDeleteFolderTree(_ context.Context, userID, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	root, ok := m.folders[id]
	if !ok || root.UserID != userID || root.DeletedAt != nil {
		return 0, nil
	}

	now := m.clock()
	subtree := map[uuid.UUID]bool{}
	var n int64
	for _, fid := range m.descendantsLocked(userID, id) {
		subtree[fid] = true
		f := m.folders[fid]
		if f.DeletedAt == nil {
			t := now
			f.DeletedAt = &t
			f.UpdatedAt = now
			m.folders[fid] = f
			n++
		}
	}
	for fid, f := range m.files {
		if f.UserID == userID && f.DeletedAt == nil && f.FolderID != nil && subtree[*f.FolderID] {
			t := now
			f.DeletedAt = &t
			f.UpdatedAt = now
			m.files[fid] = f
		}
	}
	return n, nil
}

// CreateFile inserts a pending file row.
func (m *MemoryStore) CreateFile(_ context.Context, n NewFile) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// files_size_non_negative and files_name_not_blank, in both dialects.
	if n.SizeBytes < 0 || strings.TrimSpace(n.Name) == "" {
		return File{}, skerr.ErrValidation
	}
	for _, f := range m.files {
		if f.UserID == n.UserID && f.DeletedAt == nil && f.Name == n.Name &&
			samePtr(f.FolderID, n.FolderID) && f.Status == StatusReady {
			return File{}, skerr.ErrConflict
		}
	}
	now := m.clock()
	f := File{
		ID:           n.ID,
		UserID:       n.UserID,
		FolderID:     n.FolderID,
		Name:         n.Name,
		SizeBytes:    n.SizeBytes,
		DeclaredMime: n.DeclaredMime,
		IsStriped:    n.IsStriped,
		IsEncrypted:  n.IsEncrypted,
		Status:       StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.files[n.ID] = f
	return f, nil
}

// MarkFileReady commits a file.
func (m *MemoryStore) MarkFileReady(_ context.Context, userID, id uuid.UUID, size int64, sha []byte) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.files[id]
	if !ok || f.UserID != userID || f.Status != StatusPending {
		return File{}, skerr.ErrNotFound
	}
	f.Status = StatusReady
	f.SizeBytes = size
	f.ContentSHA = sha
	f.UpdatedAt = m.clock()
	m.files[id] = f
	return f, nil
}

// MarkFileFailed records that an upload did not finish.
func (m *MemoryStore) MarkFileFailed(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.Status != StatusPending {
		return nil
	}
	f.Status = StatusFailed
	f.UpdatedAt = m.clock()
	m.files[id] = f
	return nil
}

// GetFile loads one live file the user owns.
func (m *MemoryStore) GetFile(_ context.Context, userID, id uuid.UUID) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.UserID != userID || f.DeletedAt != nil {
		return File{}, skerr.ErrNotFound
	}
	return f, nil
}

// ListFiles returns a keyset-paginated page.
func (m *MemoryStore) ListFiles(_ context.Context, userID uuid.UUID, p ListParams) ([]File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []File
	for _, f := range m.files {
		if f.UserID != userID || f.DeletedAt != nil || f.Status != StatusReady {
			continue
		}
		if !samePtr(f.FolderID, p.FolderID) {
			continue
		}
		if p.CursorCreatedAt != nil && !f.CreatedAt.Before(*p.CursorCreatedAt) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })

	if int32(len(out)) > p.Limit {
		out = out[:p.Limit]
	}
	for i := range out {
		out[i].Shards = append([]Shard(nil), m.shards[out[i].ID]...)
	}
	if out == nil {
		out = []File{}
	}
	return out, nil
}

// ListTrashed returns soft-deleted files.
func (m *MemoryStore) ListTrashed(_ context.Context, userID uuid.UUID, limit int32) ([]File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []File{}
	for _, f := range m.files {
		if f.UserID == userID && f.DeletedAt != nil {
			out = append(out, f)
		}
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

// UpdateFile renames and/or moves a file.
func (m *MemoryStore) UpdateFile(_ context.Context, userID, id uuid.UUID, name string, folderID *uuid.UUID) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.files[id]
	if !ok || f.UserID != userID || f.DeletedAt != nil {
		return File{}, skerr.ErrNotFound
	}
	for oid, other := range m.files {
		if oid == id {
			continue
		}
		if other.UserID == userID && other.DeletedAt == nil && other.Status == StatusReady &&
			other.Name == name && samePtr(other.FolderID, folderID) {
			return File{}, skerr.ErrConflict
		}
	}
	f.Name = name
	f.FolderID = folderID
	f.UpdatedAt = m.clock()
	m.files[id] = f
	return f, nil
}

// SoftDeleteFile moves a file to trash.
func (m *MemoryStore) SoftDeleteFile(_ context.Context, userID, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.UserID != userID || f.DeletedAt != nil {
		return 0, nil
	}
	t := m.clock()
	f.DeletedAt = &t
	f.UpdatedAt = t
	m.files[id] = f
	return 1, nil
}

// RestoreFile brings a file back from trash.
func (m *MemoryStore) RestoreFile(_ context.Context, userID, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.UserID != userID || f.DeletedAt == nil {
		return 0, nil
	}
	f.DeletedAt = nil
	f.UpdatedAt = m.clock()
	m.files[id] = f
	return 1, nil
}

// HardDeleteFile removes the row permanently.
func (m *MemoryStore) HardDeleteFile(_ context.Context, userID, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.UserID != userID {
		return 0, nil
	}
	delete(m.files, id)
	delete(m.shards, id)
	return 1, nil
}

// CreateShard records one manifest entry.
func (m *MemoryStore) CreateShard(_ context.Context, n NewShard) (Shard, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// file_shards_sizes_non_negative, in both real dialects
	// (00004_files.sql:78, sqlite/00003_files.sql:60). A fake store more
	// permissive than the schema lets a fixture build a state no real backend
	// can hold, and the suite then passes on a fiction — see the rule in
	// Memory.md.
	if n.SizeBytes < 0 || n.PlainSize < 0 || n.PlainOffset < 0 {
		return Shard{}, skerr.ErrValidation
	}
	for _, existing := range m.shards[n.FileID] {
		if existing.Index == n.Index {
			return Shard{}, skerr.ErrConflict
		}
	}
	sh := Shard{
		ID:          n.ID,
		FileID:      n.FileID,
		Index:       n.Index,
		AccountID:   n.AccountID,
		ProviderID:  n.ProviderID,
		SizeBytes:   n.SizeBytes,
		PlainSize:   n.PlainSize,
		PlainOffset: n.PlainOffset,
		SHA256:      n.SHA256,
		CreatedAt:   m.clock(),
	}
	m.shards[n.FileID] = append(m.shards[n.FileID], sh)
	return sh, nil
}

// ListShards returns a manifest in index order.
func (m *MemoryStore) ListShards(_ context.Context, fileID uuid.UUID) ([]Shard, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Shard(nil), m.shards[fileID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// DeleteShards removes a manifest.
func (m *MemoryStore) DeleteShards(_ context.Context, fileID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(len(m.shards[fileID]))
	delete(m.shards, fileID)
	return n, nil
}

// CorruptShard rewrites one manifest entry so the integrity paths can be
// exercised. Test support.
func (m *MemoryStore) CorruptShard(fileID uuid.UUID, index int32, mutate func(*Shard)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.shards[fileID] {
		if m.shards[fileID][i].Index == index {
			mutate(&m.shards[fileID][i])
			return
		}
	}
}

// FileStatus reports a file's current status. Test support.
func (m *MemoryStore) FileStatus(id uuid.UUID) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok {
		return "", false
	}
	return f.Status, true
}

// ListShardsSnapshot returns every manifest row in the store, so a test can
// assert that a failed upload left none behind. Test support.
func (m *MemoryStore) ListShardsSnapshot() []Shard {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Shard
	for _, list := range m.shards {
		out = append(out, list...)
	}
	return out
}

// ShardCount reports how many manifest rows a file has. Test support.
func (m *MemoryStore) ShardCount(fileID uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.shards[fileID])
}

func samePtr(a, b *uuid.UUID) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// Compile-time check.
var _ Store = (*MemoryStore)(nil)
