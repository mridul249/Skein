package auth

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Deterministic reproduction of known issue #18.
//
// The claim this file exists to make is a CONCURRENCY claim, so a sequential
// test proves nothing here. RevokeAllUserSessions is sound against any
// sequential ordering — it fails only against a refresh that has already
// claimed its parent and has not yet inserted its successor. A test that
// changes the password and then refreshes cannot express that state at all,
// and would go green against the unsound implementation.
//
// The window: Refresh claims the presented token (ClaimSession) and only later
// inserts the successor (CreateSession). A revocation landing inside that
// interval is invisible to a row-enumerating sweep — the successor does not
// exist yet to be swept, and it is inserted afterwards carrying revoked_at
// NULL. The epoch closes it by making validity something the successor
// INHERITS from the parent it claimed, rather than something it re-reads at
// insert time: the parent's epoch is stale by then, so the successor is born
// stale and ClaimSession refuses it.
//
// The barrier is a decorator over the consumer-declared Store interface, so no
// production code is touched or aware of it. The same reasoning about SQLite
// as in refresh_race_test.go applies: the barrier blocks BETWEEN statements,
// holding no write lock while it waits, so a single-writer engine cannot
// deadlock on it. The timedOut guard makes a deadlock report as itself rather
// than passing quietly.

// epochBarrierStore holds a rotation's successor insert until a password
// change has bumped the epoch.
//
// Inert for anything that is not a rotation: only an insert carrying a PrevID
// waits, so Register's and Login's initial sessions are unaffected.
type epochBarrierStore struct {
	Store

	// bumped closes once the epoch has been bumped, i.e. once the password
	// change has committed its revocation.
	bumped chan struct{}
	once   sync.Once

	// claimed closes once a rotation has claimed its parent, which is the
	// point after which the revocation must land for the race to be real.
	claimed   chan struct{}
	claimOnce sync.Once

	mu       sync.Mutex
	waited   bool
	timedOut bool
}

func newEpochBarrierStore(inner Store) *epochBarrierStore {
	return &epochBarrierStore{
		Store:   inner,
		bumped:  make(chan struct{}),
		claimed: make(chan struct{}),
	}
}

// ClaimSession announces that the rotation has taken its parent. Everything
// after this point is inside the window under test.
func (b *epochBarrierStore) ClaimSession(ctx context.Context, id uuid.UUID) (Session, error) {
	s, err := b.Store.ClaimSession(ctx, id)
	if err == nil {
		b.claimOnce.Do(func() { close(b.claimed) })
	}
	return s, err
}

// CreateSession holds the successor insert until the epoch has been bumped,
// which is exactly the interleaving that strands a successor under a
// row-enumerating revocation.
func (b *epochBarrierStore) CreateSession(ctx context.Context, n NewSession) (Session, error) {
	if n.PrevID == nil {
		return b.Store.CreateSession(ctx, n) // initial login, never blocks
	}

	b.mu.Lock()
	b.waited = true
	b.mu.Unlock()

	select {
	case <-b.bumped:
		// The revocation has committed. Insert now.
	case <-time.After(5 * time.Second):
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
	}
	return b.Store.CreateSession(ctx, n)
}

// BumpUserSessionEpoch releases the barrier once the revocation is applied.
func (b *epochBarrierStore) BumpUserSessionEpoch(ctx context.Context, userID uuid.UUID) (int64, error) {
	epoch, err := b.Store.BumpUserSessionEpoch(ctx, userID)
	b.once.Do(func() { close(b.bumped) })
	return epoch, err
}

func (b *epochBarrierStore) state() (waited, timedOut bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waited, b.timedOut
}

func newEpochTestService(t *testing.T, store Store) *Service {
	t.Helper()
	return NewService(
		store,
		NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute),
		720*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// THE PROPERTY: a refresh that is already in flight when the password changes
// must not produce a usable session.
//
// The successor is inserted AFTER the revocation and therefore carries
// revoked_at NULL — its own row says nothing about being dead. What kills it is
// the epoch it inherited from the parent it claimed, which the revocation has
// since superseded.
func TestPasswordChangeRevokesASessionRotatingConcurrently(t *testing.T) {
	store := newConformanceStore(t)
	barrier := newEpochBarrierStore(store)
	svc := newEpochTestService(t, barrier)
	ctx := context.Background()

	owner, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	// A second device: an independent login, so an independent family. This is
	// the session the password change must kill.
	other, err := svc.Login(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}

	var (
		wg        sync.WaitGroup
		rotated   TokenPair
		rotateErr error
	)

	// The in-flight refresh. It will claim its parent, then block in
	// CreateSession until the password change has bumped the epoch.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rotated, rotateErr = svc.Refresh(ctx, other.RefreshToken, testMeta())
	}()

	// Wait until the rotation has actually claimed, so the revocation is
	// guaranteed to land inside the window rather than before it. Without this
	// the test could pass for the trivial reason that the sweep saw the parent.
	select {
	case <-barrier.claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation never claimed its parent; the barrier never armed")
	}

	if _, cerr := svc.ChangePassword(ctx, owner.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}
	wg.Wait()

	waited, timedOut := barrier.state()
	if !waited {
		t.Fatal("the barrier never engaged: the successor insert did not wait, " +
			"so this run did not exercise the race it claims to")
	}
	if timedOut {
		t.Fatal("the barrier timed out rather than being released: " +
			"the interleaving deadlocked and this run proves nothing")
	}

	// The rotation may legitimately fail outright (the claim itself refused).
	// If it succeeded, its successor was inserted after the revocation — and
	// that successor must not be usable.
	if rotateErr != nil {
		t.Logf("the in-flight rotation failed at claim time: %v", rotateErr)
		return
	}
	t.Logf("the in-flight rotation completed and produced a successor; "+
		"it must now be unusable (session %s)", rotated.SessionID)

	if _, err := svc.Refresh(ctx, rotated.RefreshToken, testMeta()); err == nil {
		t.Fatal("a session created by a refresh racing a password change is STILL USABLE: " +
			"the successor outlived the revocation, which is issue #18 exactly")
	}
}

// The ordinary, non-racing case: a password change signs other devices out.
//
// Kept separate from the race above because they fail for different reasons and
// a combined test would not say which broke.
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	owner, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	other, err := svc.Login(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}

	if _, cerr := svc.ChangePassword(ctx, owner.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}

	if _, err := svc.Refresh(ctx, other.RefreshToken, testMeta()); err == nil {
		t.Error("the other device's refresh token still works after a password change")
	}
}
