package durable

import (
	"context"
	"fmt"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Committed document observation: the source behind a DocumentState and the
// DocumentWatch (pi packages/durable/src/session/observation.ts). A nil value
// is a retired incarnation, as pi's null.
//
// pi defers delivery to a microtask, so no listener ever runs inside the
// commit that produced its frame. Here delivery runs on a goroutine of its own,
// started when frames wait and ended when none do: a listener may call the
// Session, which a listener run on the mutation line could not.

// maxPendingWatchFrames is how many exact committed frames wait behind one
// busy watch listener before they collapse to one replacement.
const maxPendingWatchFrames = 100

// retirementOps is the canonical terminal update of a retired incarnation.
var retirementOps = []delta.Op{delta.Replace{Value: nil}}

// DocumentState is a disposable, read-only chord state bound to one committed
// document incarnation; its value is nil once the incarnation retires.
type DocumentState = chord.AttachedReplicatedState[map[string]any]

// Observer receives each committed revision of an observed value and the
// Session's close: a state source or a watch. Views built over committed
// publications outside this package (the harness's conversation views)
// hand their revisions to observers made by NewCommittedState and
// NewCommittedWatch.
type Observer interface {
	// Advance publishes one committed revision: the exact immutable value,
	// nil once retired, and the operations that produced it.
	Advance(ctx context.Context, value map[string]any, ops []delta.Op)
	// CloseSession ends the observer as the Session closes; failure is what
	// failed it, nil for a plain close.
	CloseSession(failure error)
}

// NewCommittedState makes a state source over value and the disposable
// chord state attached to it; release runs once the state is disposed or
// closed. report receives listener errors.
func NewCommittedState(value map[string]any, release func(), report func(error)) (Observer, DocumentState) {
	source := newCommittedStateSource(value, release)
	defer func() {
		if r := recover(); r != nil {
			source.finishDisposal()
			panic(r)
		}
	}()
	return source, chord.AttachReplicatedState[map[string]any](source, chord.ReplicatedStateSourceOptions{OnError: report})
}

// NewCommittedWatch makes a serialized exact-frame watch over value;
// release runs once it ends. report receives a failing listener's error.
func NewCommittedWatch(value map[string]any, release func(), report func(error)) *DocumentWatch {
	return newDocumentWatch(value, release, report)
}

// committedStateSource bridges the Session to chord for one attached state.
type committedStateSource struct {
	mu          sync.Mutex
	attachments []*sourceAttachment
	release     func()
	value       map[string]any
	cursor      int64
	retired     bool
	closed      bool
}

func newCommittedStateSource(value map[string]any, release func()) *committedStateSource {
	return &committedStateSource{value: value, release: release}
}

func (s *committedStateSource) Attach() chord.ReplicatedStateSourceAttachment[map[string]any] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		panic(fmt.Errorf("State source is closed"))
	}
	a := &sourceAttachment{value: s.value, cursor: s.cursor}
	a.release = func() {
		s.mu.Lock()
		for i, b := range s.attachments {
			if b == a {
				s.attachments = append(s.attachments[:i:i], s.attachments[i+1:]...)
				break
			}
		}
		empty := len(s.attachments) == 0
		s.mu.Unlock()
		if empty {
			s.finishDisposal()
		}
	}
	s.attachments = append(s.attachments, a)
	return a
}

// Advance publishes one committed revision; nil retires the source.
func (s *committedStateSource) Advance(ctx context.Context, value map[string]any, ops []delta.Op) {
	s.mu.Lock()
	if s.closed || s.retired {
		s.mu.Unlock()
		return
	}
	s.value = value
	s.cursor++
	if value == nil {
		s.retired = true
	}
	frame := chord.ReplicatedStateSourceFrame[map[string]any]{Cursor: s.cursor, Value: value, Ops: ops, Context: ctx}
	attachments := append([]*sourceAttachment(nil), s.attachments...)
	s.mu.Unlock()
	for _, a := range attachments {
		a.publish(frame)
	}
}

// CloseSession stops publishing; the attached states keep their last value.
func (s *committedStateSource) CloseSession(error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	attachments := append([]*sourceAttachment(nil), s.attachments...)
	s.mu.Unlock()
	for _, a := range attachments {
		a.Dispose()
	}
	s.finishDisposal()
}

func (s *committedStateSource) finishDisposal() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.value = nil
	release := s.release
	s.release = nil
	s.mu.Unlock()
	if release != nil {
		release()
	}
}

type sourceAttachment struct {
	value  map[string]any
	cursor int64

	mu         sync.Mutex
	release    func()
	frames     []chord.ReplicatedStateSourceFrame[map[string]any]
	listener   func(chord.ReplicatedStateSourceFrame[map[string]any])
	activated  bool
	disposed   bool
	scheduled  bool
	delivering bool
}

func (a *sourceAttachment) Snapshot() (map[string]any, int64) { return a.value, a.cursor }

