package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// Jobs run one at a time and in the order they were submitted; that ordering is
// what keeps a burst of session events reaching a peer in the order the runtime
// emitted them.
func TestSerialQueueRunsJobsInOrder(t *testing.T) {
	t.Parallel()
	var queue serialQueue
	var mu sync.Mutex
	var order []int
	var concurrent, maxConcurrent int

	for i := range 64 {
		queue.Go(func() {
			mu.Lock()
			concurrent++
			maxConcurrent = max(maxConcurrent, concurrent)
			order = append(order, i)
			concurrent--
			mu.Unlock()
		})
	}
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", maxConcurrent)
	}
	if len(order) != 64 {
		t.Fatalf("ran %d jobs, want 64", len(order))
	}
	for i, ran := range order {
		if ran != i {
			t.Fatalf("order = %v, want ascending", order)
		}
	}
}

// A queue that has already drained can be used again, which is what happens
// every time a session goes quiet and then busy.
func TestSerialQueueRestartsAfterDraining(t *testing.T) {
	t.Parallel()
	var queue serialQueue
	done := make(chan struct{}, 2)
	queue.Go(func() { done <- struct{}{} })
	queue.Wait()
	queue.Go(func() { done <- struct{}{} })
	queue.Wait()
	if len(done) != 2 {
		t.Fatalf("ran %d jobs, want 2", len(done))
	}
}

// Wait must not miss work submitted while it is already waiting.
func TestSerialQueueWaitCoversLateSubmissions(t *testing.T) {
	t.Parallel()
	var queue serialQueue
	var mu sync.Mutex
	ran := 0
	release := make(chan struct{})

	queue.Go(func() {
		<-release
		mu.Lock()
		ran++
		mu.Unlock()
	})
	queue.Go(func() {
		mu.Lock()
		ran++
		mu.Unlock()
	})
	close(release)
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if ran != 2 {
		t.Fatalf("ran = %d, want 2", ran)
	}
}

// A waiter that has been given a deadline must return by it. Shutdown waits on
// these queues, and the jobs on them call into code this package does not own.
func TestSerialQueueWaitContextGivesUp(t *testing.T) {
	t.Parallel()
	var queue serialQueue
	release := make(chan struct{})
	queue.Go(func() { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := queue.WaitContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline that was set", err)
	}

	// Giving up on the wait does not stop the queue: the job still runs.
	close(release)
	queue.Wait()
}

// Executed jobs must not be pinned by the queue's backing array: a long-lived
// session's queue would otherwise hold every closure it has ever run.
func TestSerialQueueReleasesExecutedJobs(t *testing.T) {
	t.Parallel()
	var queue serialQueue
	started, release := make(chan struct{}), make(chan struct{})
	queue.Go(func() {
		close(started)
		<-release
	})
	<-started
	queue.Go(func() {})

	// Captured while the queue is stalled on the first job, so this is the
	// array the second job lands in. Reslicing past it after it has run would
	// hide the reference without dropping it.
	queue.mu.Lock()
	backing := queue.jobs[:cap(queue.jobs)]
	queue.mu.Unlock()
	if len(backing) == 0 {
		t.Fatal("expected the queued job to be visible in the backing array")
	}

	close(release)
	queue.Wait()

	queue.mu.Lock()
	defer queue.mu.Unlock()
	for i, job := range backing {
		if job != nil {
			t.Fatalf("executed job %d is still referenced by the queue's backing array, "+
				"pinning it and everything it captured", i)
		}
	}
}

func TestNewUUIDShape(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for range 1000 {
		id := newUUID()
		if len(id) != 36 {
			t.Fatalf("id %q is not 36 characters", id)
		}
		parts := strings.Split(id, "-")
		if len(parts) != 5 ||
			len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 ||
			len(parts[3]) != 4 || len(parts[4]) != 12 {
			t.Fatalf("id %q is not in canonical form", id)
		}
		if parts[2][0] != '4' {
			t.Fatalf("id %q is not version 4", id)
		}
		if !strings.ContainsRune("89ab", rune(parts[3][0])) {
			t.Fatalf("id %q does not carry the RFC 4122 variant", id)
		}
		if strings.ToLower(id) != id {
			t.Fatalf("id %q is not lowercase", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("id %q was generated twice", id)
		}
		seen[id] = struct{}{}
	}
}

// An InternalError's own message names its cause. Not every reporter unwraps:
// a snapshot broadcast (snapshots.go) and a queued session job (sessions.go)
// hand the observer the wrapper itself, and an Error() that returned only the
// sanitized constant would lose the failure entirely. The sanitization belongs
// to the wire formatter, which never asks the error for its text — this pins
// both halves at once.
func TestInternalErrorNamesItsCauseButTheWireStaysSanitized(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("private storage detail: %w", errors.New("private root cause"))
	wrapper := NewInternalError(cause)

	if !strings.Contains(wrapper.Error(), "private storage detail") ||
		!strings.Contains(wrapper.Error(), "private root cause") {
		t.Fatalf("the observer must be able to read the cause off the wrapper, got %q", wrapper.Error())
	}

	var reported []error
	s := &Server{onError: func(err error) { reported = append(reported, err) }}
	protoErr := s.toProtocolError(wrapper)
	if protoErr.Code != protocol.ErrorInternal {
		t.Fatalf("code = %q, want internal_error", protoErr.Code)
	}
	if protoErr.Message != InternalErrorMessage {
		t.Fatalf("message = %q, want the sanitized constant", protoErr.Message)
	}
	if strings.Contains(protoErr.Message, "private") {
		t.Fatalf("private detail reached the peer: %q", protoErr.Message)
	}
	if len(reported) != 1 || !errors.Is(reported[0], cause) {
		t.Fatalf("the cause must reach the observer, got %v", reported)
	}
}

// A nil cause has nothing to name, so the wrapper falls back to the sanitized
// constant rather than reading "internal server error: %!w(<nil>)".
func TestInternalErrorWithNoCauseReadsAsTheConstant(t *testing.T) {
	t.Parallel()
	if got := NewInternalError(nil).Error(); got != InternalErrorMessage {
		t.Fatalf("Error() = %q, want %q", got, InternalErrorMessage)
	}
}
