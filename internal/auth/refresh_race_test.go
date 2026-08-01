package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Deterministic reproduction of known issue #11.
//
// TestRefreshConcurrentUseRevokesFamily catches this 7-13% of runs under -race,
// which is not enough to verify a fix: a change that shifts timing without
// closing the window looks identical to a real fix on a lucky run, and so does
// no fix at all. This forces the failing interleaving every time.
//
// The window, from the Task 1 analysis: a winning refresh claims the presented
// token (service.go:214) and only later inserts its successor
// (service.go:237). A losing refresh can only reach revokeFamily after that
// claim has committed, so both loser paths land inside that one interval. If the
// loser's RevokeSessionFamily commits before the winner's insert, the UPDATE
// ... WHERE family_id = $1 AND revoked_at IS NULL enumerates only the original
// session, and the successor is inserted afterwards with revoked_at NULL. The
// security response fires, reports success, and leaves a live session chain.
//
// No production code is touched. Store is a consumer-declared interface
// (store.go:74), so the barrier is a decorator around whichever store is under
// test: it holds the winner's successor insert until the loser's family
// revocation has returned.
//
// This runs against SQLite too, which is not obviously safe: SQLite permits one
// writer at a time, so an interleaving that suspends one write mid-flight while
// another must commit could deadlock rather than pass or fail. It does not, and
// the reason is that the barrier blocks BETWEEN statements rather than inside a
// transaction. CreateSession waits before issuing its INSERT, holding no write
// lock while it waits, so the loser's revocation acquires the lock, commits and
// releases it; only then does the insert run. Had the barrier been placed
// inside a transaction that already held the write lock, this would deadlock
// until busy_timeout fired and the harness would report timedOut rather than
// silently passing -- which is what the timedOut guard below exists for.

// barrierStore forces the revoke-before-insert order.
//
// It is inert for anything that is not a rotation: only an insert carrying a
// PrevID waits, so Register's initial session is unaffected. Nothing here
// changes behaviour when the barrier is not armed.
type barrierStore struct {
	Store

	// revoked closes once RevokeSessionFamily has returned, which is the
	// loser committing its revocation.
	revoked chan struct{}
	once    sync.Once

	// timedOut records the barrier giving up rather than being released. A
	// deadlocked barrier must fail loudly as itself, not masquerade as the
	// bug under test.
	mu       sync.Mutex
	timedOut bool
	waited   bool
}

func newBarrierStore(inner Store) *barrierStore {
	return &barrierStore{Store: inner, revoked: make(chan struct{})}
}

// CreateSession holds a rotation's successor insert until the family
// revocation has happened.
func (b *barrierStore) CreateSession(ctx context.Context, n NewSession) (Session, error) {
	if n.PrevID == nil {
		// Not a rotation — the initial session from Register. Never block.
		return b.Store.CreateSession(ctx, n)
	}

	b.mu.Lock()
	b.waited = true
	b.mu.Unlock()

	select {
	case <-b.revoked:
		// The loser has revoked the family. Now insert, which is exactly the
		// order that strands the successor.
	case <-time.After(5 * time.Second):
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
	}

	return b.Store.CreateSession(ctx, n)
}

// RevokeSessionFamily releases the barrier once the revocation has been applied.
func (b *barrierStore) RevokeSessionFamily(ctx context.Context, familyID uuid.UUID) (int64, error) {
	n, err := b.Store.RevokeSessionFamily(ctx, familyID)
	b.once.Do(func() { close(b.revoked) })
	return n, err
}

func (b *barrierStore) state() (waited, timedOut bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waited, b.timedOut
}

// describeFamily renders every session in the family so the interleaving is
// legible from the failure output rather than reconstructed by hand.
func describeFamily(t *testing.T, store testStore, familyID uuid.UUID) {
	t.Helper()

	sessions := store.SessionsInFamily(familyID)
	t.Logf("family %s membership at assertion time (%d sessions):", familyID, len(sessions))
	for _, s := range sessions {
		t.Logf("  id=%s prev_id=%s used_at=%s revoked_at=%s",
			s.ID, fmtPrev(s.PrevID), fmtTime(s.UsedAt), fmtTime(s.RevokedAt))
	}
}