func (a *sourceAttachment) Activate(listener func(chord.ReplicatedStateSourceFrame[map[string]any])) {
	a.mu.Lock()
	if a.activated {
		a.mu.Unlock()
		panic(fmt.Errorf("State attachment is already active"))
	}
	if a.disposed {
		a.mu.Unlock()
		panic(fmt.Errorf("State attachment is disposed"))
	}
	a.activated = true
	a.listener = listener
	a.mu.Unlock()
	a.drain()
}

func (a *sourceAttachment) publish(frame chord.ReplicatedStateSourceFrame[map[string]any]) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.disposed {
		return
	}
	a.frames = append(a.frames, frame)
	if !a.activated || a.delivering || a.scheduled {
		return
	}
	a.scheduled = true
	go func() {
		a.mu.Lock()
		a.scheduled = false
		disposed := a.disposed
		a.mu.Unlock()
		if disposed {
			return
		}
		// chord contains source-contract failures in its listener; an
		// unexpected panic is isolated to this attachment as well.
		defer func() {
			if r := recover(); r != nil {
				a.Dispose()
			}
		}()
		a.drain()
	}()
}

func (a *sourceAttachment) Dispose() {
	a.mu.Lock()
	if a.disposed {
		a.mu.Unlock()
		return
	}
	a.disposed = true
	a.frames = nil
	a.listener = nil
	release := a.release
	a.release = nil
	a.mu.Unlock()
	if release != nil {
		release()
	}
}

func (a *sourceAttachment) drain() {
	a.mu.Lock()
	if a.listener == nil || a.delivering || a.disposed {
		a.mu.Unlock()
		return
	}
	a.delivering = true
	defer func() {
		a.mu.Lock()
		a.delivering = false
		a.mu.Unlock()
	}()
	for !a.disposed && len(a.frames) > 0 {
		frame := a.frames[0]
		a.frames = a.frames[1:]
		listener := a.listener
		a.mu.Unlock()
		listener(frame)
		a.mu.Lock()
	}
	a.mu.Unlock()
}

// Watch end reasons.
const (
	WatchStopped       = "stopped"
	WatchCancelled     = "cancelled"
	WatchSessionClosed = "session_closed"
	WatchRetired       = "retired"
	WatchListenerError = "listener_error"
	WatchSessionFailed = "session_failed"
)

// WatchEnd is the terminal result of one watch. Err is the listener's error
// for "listener_error", and what failed the Session for "session_failed".
type WatchEnd struct {
	Reason string
	Err    error
}

// CommittedWatchListener receives one committed value: exact and immutable,
// with the exact operations that produced it. Its context carries the
// commit's values without its cancellation. An error ends the watch with
// "listener_error".
type CommittedWatchListener[T any] func(ctx context.Context, value T, ops []delta.Op) error

// WatchListener receives one committed revision of a document: nil once
// retired.
type WatchListener = CommittedWatchListener[map[string]any]

// CommittedWatchOptions shape a CommittedWatch.
type CommittedWatchOptions[T any] struct {
	// Replace gives the value an overflow delivers; nil delivers the newest
	// value.
	Replace func() T
	// Retires reports a value that retires the watch; it is delivered last.
	// Nil never retires.
	Retires func(T) bool
}

type watchFrame[T any] struct {
	value T
	ops   []delta.Op
	ctx   context.Context
}

// CommittedWatch is a serialized exact-frame observation of committed
// values (pi CommittedWatch<T>), with bounded pending delivery: beyond 100
// frames waiting behind a busy listener they collapse into one replacement.
// Views built over commit publications outside this package advance it.
type CommittedWatch[T any] struct {
	detach  func()
	report  func(error)
	options CommittedWatchOptions[T]

	mu         sync.Mutex
	value      T
	pending    []watchFrame[T]
	listener   CommittedWatchListener[T]
	started    bool
	scheduled  bool
	running    bool
	retired    bool
	end        *WatchEnd
	closed     chan struct{}
	stopCancel func() bool
}

// DocumentWatch observes one committed document incarnation; its value is
// nil once the incarnation retires.
type DocumentWatch = CommittedWatch[map[string]any]

// NewCommittedWatchOf makes a serialized exact-frame watch starting at
// value; detach runs once it ends. report receives a failing listener's
// error.
func NewCommittedWatchOf[T any](value T, detach func(), report func(error), options CommittedWatchOptions[T]) *CommittedWatch[T] {
	return &CommittedWatch[T]{value: value, detach: detach, report: report, options: options, closed: make(chan struct{})}
}

func newDocumentWatch(value map[string]any, detach func(), report func(error)) *DocumentWatch {
	return NewCommittedWatchOf(value, detach, report, CommittedWatchOptions[map[string]any]{
		Retires: func(value map[string]any) bool { return value == nil },
	})
}

// Value is the acquisition value before Start, and the latest delivered
// value afterwards.
func (w *CommittedWatch[T]) Value() T {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.value
}

