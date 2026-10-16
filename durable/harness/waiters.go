package harness

import (
	"context"
	"sync"
)

// waiter is one pending wait; it settles once.
type waiter[T any] struct {
	once  sync.Once
	done  chan struct{}
	value T
	err   error
}

func (w *waiter[T]) settle(value T, err error) {
	w.once.Do(func() {
		w.value, w.err = value, err
		close(w.done)
	})
}

// waiters are pending waits by key, pi's Waiters. Each settles once: through
// resolve, rejectAll, or cancellation of its context. Their owner guards them
// with its own mutex; wait runs without it.
type waiters[K comparable, T any] struct {
	sets map[K]map[*waiter[T]]struct{}
}

func newWaiters[K comparable, T any]() *waiters[K, T] {
	return &waiters[K, T]{sets: map[K]map[*waiter[T]]struct{}{}}
}

// add registers a wait for key; the caller holds the owner's mutex.
func (ws *waiters[K, T]) add(key K) *waiter[T] {
	w := &waiter[T]{done: make(chan struct{})}
	set := ws.sets[key]
	if set == nil {
		set = map[*waiter[T]]struct{}{}
		ws.sets[key] = set
	}
	set[w] = struct{}{}
	return w
}

// remove drops one wait; the caller holds the owner's mutex.
func (ws *waiters[K, T]) remove(key K, w *waiter[T]) {
	if set := ws.sets[key]; set != nil {
		delete(set, w)
		if len(set) == 0 {
			delete(ws.sets, key)
		}
	}
}

func (ws *waiters[K, T]) keys() []K {
	keys := make([]K, 0, len(ws.sets))
	for key := range ws.sets {
		keys = append(keys, key)
	}
	return keys
}

func (ws *waiters[K, T]) resolve(key K, value T) {
	set := ws.sets[key]
	delete(ws.sets, key)
	for w := range set {
		w.settle(value, nil)
	}
}

func (ws *waiters[K, T]) rejectAll(err error) {
	sets := ws.sets
	ws.sets = map[K]map[*waiter[T]]struct{}{}
	var zero T
	for _, set := range sets {
		for w := range set {
			w.settle(zero, err)
		}
	}
}

// wait blocks until w settles or ctx is done; on cancellation it removes w
// under mu.
func wait[K comparable, T any](ctx context.Context, mu *sync.Mutex, ws *waiters[K, T], key K, w *waiter[T]) (T, error) {
	select {
	case <-w.done:
		return w.value, w.err
	case <-ctx.Done():
		mu.Lock()
		ws.remove(key, w)
		mu.Unlock()
		// A settlement that raced the cancellation wins, as a promise that
		// resolved first would.
		select {
		case <-w.done:
			return w.value, w.err
		default:
		}
		var zero T
		return zero, context.Cause(ctx)
	}
}
