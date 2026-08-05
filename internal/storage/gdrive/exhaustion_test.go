package gdrive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/storage"
)

// WHAT DOES AN EXHAUSTED RETRY ACTUALLY SURFACE AS?
//
// This is the first task of Block 7 and it is deliberately a test rather than
// a reading of the code. Reconcile's three-state classification hangs on the
// answer: if an exhausted rate-limit retry were indistinguishable from
// ErrObjectNotFound, reconcile would flag healthy files as corrupted the first
// time Drive throttled it — the worst possible failure for this feature.
//
// The answer, pinned here so it cannot drift: the pool wraps the LAST error,
// so an exhausted retry still matches storage.ErrRateLimited and NEVER matches
// storage.ErrObjectNotFound.
func TestExhaustedRetrySurfacesAsRateLimitedNotNotFound(t *testing.T) {
	p := NewPool(
		withSleep(func(context.Context, time.Duration) error { return nil }),
		WithMaxAttempts(3),
	)

	err := p.Do(context.Background(), func(context.Context) error {
		return &RateLimitError{cause: errors.New("slow down")}
	})

	if err == nil {
		t.Fatal("Do() = nil against a permanently rate-limited backend")
	}

	// THE LOAD-BEARING ASSERTION for reconcile.
	if errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal("an exhausted retry matches ErrObjectNotFound; reconcile would " +
			"flag a healthy file as missing because Drive throttled us")
	}
	if !errors.Is(err, storage.ErrRateLimited) {
		t.Errorf("error = %v, want it to still match ErrRateLimited so the "+
			"caller can classify it as indeterminate rather than missing", err)
	}
	// And it says it gave up, so the cause is legible in a log.
	if !errors.Is(err, storage.ErrRateLimited) {
		t.Errorf("exhaustion lost the rate-limit sentinel: %v", err)
	}
}

// A genuine 404 is distinguishable, and is NOT retried.
func TestNotFoundIsDistinguishableFromExhaustion(t *testing.T) {
	p := NewPool(
		withSleep(func(context.Context, time.Duration) error { return nil }),
		WithMaxAttempts(3),
	)

	calls := 0
	err := p.Do(context.Background(), func(context.Context) error {
		calls++
		return storage.ErrObjectNotFound
	})

	if !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("error = %v, want ErrObjectNotFound", err)
	}
	if errors.Is(err, storage.ErrRateLimited) {
		t.Error("a genuine 404 also matches ErrRateLimited; the two states " +
			"would be indistinguishable")
	}
	if calls != 1 {
		t.Errorf("%d calls for a 404; it must not be retried", calls)
	}
}

// Cancellation and transport failure are also NOT ErrObjectNotFound, so they
// classify as indeterminate rather than missing.
func TestOtherFailuresAreNotMistakenForMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport failure", errors.New("connection reset by peer")},
		{"context cancelled", context.Canceled},
		{"deadline exceeded", context.DeadlineExceeded},
		{"revoked grant", storage.ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPool(withSleep(func(context.Context, time.Duration) error { return nil }))
			err := p.Do(context.Background(), func(context.Context) error { return tc.err })
			if errors.Is(err, storage.ErrObjectNotFound) {
				t.Errorf("%v matches ErrObjectNotFound; reconcile would flag a "+
					"healthy file because it could not reach the drive", tc.err)
			}
		})
	}
}
