package router

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Known issue #7, now closed: two overlapping plans against a pool that can
// hold one must not both roll back.
//
// Before admitPlanning this failed reliably — 11 of 200 pairs left both uploads
// refused, because each claimed part of the pool shard by shard and then found
// nothing for the rest. The user saw two failures against a pool with room for
// one.
//
// 200 pairs, and every single pair must produce a winner. One pair is not
// evidence: the interleaving that caused this only shows up under repetition,
// which is exactly how the original bug hid behind a passing test.
func TestConcurrentPlanPairsAlwaysYieldAWinner(t *testing.T) {
	const pairs = 200

	for trial := 0; trial < pairs; trial++ {
		r, store := newReserver(t)
		acct := uuid.New()
		// Room for one 8 GiB upload, not two.
		store.AddAccount(acct, 1, "a@example.com", 10*gib, 0)

		p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners int
		)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := p.Plan(context.Background(), uuid.New(), 8*gib); err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if winners == 0 {
			t.Fatalf("pair %d: both plans were refused against a pool with room for one; "+
				"the mutual-rollback starvation is back", trial)
		}
		// Safety still holds: two winners would mean 16 GiB promised out of 10.
		if winners > 1 {
			t.Fatalf("pair %d: %d plans succeeded; capacity was oversubscribed", trial, winners)
		}
		if got := store.ReservedOn(acct); got != 8*gib {
			t.Fatalf("pair %d: reserved %d bytes for one 8 GiB winner, want %d",
				trial, got, 8*gib)
		}
	}
}

// The same guarantee when the file has to span drives, since striping is the
// case with the most reservations and so the most opportunity to interleave.
func TestConcurrentPlanPairsAlwaysYieldAWinnerWhenStriped(t *testing.T) {
	const pairs = 200

	for trial := 0; trial < pairs; trial++ {
		r, store := newReserver(t)
		ids := make([]uuid.UUID, 3)
		for i := range ids {
			ids[i] = uuid.New()
			// No single drive can hold 8 GiB; the pool can hold one.
			store.AddAccount(ids[i], int32(i+1), "d@example.com", 4*gib, 0)
		}

		p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners int
		)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := p.Plan(context.Background(), uuid.New(), 8*gib); err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if winners == 0 {
			t.Fatalf("striped pair %d: both plans were refused against a pool with room for one", trial)
		}
		if winners > 1 {
			t.Fatalf("striped pair %d: %d plans succeeded; capacity was oversubscribed", trial, winners)
		}

		var reserved int64
		for _, id := range ids {
			reserved += store.ReservedOn(id)
		}
		if reserved != 8*gib {
			t.Fatalf("striped pair %d: reserved %d across drives for one 8 GiB winner, want %d",
				trial, reserved, 8*gib)
		}
	}
}

// A caller that gives up while queued must not hold the slot behind it.
func TestPlanningAdmissionHonoursContextCancellation(t *testing.T) {
	r, store := newReserver(t)
	acct := uuid.New()
	store.AddAccount(acct, 1, "a@example.com", 100*gib, 0)
	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	userID := uuid.New()

	// Occupy the gate by hand, so the next acquire has to wait.
	release, err := p.admitPlanning(context.Background(), userID)
	if err != nil {
		t.Fatalf("admitPlanning() = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.admitPlanning(ctx, userID); err == nil {
		t.Fatal("a cancelled caller acquired the planning slot")
	}

	release()
	release() // releasing twice must not free the slot twice

	// The gate is usable again, and only once.
	again, err := p.admitPlanning(context.Background(), userID)
	if err != nil {
		t.Fatalf("admitPlanning() after release = %v", err)
	}
	again()
}

// Planning is serialised per user, so two different users never queue behind
// one another — their drives are disjoint and there is nothing to contend for.
func TestPlanningAdmissionIsPerUser(t *testing.T) {
	r, store := newReserver(t)
	store.AddAccount(uuid.New(), 1, "a@example.com", 100*gib, 0)
	p := NewPlanner(r, PolicyMostAvailable, shard256, noOverhead)

	held, err := p.admitPlanning(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("admitPlanning() = %v", err)
	}
	defer held()

	// A different user must not block on the first user's slot.
	done := make(chan struct{})
	go func() {
		other, oerr := p.admitPlanning(context.Background(), uuid.New())
		if oerr != nil {
			t.Errorf("admitPlanning() for another user = %v", oerr)
		} else {
			other()
		}
		close(done)
	}()

	<-done
}
