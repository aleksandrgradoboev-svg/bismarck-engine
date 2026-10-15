package ai

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// entryCount reports how many keys keyedLock is currently tracking. The map must
// not grow with every key ever locked (pi's chain cleanup).
func (k *keyedLock) entryCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}

// refCount reports key's outstanding reference count, or -1 when the key is gone.
func (k *keyedLock) refCount(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry := k.entries[key]
	if entry == nil {
		return -1
	}
	return entry.refs
}

// TestKeyedLockExcludesConcurrentHolders is the primitive's core guarantee: at
// most one caller holds a given key at a time, while different keys proceed in
// parallel.
func TestKeyedLockExcludesConcurrentHolders(t *testing.T) {
	k := newKeyedLock()
	const goroutines, iterations = 16, 50

	var inside atomic.Int32
	var overlaps atomic.Int32
	// One counter per key, each mutated only while that key is held. A shared
	// map would be a data race between the two keys regardless of the lock.
	keys := [2]string{"alpha", "beta"}
	var counts [2]int

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Two keys, so the test also exercises independent keys running at once.
			slot := g % 2
			key := keys[slot]
			for range iterations {
				if err := k.lock(context.Background(), key); err != nil {
					t.Errorf("lock: %v", err)
					return
				}
				if inside.Add(1) > 2 { // at most one holder per key, two keys total
					overlaps.Add(1)
				}
				// Unsynchronized access: the race detector proves the exclusion.
				counts[slot]++
				inside.Add(-1)
				k.unlock(key)
			}
		}(g)
	}
	wg.Wait()

	if n := overlaps.Load(); n != 0 {
		t.Fatalf("observed %d concurrent holders; the lock does not exclude", n)
	}
	want := goroutines / 2 * iterations
	if counts[0] != want || counts[1] != want {
		t.Fatalf("counts = %v, want %d each (lost updates)", counts, want)
	}
	if k.entryCount() != 0 {
		t.Fatalf("entries leaked after all work drained: %d", k.entryCount())
	}
}

// TestKeyedLockCancelWhileQueuedReleasesRef locks the refcount cleanup: a caller
// cancelled while queued must give its reference back, or the key's entry
// outlives its use and the map grows with every key ever locked.
func TestKeyedLockCancelWhileQueuedReleasesRef(t *testing.T) {
	k := newKeyedLock()
	const key = "provider"

	if err := k.lock(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := k.refCount(key); got != 1 {
		t.Fatalf("holder refs = %d, want 1", got)
	}

	const waiters = 8
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, waiters)
	queued := make(chan struct{}, waiters)
	for range waiters {
		go func() {
			queued <- struct{}{}
			errs <- k.lock(ctx, key)
		}()
	}
	for range waiters {
		<-queued
	}
	// Let the waiters reach the select before cancelling them.
	waitForRefs(t, k, key, waiters+1)

	cancel()
	for range waiters {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued waiter err = %v, want context.Canceled", err)
		}
	}

	if got := k.refCount(key); got != 1 {
		t.Fatalf("refs after cancelled waiters = %d, want 1 (only the holder)", got)
	}
	k.unlock(key)
	if k.entryCount() != 0 {
		t.Fatalf("entry not reclaimed after the last release: %d", k.entryCount())
	}
}

