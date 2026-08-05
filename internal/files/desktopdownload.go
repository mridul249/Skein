//go:build desktop

// Package-level note: this file is compiled ONLY into the desktop binary.
//
// The build tag is the security boundary, not a runtime check. The route this
// backs writes to an arbitrary path on the machine running the server, which
// on desktop is the user's own machine and on a hosted server is somebody
// else's. TestServerBinaryHasNoDesktopDownloadRoute inspects the built server
// binary rather than trusting this comment.
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// DownloadState is where a transfer has got to.
type DownloadState string

const (
	DownloadRunning   DownloadState = "running"
	DownloadComplete  DownloadState = "complete"
	DownloadFailed    DownloadState = "failed"
	DownloadCancelled DownloadState = "cancelled"
)

// DesktopDownload is one transfer, as reported to the UI.
type DesktopDownload struct {
	ID     string        `json:"id"`
	FileID uuid.UUID     `json:"file_id"`
	Name   string        `json:"name"`
	Path   string        `json:"path"`
	State  DownloadState `json:"state"`

	Done  int64 `json:"done"`
	Total int64 `json:"total"`
	// BytesPerSec is smoothed; 0 until a rate can be measured.
	BytesPerSec int64 `json:"bytes_per_sec"`
	// ETASeconds is -1 when it cannot be estimated. The UI renders that blank,
	// never "0s", which would read as "finished".
	ETASeconds int64 `json:"eta_seconds"`
	// Err is a user-safe message. Empty unless State is failed.
	Err string `json:"error,omitempty"`
}

// DownloadManager runs Go-side downloads for the desktop build.
//
// The bytes stream through this process straight to disk, which is what makes
// real progress, real cancellation and in-band error reporting possible — all
// three are impossible on the browser's a.click() path, where the webview owns
// the transfer and tells JS nothing (known issue #15).
//
// MEMORY: constant, and asserted by TestDesktopDownloadPeakRSSIsFlat rather
// than by inspection. #15's off-heap property used to hold structurally,
// because the bytes never entered this process at all. On this path it holds
// only by discipline — one io.CopyBuffer with a fixed buffer, no ReadAll, no
// bytes.Buffer — so that test is the entirety of what replaces the old
// structural guarantee. Do not weaken it.
type DownloadManager struct {
	svc *Service

	mu        sync.Mutex
	downloads map[string]*downloadEntry
	seq       int
}

type downloadEntry struct {
	// userID is the user who started this transfer, and the only one who may
	// see, watch or cancel it. Start already had this and discarded it, so
	// every later operation keyed on the download id alone — and those ids are
	// sequential, so they are guessed rather than discovered.
	userID   uuid.UUID
	snapshot DesktopDownload
	// ctx is the download's own context. Cancellation is decided by asking
	// it, not by inspecting the error: the provider's HTTP stack replaces a
	// cancelled request's error with its own ("connection reset", "stream
	// error"), so errors.Is(err, context.Canceled) is false by the time the
	// copy returns. Observed live 2026-08-05 — a cancelled download reported
	// "failed / check your connection" instead of "cancelled".
	ctx    context.Context
	cancel context.CancelFunc
	// subs are the SSE listeners. A download with no listener keeps running:
	// the transfer is not owned by the connection watching it.
	subs map[chan DesktopDownload]struct{}
	done bool
}

// NewDownloadManager builds the manager.
func NewDownloadManager(svc *Service) *DownloadManager {
	return &DownloadManager{svc: svc, downloads: map[string]*downloadEntry{}}
}

// ResolveTarget validates a save location and returns the absolute file path
// to write.
//
// dir is a SERVER-SIDE WRITE TARGET. Even though the server is the user's own
// machine on desktop, the path arrives over HTTP and is treated as untrusted:
// the name is reduced to its base, the result must stay inside dir after
// symlink resolution, and dir itself must exist and be writable BEFORE any
// byte moves — a transfer that fails on the final write has already spent the
// whole download.
func ResolveTarget(root, dir, name string) (string, error) {
	if dir == "" {
		dir = root
	}
	if dir == "" {
		return "", skerr.Public(skerr.ErrValidation,
			"No download folder is configured.")
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", skerr.Public(skerr.ErrValidation, "That download folder is not a valid path.")
	}

	info, err := os.Stat(absDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", skerr.Public(skerr.ErrValidation,
				"The download folder %s does not exist.", absDir)
		}
		return "", skerr.Public(skerr.ErrValidation,
			"The download folder %s cannot be read.", absDir)
	}
	if !info.IsDir() {
		return "", skerr.Public(skerr.ErrValidation,
			"%s is not a folder.", absDir)
	}

	// Writability, checked up front rather than discovered at the end.
	probe, err := os.CreateTemp(absDir, ".skein-write-probe-*")
	if err != nil {
		return "", skerr.Public(skerr.ErrValidation,
			"The download folder %s is not writable.", absDir)
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	// filepath.Base strips every directory component, so "../../etc/passwd"
	// and "/etc/passwd" both reduce to "passwd". The file NAME comes from the
	// database, but it is user-supplied data and is treated as such.
	base := filepath.Base(filepath.Clean("/" + name))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "download"
	}

	target := uniquePath(filepath.Join(absDir, base))

	// Belt and braces after symlink resolution: the directory may itself be a
	// link, so compare resolved forms.
	realDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		realDir = absDir
	}
	if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(realDir)+string(filepath.Separator)) &&
		filepath.Dir(target) != absDir {
		return "", skerr.Public(skerr.ErrValidation,
			"That download location is not allowed.")
	}
	return target, nil
}

