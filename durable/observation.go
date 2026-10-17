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

// WatchListener receives one committed revision: the exact immutable value,
// nil once retired, and the exact operations that produced it. Its context
// carries the commit's values without its cancellation. An error ends the
// watch with "listener_error".
type WatchListener func(ctx context.Context, value map[string]any, ops []delta.Op) error

type watchFrame struct {
	value map[string]any
	ops   []delta.Op
	ctx   context.Context
}

// DocumentWatch is a serialized exact-frame observation of one committed
// document incarnation, with bounded pending delivery: beyond 100 frames
// waiting behind a busy listener they collapse into one root replacement.
type DocumentWatch struct {
	detach func()
	report func(error)

	mu         sync.Mutex
	value      map[string]any
	pending    []watchFrame
	listener   WatchListener
	started    bool
	scheduled  bool
	running    bool
	retired    bool
	end        *WatchEnd
	closed     chan struct{}
	stopCancel func() bool
}

func newDocumentWatch(value map[string]any, detach func(), report func(error)) *DocumentWatch {
	return &DocumentWatch{value: value, detach: detach, report: report, closed: make(chan struct{})}
}

// Value is the acquisition revision before Start, and the latest delivered
// revision afterwards; nil once retired.
func (w *DocumentWatch) Value() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.value
}

// Start installs the sole listener. It never calls it inline.
func (w *DocumentWatch) Start(listener WatchListener) error {
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
func (w *DocumentWatch) Stop() WatchEnd {
	w.terminate(WatchEnd{Reason: WatchStopped})
	return w.End()
}

// Closed is closed when the watch terminates.
func (w *DocumentWatch) Closed() <-chan struct{} { return w.closed }

// End is the terminal result; the zero WatchEnd until Closed is closed.
func (w *DocumentWatch) End() WatchEnd {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.end == nil {
		return WatchEnd{}
	}
	return *w.end
}

// ObserveCancellation cancels the watch when ctx is done.
func (w *DocumentWatch) ObserveCancellation(ctx context.Context) {
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

func (w *DocumentWatch) Cancel() { w.terminate(WatchEnd{Reason: WatchCancelled}) }

// CloseSession ends the watch with "session_closed", or with
// "session_failed" and the error when the Session failed.
func (w *DocumentWatch) CloseSession(failure error) {
	if failure == nil {
		w.terminate(WatchEnd{Reason: WatchSessionClosed})
		return
	}
	w.terminate(WatchEnd{Reason: WatchSessionFailed, Err: failure})
}

func (w *DocumentWatch) Advance(ctx context.Context, value map[string]any, ops []delta.Op) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.end != nil || w.retired {
		return
	}
	if value == nil {
		w.retired = true
	}
	if len(w.pending) >= maxPendingWatchFrames {
		w.pending = w.pending[:0]
		w.pending = append(w.pending, watchFrame{value: value, ops: []delta.Op{replaceOp(value)}, ctx: ctx})
	} else {
		w.pending = append(w.pending, watchFrame{value: value, ops: ops, ctx: ctx})
	}
	if w.started {
		w.scheduleLocked()
	}
}

func replaceOp(value map[string]any) delta.Op {
	if value == nil {
		return delta.Replace{Value: nil}
	}
	return delta.Replace{Value: value}
}

func (w *DocumentWatch) scheduleLocked() {
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

func (w *DocumentWatch) drain() {
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
		if frame.value == nil {
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

func callWatchListener(listener WatchListener, frame watchFrame) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return listener(context.WithoutCancel(frame.ctx), frame.value, frame.ops)
}

// terminate ends the watch once: detach, drop pending frames and close.
func (w *DocumentWatch) terminate(end WatchEnd) {
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