// TestKeyedLockNoLostWakeupOnCancelRace hammers the select in lock(): the send
// on the slot and the ctx cancellation become ready at the same moment, and Go
// picks a ready case at random. Either the waiter took the lock, or it did not —
// but the token must never be consumed by a waiter that reported an error (which
// would strand the key forever) nor dropped by one that reported success.
func TestKeyedLockNoLostWakeupOnCancelRace(t *testing.T) {
	for i := range 300 {
		k := newKeyedLock()
		const key = "racy"

		if err := k.lock(context.Background(), key); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		acquired := make(chan error, 1)
		start := make(chan struct{})
		go func() {
			<-start
			acquired <- k.lock(ctx, key)
		}()

		// Release the slot and cancel the waiter as simultaneously as possible.
		unlocked := make(chan struct{})
		cancelled := make(chan struct{})
		go func() { defer close(unlocked); <-start; k.unlock(key) }()
		go func() { defer close(cancelled); <-start; cancel() }()
		close(start)

		err := <-acquired
		// Both helpers must have settled before the map can be inspected.
		<-unlocked
		<-cancelled
		switch {
		case err == nil:
			// Took the lock: it must be genuinely held, so release it.
			k.unlock(key)
		case errors.Is(err, context.Canceled):
			// Did not take the lock: the slot must be free for the next caller.
		default:
			t.Fatalf("iteration %d: unexpected err %v", i, err)
		}
		cancel()

		// Whatever happened, the key must be usable and fully reclaimed.
		done := make(chan error, 1)
		go func() { done <- k.lock(context.Background(), key) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: relock failed: %v", i, err)
			}
			k.unlock(key)
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: key stranded — a token was lost in the select race", i)
		}
		if n := k.entryCount(); n != 0 {
			t.Fatalf("iteration %d: entries leaked: %d", i, n)
		}
	}
}

// TestKeyedLockCancelledCallerDoesNotHoldLock pins the documented contract: when
// lock returns an error the lock is NOT held, so another caller gets it.
func TestKeyedLockCancelledCallerDoesNotHoldLock(t *testing.T) {
	k := newKeyedLock()
	const key = "provider"

	if err := k.lock(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { cancelled <- k.lock(ctx, key) }()
	waitForRefs(t, k, key, 2)
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	k.unlock(key) // the original holder
	if err := k.lock(context.Background(), key); err != nil {
		t.Fatalf("lock after a cancelled waiter: %v", err)
	}
	k.unlock(key)
}

// TestKeyedLockAlreadyCancelledContext: lock must not acquire when the context is
// done on entry.
func TestKeyedLockAlreadyCancelledContext(t *testing.T) {
	k := newKeyedLock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := k.lock(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if k.entryCount() != 0 {
		t.Fatalf("a rejected lock must not create an entry: %d", k.entryCount())
	}
	// The key is still free.
	if err := k.lock(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	k.unlock("k")
}

// TestKeyedLockReleasedTokenAlwaysReachesAWaiter is the sharp form of "no lost
// wakeup": a second waiter stays queued on the same key throughout, so the key's
// entry survives the cancel and any token mishandled by the racing waiter is
// observable. Whatever the select picks, the surviving waiter must still get the
// lock — a token consumed by a caller that reported failure, or one stranded in
// the slot, would hang it forever.
func TestKeyedLockReleasedTokenAlwaysReachesAWaiter(t *testing.T) {
	for i := range 200 {
		k := newKeyedLock()
		const key = "racy"

		if err := k.lock(context.Background(), key); err != nil {
			t.Fatal(err)
		}

		// The surviving waiter: never cancelled, must eventually acquire.
		survivor := make(chan error, 1)
		go func() { survivor <- k.lock(context.Background(), key) }()

		// The racing waiter: cancelled at the same moment the holder releases.
		ctx, cancel := context.WithCancel(context.Background())
		racer := make(chan error, 1)
		go func() { racer <- k.lock(ctx, key) }()

		// Both waiters queued (holder + 2) before the race begins, so the racer
		// is genuinely parked in the select when the cancel lands.
		waitForRefs(t, k, key, 3)

		start := make(chan struct{})
		unlocked := make(chan struct{})
		go func() { defer close(unlocked); <-start; k.unlock(key) }()
		go func() { <-start; cancel() }()
		close(start)

		<-unlocked
		if err := <-racer; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: racer err = %v", i, err)
		} else if err == nil {
			k.unlock(key) // the racer won it; hand it on to the survivor
		}

		select {
		case err := <-survivor:
			if err != nil {
				t.Fatalf("iteration %d: survivor err = %v", i, err)
			}
			k.unlock(key)
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the released token never reached the waiting caller", i)
		}
		cancel()

		if n := k.entryCount(); n != 0 {
			t.Fatalf("iteration %d: entries leaked: %d", i, n)
		}
	}
}

func waitForRefs(t *testing.T, k *keyedLock, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k.refCount(key) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("refs for %q = %d, want %d", key, k.refCount(key), want)
}