// uniquePath appends " (2)", " (3)" … until the path is free, matching what
// every browser does rather than silently overwriting.
func uniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 2; i < 10000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext)
}

// Start begins a download and returns its id immediately.
//
// The file is checked for reachable shards BEFORE anything is created on disk,
// so a damaged file fails as an error rather than as a truncated file.
func (m *DownloadManager) Start(ctx context.Context, userID, fileID uuid.UUID, root, dir string) (DesktopDownload, error) {
	file, err := m.svc.Get(ctx, userID, fileID)
	if err != nil {
		return DesktopDownload{}, err
	}
	// Damaged files never start. Without this the transfer runs until it hits
	// the missing shard and leaves a partial file behind.
	if cerr := m.svc.CheckReadable(ctx, userID, fileID); cerr != nil {
		return DesktopDownload{}, cerr
	}

	target, err := ResolveTarget(root, dir, file.Name)
	if err != nil {
		return DesktopDownload{}, err
	}

	// Detached from the request: the download outlives the HTTP call that
	// started it, and must not die when that response is written.
	dlCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("dl-%d", m.seq)
	entry := &downloadEntry{
		userID: userID,
		snapshot: DesktopDownload{
			ID: id, FileID: fileID, Name: file.Name, Path: target,
			State: DownloadRunning, Total: file.SizeBytes, ETASeconds: -1,
		},
		ctx:    dlCtx,
		cancel: cancel,
		subs:   map[chan DesktopDownload]struct{}{},
	}
	m.downloads[id] = entry
	m.mu.Unlock()

	go m.run(dlCtx, id, userID, fileID, target)

	return entry.snapshot, nil
}

// run performs the transfer. Every exit path either completes the file or
// removes it.
func (m *DownloadManager) run(ctx context.Context, id string, userID, fileID uuid.UUID, target string) {
	defer m.finishRunning(id)

	content, err := m.svc.Open(ctx, userID, fileID, nil)
	if err != nil {
		m.fail(id, target, err)
		return
	}
	defer func() { _ = content.Body.Close() }()

	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		m.fail(id, target, fmt.Errorf("create download file: %w", err))
		return
	}

	// ProgressReader wraps the read, feeding throttled samples to the SSE
	// subscribers. It counts and forwards; it never buffers.
	pr := NewProgressReader(ctx, content.Body, func(p DownloadProgress) {
		m.publishProgress(id, p)
	}, DownloadProgress{TransferID: id, FileID: fileID.String(), Total: content.TotalSize})

	buf := make([]byte, storage.CopyBufferSize)
	_, copyErr := io.CopyBuffer(f, pr, buf)
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		// Cancellation and failure both remove the partial file. A truncated
		// file under the real filename is worse than no file: it looks like a
		// download that worked.
		m.fail(id, target, copyErr)
	case closeErr != nil:
		m.fail(id, target, fmt.Errorf("finish writing: %w", closeErr))
	default:
		m.complete(id)
	}
}