func fmtPrev(id *uuid.UUID) string {
	if id == nil {
		return "<nil, original login>"
	}
	return id.String()
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.Format(time.RFC3339Nano)
}

// Rules.md §2.8: "Presentation of an already-used refresh token revokes the
// entire family." Not the family as enumerated at that instant — the entire
// family. A successor inserted after the revocation is a family member that
// outlived it.
//
// This is the same invariant as service_test.go:277, forced rather than raced.
// That test stays exactly as it is: it is the check that the fix also holds
// under real timing, not only under this barrier.
func TestRefreshConcurrentUseRevokesFamilyDeterministically(t *testing.T) {
	// Runs against whichever backend the conformance harness selected, not
	// MemoryStore unconditionally: forcing this interleaving only proves
	// something about the store actually under test. Verified to engage under
	// both -- waited=true, timedOut=false on memory and on SQLite alike.
	store := newConformanceStore(t)
	barrier := newBarrierStore(store)
	svc := NewService(
		barrier,
		NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute),
		720*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rerr := svc.Refresh(ctx, first.RefreshToken, testMeta()); rerr == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// The barrier has to have done its job, or the run proves nothing about
	// ordering either way.
	waited, timedOut := barrier.state()
	if !waited {
		t.Fatal("the barrier never engaged: no rotation insert happened, so no interleaving was forced")
	}
	if timedOut {
		t.Fatal("the barrier timed out waiting for the family revocation: " +
			"this is a deadlocked harness, not the bug under test")
	}

	// Unchanged from the probabilistic test: exactly one winner, and the loser
	// is treated as reuse. If this fails the harness has changed the outcome
	// rather than only the order.
	if okCount != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want exactly 1", okCount)
	}

	family, ok := store.SessionByID(first.SessionID)
	if !ok {
		t.Fatalf("session %s vanished", first.SessionID)
	}

	// Behaviour, not row state. Under the forced interleaving the successor is
	// inserted after the revocation, so a correct implementation leaves its
	// revoked_at nil and makes it unclaimable instead. Asserting revoked_at
	// here would fail against working code.
	sessions := store.SessionsInFamily(family.FamilyID)
	var claimable []Session
	for _, s := range sessions {
		if _, cerr := store.ClaimSession(ctx, s.ID); cerr == nil {
			claimable = append(claimable, s)
		}
	}

	if len(claimable) > 0 {
		describeFamily(t, store, family.FamilyID)
		for _, s := range claimable {
			role := "the ORIGINAL login session"
			if s.PrevID != nil {
				role = fmt.Sprintf("a ROTATED SUCCESSOR of %s", s.PrevID)
			}
			t.Errorf("session %s is still CLAIMABLE after the family revocation: %s "+
				"(prev_id=%s used_at=%s revoked_at=%s)",
				s.ID, role, fmtPrev(s.PrevID), fmtTime(s.UsedAt), fmtTime(s.RevokedAt))
		}
		t.Errorf("Rules.md §2.8 requires reuse to revoke the ENTIRE family; "+
			"%d of %d sessions can still refresh after revocation",
			len(claimable), len(sessions))
	}

	// The mechanism, asserted alongside the property so a change that keeps one
	// without the other cannot pass.
	revokedAt, ok := store.FamilyRevokedAt(family.FamilyID)
	if !ok {
		t.Fatalf("family %s has no token_families row", family.FamilyID)
	}
	if revokedAt == nil {
		describeFamily(t, store, family.FamilyID)
		t.Errorf("family %s carries no revoked_at, so nothing binds a successor "+
			"inserted after the sweep", family.FamilyID)
	}
}