// Start installs the sole listener. It never calls it inline.
func (w *CommittedWatch[T]) Start(listener CommittedWatchListener[T]) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return fmt.Errorf("Watch is already started")
	}
	if w.end != nil {
		return fmt.Errorf("Watch is stopped")
	}
	w.started = true
	w.listener = listener
	if len(w.pending) > 0 {
		w.scheduleLocked()
	}
	return nil
}

// Stop idempotently stops future callbacks and returns the watch's terminal
// result. A callback already running keeps running.
func (w *CommittedWatch[T]) Stop() WatchEnd {
	w.terminate(WatchEnd{Reason: WatchStopped})
	return w.End()
}

// Closed is closed when the watch terminates.
func (w *CommittedWatch[T]) Closed() <-chan struct{} { return w.closed }

// End is the terminal result; the zero WatchEnd until Closed is closed.
func (w *CommittedWatch[T]) End() WatchEnd {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.end == nil {
		return WatchEnd{}
	}
	return *w.end
}

// ObserveCancellation cancels the watch when ctx is done.
func (w *CommittedWatch[T]) ObserveCancellation(ctx context.Context) {
	stop := context.AfterFunc(ctx, w.Cancel)
	w.mu.Lock()
	if w.end != nil {
		w.mu.Unlock()
		stop()
		return
	}
	w.stopCancel = stop
	w.mu.Unlock()
}

func (w *CommittedWatch[T]) Cancel() { w.terminate(WatchEnd{Reason: WatchCancelled}) }

// CloseSession ends the watch with "session_closed", or with
// "session_failed" and the error when the Session failed.
func (w *CommittedWatch[T]) CloseSession(failure error) {
	if failure == nil {
		w.terminate(WatchEnd{Reason: WatchSessionClosed})
		return
	}
	w.terminate(WatchEnd{Reason: WatchSessionFailed, Err: failure})
}

// Advance queues one committed value and its operations.
func (w *CommittedWatch[T]) Advance(ctx context.Context, value T, ops []delta.Op) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.end != nil || w.retired {
		return
	}
	if w.retires(value) {
		w.retired = true
	}
	if len(w.pending) >= maxPendingWatchFrames {
		w.pending = w.pending[:0]
		replacement := value
		if w.options.Replace != nil {
			replacement = w.options.Replace()
		}
		w.pending = append(w.pending, watchFrame[T]{value: replacement, ops: []delta.Op{w.replaceOp(replacement)}, ctx: ctx})
	} else {
		w.pending = append(w.pending, watchFrame[T]{value: value, ops: ops, ctx: ctx})
	}
	if w.started {
		w.scheduleLocked()
	}
}

func (w *CommittedWatch[T]) retires(value T) bool {
	return w.options.Retires != nil && w.options.Retires(value)
}

// replaceOp is the root replacement an overflow delivers; a retiring value
// replaces the root with null.
func (w *CommittedWatch[T]) replaceOp(value T) delta.Op {
	if w.retires(value) {
		return delta.Replace{Value: nil}
	}
	return delta.Replace{Value: value}
}

func (w *CommittedWatch[T]) scheduleLocked() {
	if w.scheduled || w.running || w.end != nil {
		return
	}
	w.scheduled = true
	go func() {
		w.mu.Lock()
		w.scheduled = false
		w.mu.Unlock()
		w.drain()
	}()
}

func (w *CommittedWatch[T]) drain() {
	w.mu.Lock()
	if w.running || w.end != nil || !w.started {
		w.mu.Unlock()
		return
	}
	w.running = true
	for w.end == nil && len(w.pending) > 0 {
		frame := w.pending[0]
		w.pending = w.pending[1:]
		w.value = frame.value
		listener := w.listener
		w.mu.Unlock()
		err := callWatchListener(listener, frame)
		w.mu.Lock()
		if err != nil {
			if w.end == nil {
				w.mu.Unlock()
				w.report(err)
				w.terminate(WatchEnd{Reason: WatchListenerError, Err: err})
				w.mu.Lock()
			}
			break
		}
		if w.retires(frame.value) {
			w.mu.Unlock()
			w.terminate(WatchEnd{Reason: WatchRetired})
			w.mu.Lock()
			break
		}
	}
	w.running = false
	if w.end == nil && len(w.pending) > 0 {
		w.scheduleLocked()
	}
	w.mu.Unlock()
}

func callWatchListener[T any](listener CommittedWatchListener[T], frame watchFrame[T]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return listener(context.WithoutCancel(frame.ctx), frame.value, frame.ops)
}

// terminate ends the watch once: detach, drop pending frames and close.
func (w *CommittedWatch[T]) terminate(end WatchEnd) {
	w.mu.Lock()
	if w.end != nil {
		w.mu.Unlock()
		return
	}
	w.end = &end
	w.pending = nil
	stop := w.stopCancel
	w.stopCancel = nil
	w.mu.Unlock()
	w.detach()
	if stop != nil {
		stop()
	}
	close(w.closed)
}

func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}
