package output

import (
	"context"
	"sync"
	"time"
)

// ProgressBytesPerSecond is the rate at which each progress commit also buys
// a pause proportional to what it wrote (pi PROGRESS_BYTES_PER_SECOND).
const ProgressBytesPerSecond = 100 * 1024

// ProgressWaiter settles with the commit that includes its change (pi's
// markAndWait promise).
type ProgressWaiter struct {
	once sync.Once
	done chan struct{}
	err  error
}

func newProgressWaiter() *ProgressWaiter { return &ProgressWaiter{done: make(chan struct{})} }

// Resolve settles the waiter successfully.
func (w *ProgressWaiter) Resolve() { w.settle(nil) }

// Reject settles the waiter with err.
func (w *ProgressWaiter) Reject(err error) { w.settle(err) }

func (w *ProgressWaiter) settle(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.done)
	})
}

// Done is closed once the waiter settles.
func (w *ProgressWaiter) Done() <-chan struct{} { return w.done }

// Wait blocks until the waiter settles, returning the commit's error, or
// until ctx ends; ending the wait does not cancel the commit.
func (w *ProgressWaiter) Wait(ctx context.Context) error {
	select {
	case <-w.done:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clock is the time source Progress paces by; tests replace it.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Progress paces commits of a changing value: the first change after an idle
// period commits at once; each commit then delays the next by at least the
// minimum interval and by its written size at ProgressBytesPerSecond. At most
// one commit is in flight; changes made meanwhile coalesce into the next one.
// It is safe for concurrent use.
type Progress struct {
	write       func() (int, error)
	onError     func(error)
	minInterval time.Duration
	clock       clock

	mu        sync.Mutex
	waiters   []*ProgressWaiter
	stopTimer func() bool
	inFlight  chan struct{}
	nextAt    time.Time
	dirty     bool
	stopped   bool
}

// NewProgress makes a Progress. write commits the current value and returns
// how many bytes it wrote; it runs on its own goroutine. onError receives a
// failed commit's error.
func NewProgress(write func() (int, error), onError func(error), minInterval time.Duration) *Progress {
	return &Progress{write: write, onError: onError, minInterval: minInterval, clock: realClock{}}
}

// Mark schedules a commit.
func (p *Progress) Mark() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	p.scheduleLocked()
}

// MarkAndWait schedules a commit; the waiter settles with the commit that
// includes this change.
func (p *Progress) MarkAndWait() *ProgressWaiter {
	w := newProgressWaiter()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters = append(p.waiters, w)
	p.dirty = true
	p.scheduleLocked()
	return w
}

// Stop stops committing and waits for the commit in flight; it returns the
// waiters the final commit must settle.
func (p *Progress) Stop() []*ProgressWaiter {
	p.mu.Lock()
	p.stopped = true
	if p.stopTimer != nil {
		p.stopTimer()
		p.stopTimer = nil
	}
	inFlight := p.inFlight
	p.mu.Unlock()
	if inFlight != nil {
		<-inFlight
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	waiters := p.waiters
	p.waiters = nil
	return waiters
}

func (p *Progress) scheduleLocked() {
	if p.stopped || p.stopTimer != nil || p.inFlight != nil {
		return
	}
	wait := p.nextAt.Sub(p.clock.Now())
	if wait <= 0 {
		p.flushLocked()
		return
	}
	p.stopTimer = p.clock.AfterFunc(wait, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.stopTimer = nil
		p.flushLocked()
	})
}

func (p *Progress) flushLocked() {
	if p.stopped || !p.dirty {
		return
	}
	p.dirty = false
	waiters := p.waiters
	p.waiters = nil
	started := p.clock.Now()
	done := make(chan struct{})
	p.inFlight = done
	go func() {
		defer close(done)
		bytes, err := p.write()
		// pi's then: the pause, the waiters, the error report; then its
		// finally: the commit is no longer in flight, and changes made
		// meanwhile schedule the next one.
		p.mu.Lock()
		if err == nil {
			pause := max(p.minInterval, time.Duration(float64(bytes)*float64(time.Second)/ProgressBytesPerSecond))
			p.nextAt = started.Add(pause)
		} else {
			p.nextAt = started.Add(p.minInterval)
		}
		p.mu.Unlock()
		for _, w := range waiters {
			w.settle(err)
		}
		if err != nil && p.onError != nil {
			p.onError(err)
		}
		p.mu.Lock()
		p.inFlight = nil
		if p.dirty {
			p.scheduleLocked()
		}
		p.mu.Unlock()
	}()
}