func (m *DownloadManager) fail(id, target string, cause error) {
	// Remove the partial file on EVERY failure path, cancellation included.
	if target != "" {
		_ = os.Remove(target)
	}

	m.mu.Lock()
	entry, ok := m.downloads[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	// Ask the context, then fall back to the error chain. The context is
	// authoritative: it is cancelled iff the user asked, whatever the
	// transport turned the error into on the way back.
	// Ask the context, then fall back to the error chain. The context is
	// authoritative: it is cancelled iff the user asked, whatever the
	// transport turned the error into on the way back.
	cancelled := errors.Is(cause, context.Canceled) ||
		(entry.ctx != nil && entry.ctx.Err() != nil)
	entry.snapshot.State = DownloadFailed
	if cancelled {
		entry.snapshot.State = DownloadCancelled
	}
	entry.snapshot.ETASeconds = 0
	entry.snapshot.Err = userMessage(cause, cancelled)
	snap := entry.snapshot
	m.mu.Unlock()

	m.publish(id, snap)
}

// userMessage turns an internal error into something safe and useful.
func userMessage(cause error, cancelled bool) string {
	switch {
	case cancelled:
		return ""
	case errors.Is(cause, skerr.ErrIntegrity):
		// The damaged-file case, hit mid-transfer when a shard disappears
		// between the pre-flight check and the read.
		var pub *skerr.PublicError
		if errors.As(cause, &pub) && pub.Message != "" {
			return pub.Message
		}
		return "This file is damaged: a shard is missing from its drive."
	case errors.Is(cause, skerr.ErrDriveNeedsReconnect):
		return "A drive holding this file needs reconnecting."
	case errors.Is(cause, skerr.ErrRateLimited):
		return "A drive is rate limiting. Try again shortly."
	default:
		return "The download failed. Check your connection and try again."
	}
}

func (m *DownloadManager) complete(id string) {
	m.mu.Lock()
	entry, ok := m.downloads[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	entry.snapshot.State = DownloadComplete
	entry.snapshot.Done = entry.snapshot.Total
	entry.snapshot.ETASeconds = 0
	snap := entry.snapshot
	m.mu.Unlock()

	m.publish(id, snap)
}

func (m *DownloadManager) publishProgress(id string, p DownloadProgress) {
	m.mu.Lock()
	entry, ok := m.downloads[id]
	if !ok || entry.snapshot.State != DownloadRunning {
		m.mu.Unlock()
		return
	}
	entry.snapshot.Done = p.Done
	entry.snapshot.BytesPerSec = p.BytesPerSec
	entry.snapshot.ETASeconds = p.ETASeconds
	snap := entry.snapshot
	m.mu.Unlock()

	m.publish(id, snap)
}

// publish sends a snapshot to every subscriber, dropping it for any that is
// not keeping up rather than blocking the transfer on a slow reader.
func (m *DownloadManager) publish(id string, snap DesktopDownload) {
	m.mu.Lock()
	entry, ok := m.downloads[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	subs := make([]chan DesktopDownload, 0, len(entry.subs))
	for ch := range entry.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
			// A subscriber that cannot keep up misses this sample. It will get
			// the next one, and the heartbeat carries the current state
			// regardless — progress is a snapshot, not a log.
		}
	}
}

func (m *DownloadManager) finishRunning(id string) {
	m.mu.Lock()
	if entry, ok := m.downloads[id]; ok {
		entry.done = true
	}
	m.mu.Unlock()
}

// lookup resolves a download the caller owns.
//
// A download belonging to somebody else is reported exactly as one that does
// not exist. NOT a 403: a 403 confirms the transfer is there, which is the
// fact being protected — the same rule Get/Open/Delete follow for files.
//
// The caller must hold m.mu.
func (m *DownloadManager) lookup(userID uuid.UUID, id string) (*downloadEntry, bool) {
	entry, ok := m.downloads[id]
	if !ok || entry.userID != userID {
		return nil, false
	}
	return entry, true
}

// Cancel stops a transfer. The partial file is removed by run's failure path.
func (m *DownloadManager) Cancel(userID uuid.UUID, id string) error {
	m.mu.Lock()
	entry, ok := m.lookup(userID, id)
	m.mu.Unlock()
	if !ok {
		return skerr.ErrNotFound
	}
	entry.cancel()
	return nil
}

// Get returns one download's current state.
func (m *DownloadManager) Get(userID uuid.UUID, id string) (DesktopDownload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.lookup(userID, id)
	if !ok {
		return DesktopDownload{}, false
	}
	return entry.snapshot, true
}

// List returns the caller's own downloads, newest last. Never anybody else's.
func (m *DownloadManager) List(userID uuid.UUID) []DesktopDownload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DesktopDownload, 0, len(m.downloads))
	for _, e := range m.downloads {
		if e.userID != userID {
			continue
		}
		out = append(out, e.snapshot)
	}
	return out
}

// Subscribe returns a channel of snapshots and an unsubscribe function.
//
// RECONNECT STORY: the transfer is owned by the manager, never by the
// connection watching it. A dropped EventSource cancels nothing — the download
// continues, and the client re-attaches by subscribing again. The first thing
// a new subscriber receives is the CURRENT snapshot, so a client that missed
// samples while disconnected is immediately correct rather than replaying
// history it does not need.
func (m *DownloadManager) Subscribe(userID uuid.UUID, id string) (<-chan DesktopDownload, func(), bool) {
	m.mu.Lock()
	entry, ok := m.lookup(userID, id)
	if !ok {
		m.mu.Unlock()
		return nil, nil, false
	}
	// Buffered: publish never blocks the transfer on a slow reader.
	ch := make(chan DesktopDownload, 4)
	entry.subs[ch] = struct{}{}
	current := entry.snapshot
	m.mu.Unlock()

	// Deliver the current state immediately, which is what makes re-attaching
	// after a dropped connection correct.
	ch <- current

	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e, still := m.downloads[id]; still {
			delete(e.subs, ch)
		}
	}, true
}
