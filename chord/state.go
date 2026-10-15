package chord

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Replicated state (pi packages/chord/src/state.ts): an immutable JSON value
// with ordered, serialized change notifications. Either authoritative and
// mutable ([NewReplicatedState]) or a publication-only view of an external
// revision source ([AttachReplicatedState]).

// DeliveryKind is what a delivery carries: the value a subscription starts
// from, or a later revision.
type DeliveryKind string

const (
	DeliveryHydrate DeliveryKind = "hydrate"
	DeliveryUpdate  DeliveryKind = "update"
)

// ReplicatedStateDelivery describes one delivery to a subscriber.
type ReplicatedStateDelivery struct {
	Kind     DeliveryKind
	Sequence int
}

// StateListener receives a state's value. Returning nil means it is done
// with this delivery. Returning a channel means it goes on in the background
// — pi's listener that returns a promise: the subscription's next delivery
// waits until the channel yields a value or is closed, and a non-nil error
// from it is reported. A panic in the listener, pi's throw, is reported too.
// Either way delivery continues.
type StateListener[T any] func(value T, ctx context.Context, delivery ReplicatedStateDelivery) <-chan error

// ReplicatedState is a value with ordered change notifications.
type ReplicatedState[T any] interface {
	// Value is the current value. It is contract-immutable: not copied, and it
	// may share containers with the provider, so consumers must not mutate
	// it. Later updates do not mutate previously returned values.
	Value() T
	// Subscribe delivers the current value (hydration, with
	// context.Background()) and then each later revision. Each subscription
	// serializes its listener, awaiting hydration before updates. At most 100
	// deliveries wait behind a running listener; overflow keeps only the
	// newest pending one, so update sequences may skip. Failures are reported
	// in isolation and delivery continues. The returned function unsubscribes:
	// it discards pending deliveries without waiting for or cancelling a
	// running listener.
	Subscribe(listener StateListener[T]) (unsubscribe func())
}

// MutableReplicatedState is authoritative replicated state.
type MutableReplicatedState[T any] interface {
	ReplicatedState[T]
	// Change atomically publishes one synchronous overlay mutation. The draft
	// is unusable after mutate returns. Values placed through it are copied by
	// value. A mutate error or panic rolls the change back and is returned or
	// re-panicked.
	Change(ctx context.Context, mutate func(draft *delta.Draft) error) error
	// Replace atomically takes immutable ownership of an alias-free strict-JSON
	// replacement; the caller must not mutate it afterwards. A deeply equal
	// replacement publishes nothing.
	Replace(ctx context.Context, value T) error
}

// ReplicatedStateSourceFrame is one immutable authoritative revision committed
// after an attachment snapshot.
type ReplicatedStateSourceFrame[T any] struct {
	// Cursor is the monotonic source cursor. The first frame after a snapshot
	// must be the snapshot's cursor + 1.
	Cursor int64
	// Value is the exact immutable value this commit produced.
	Value T
	// Ops is the exact immutable operation batch that produced Value from the
	// preceding revision.
	Ops     []delta.Op
	Context context.Context
}

// ReplicatedStateSourceAttachment is one attachment to a source.
type ReplicatedStateSourceAttachment[T any] interface {
	// Snapshot is the fixed immutable snapshot captured at the atomic
	// attachment boundary.
	Snapshot() (value T, cursor int64)
	// Activate installs the sole listener and synchronously drains every
	// buffered frame in source commit order. It is single-use. From then on
	// every new committed frame must be delivered in order until disposal,
	// including commits made reentrantly while a frame is being delivered.
	Activate(listener func(frame ReplicatedStateSourceFrame[T]))
	// Dispose stops delivery and releases source resources; it is idempotent.
	Dispose()
}

// ReplicatedStateSource is an authoritative immutable revision source.
//
// Attach must synchronously and atomically capture one snapshot and register
// the attachment to buffer every later committed frame: the snapshot holds
// every commit before that boundary, buffered frames every commit after it,
// with no overlap or gap. Values and operation batches are immutable and stay
// valid after delivery; chord only publishes them, never applying or
// re-diffing them.
type ReplicatedStateSource[T any] interface {
	Attach() ReplicatedStateSourceAttachment[T]
}

// ReplicatedStateSourceOptions configure AttachReplicatedState.
type ReplicatedStateSourceOptions struct {
	// OnError receives source-contract and listener failures without
	// throwing them into the source. Nil reports them through ReportError.
	OnError func(error)
}

// AttachedReplicatedState is a synchronously hydrated publication-only state
// backed by one source attachment.
type AttachedReplicatedState[T any] interface {
	ReplicatedState[T]
	// Dispose idempotently releases the source attachment. The last published
	// value stays readable.
	Dispose()
}

// ReportError receives failures nothing else is set to receive. pi rethrows
// them in a microtask, which surfaces them as uncaught; a Go server must not
// crash for a listener's failure, so the default logs them.
var ReportError = func(err error) { log.Printf("chord: %v", err) }

const maxPendingDeliveries = 100

type stateDelivery[T any] struct {
	value    T
	ctx      context.Context
	delivery ReplicatedStateDelivery
}

// stateSubscriber is one subscription, independent of the producer's and the
// other subscribers' progress.
type stateSubscriber[T any] struct {
	listener    StateListener[T]
	reportError func(error)

	mu      sync.Mutex
	pending []stateDelivery[T]
	running bool
	started bool
	closed  bool
}

func (s *stateSubscriber[T]) push(d stateDelivery[T]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(s.pending) == maxPendingDeliveries {
		// A cold replica can queue updates reentrantly before this
		// subscriber's first hydration starts. Neither state here can: a new
		// subscription starts its hydration at once. Kept as pi keeps it.
		var hydration *stateDelivery[T]
		if !s.started {
			h := s.pending[0]
			hydration = &h
		}
		s.pending = s.pending[:0]
		if hydration != nil {
			s.pending = append(s.pending, *hydration)
		}
	}
	s.pending = append(s.pending, d)
}

func (s *stateSubscriber[T]) drain() {
	s.mu.Lock()
	if s.running || s.closed {
		s.mu.Unlock()
		return
	}
	s.running = true
	for len(s.pending) > 0 {
		d := s.pending[0]
		s.pending = s.pending[1:]
		s.started = true
		s.mu.Unlock()
		wait := s.call(d)
		if wait != nil {
			// The listener goes on in the background; this subscription resumes
			// once it settles, without holding up anyone else.
			go func() {
				if err, ok := <-wait; ok && err != nil {
					s.report(err)
				}
				s.mu.Lock()
				s.running = false
				s.mu.Unlock()
				s.drain()
			}()
			return
		}
		s.mu.Lock()
	}
	s.running = false
	s.mu.Unlock()
}

// call runs the listener; a panic is reported as pi reports a throw.
func (s *stateSubscriber[T]) call(d stateDelivery[T]) (wait <-chan error) {
	defer func() {
		if r := recover(); r != nil {
			s.report(panicError(r))
			wait = nil
		}
	}()
	wait = s.listener(d.value, d.ctx, d.delivery)
	if wait == nil {
		return nil
	}
	// A listener that already finished needs no background wait.
	select {
	case err, ok := <-wait:
		if ok && err != nil {
			s.report(err)
		}
		return nil
	default:
		return wait
	}
}

func (s *stateSubscriber[T]) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.pending = nil
}

func (s *stateSubscriber[T]) report(err error) {
	defer func() {
		if r := recover(); r != nil {
			ReportError(panicError(r))
		}
	}()
	s.reportError(err)
}

func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}

type publication[T any] struct {
	value    T
	sequence int
	ctx      context.Context
}

type subscription[T any] struct {
	subscriber *stateSubscriber[T]
	// hydrated is the sequence the subscription hydrated at.
	hydrated int
}

// statePublisher maintains publication order independently of how revisions
// are produced.
type statePublisher[T any] struct {
	reportError func(error)

	mu           sync.Mutex
	subs         []*subscription[T]
	publications []publication[T]
	value        T
	sequence     int
	delivering   bool
}

func newStatePublisher[T any](initial T, reportError func(error)) *statePublisher[T] {
	return &statePublisher[T]{value: initial, reportError: reportError}
}

func (p *statePublisher[T]) current() T {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value
}

func (p *statePublisher[T]) subscribe(listener StateListener[T]) func() {
	p.mu.Lock()
	value, sequence := p.value, p.sequence
	sub := &subscription[T]{subscriber: &stateSubscriber[T]{listener: listener, reportError: p.reportError}, hydrated: sequence}
	p.subs = append(p.subs, sub)
	p.mu.Unlock()
	// Hydration has no caller; it is delivered with the background context.
	sub.subscriber.push(stateDelivery[T]{value: value, ctx: context.Background(), delivery: ReplicatedStateDelivery{Kind: DeliveryHydrate, Sequence: sequence}})
	sub.subscriber.drain()
	var once sync.Once
	return func() {
		once.Do(func() {
			sub.subscriber.close()
			p.mu.Lock()
			for i, s := range p.subs {
				if s == sub {
					p.subs = append(p.subs[:i:i], p.subs[i+1:]...)
					break
				}
			}
			p.mu.Unlock()
		})
	}
}

// publish publishes an already-prepared immutable revision. Subscriber
// failures are reported, never returned. A publish made while another is
// delivering, from a listener or another goroutine, is queued and delivered
// by it in sequence order.
func (p *statePublisher[T]) publish(value T, ctx context.Context) {
	p.mu.Lock()
	p.value = value
	p.sequence++
	p.publications = append(p.publications, publication[T]{value: value, sequence: p.sequence, ctx: ctx})
	if p.delivering {
		p.mu.Unlock()
		return
	}
	p.delivering = true
	defer func() {
		p.mu.Lock()
		p.delivering = false
		p.mu.Unlock()
	}()
	for len(p.publications) > 0 {
		pub := p.publications[0]
		p.publications = p.publications[1:]
		subs := append([]*subscription[T](nil), p.subs...)
		p.mu.Unlock()
		delivery := ReplicatedStateDelivery{Kind: DeliveryUpdate, Sequence: pub.sequence}
		for _, sub := range subs {
			if pub.sequence <= sub.hydrated {
				continue
			}
			sub.subscriber.push(stateDelivery[T]{value: pub.value, ctx: pub.ctx, delivery: delivery})
			sub.subscriber.drain()
		}
		p.mu.Lock()
	}
	p.mu.Unlock()
}

var (
	errReentrantChange  = errors.New("Replicated state cannot be changed reentrantly from a change callback")
	errReplaceInsideChg = errors.New("Replicated state cannot be replaced from a change callback")
)

type mutableReplicatedState[T any] struct {
	mu        sync.Mutex
	tracker   *delta.Tracker[T]
	publisher *statePublisher[T]
	changing  bool
}

// NewReplicatedState creates authoritative state by taking immutable ownership
// of an alias-free strict-JSON root; the caller must not mutate initial
// afterwards. Listener failures go to ReportError.
func NewReplicatedState[T any](initial T) MutableReplicatedState[T] {
	tracker := delta.Track(initial)
	return &mutableReplicatedState[T]{tracker: tracker, publisher: newStatePublisher(tracker.Value(), func(err error) { ReportError(err) })}
}

func (s *mutableReplicatedState[T]) Value() T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracker.Value()
}

func (s *mutableReplicatedState[T]) Subscribe(listener StateListener[T]) func() {
	return s.publisher.subscribe(listener)
}

func (s *mutableReplicatedState[T]) Change(ctx context.Context, mutate func(draft *delta.Draft) error) (err error) {
	s.mu.Lock()
	if s.changing {
		s.mu.Unlock()
		return errReentrantChange
	}
	s.changing = true
	s.mu.Unlock()
	// pi's finally: a panicking mutate must not leave the state changing.
	released := false
	defer func() {
		if !released {
			s.mu.Lock()
			s.changing = false
			s.mu.Unlock()
		}
	}()
	prepared, err := s.prepareChange(mutate)
	s.mu.Lock()
	s.changing = false
	released = true
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.tracker.Adopt(prepared); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if len(prepared.Ops()) > 0 {
		s.publisher.publish(prepared.Value(), ctx)
	}
	return nil
}

// prepareChange runs mutate on a draft; an error or a panic aborts the draft.
func (s *mutableReplicatedState[T]) prepareChange(mutate func(draft *delta.Draft) error) (prepared *delta.Prepared[T], err error) {
	change, err := s.tracker.BeginChange()
	if err != nil {
		return nil, err
	}
	settled := false
	defer func() {
		if !settled {
			change.Abort()
		}
	}()
	if err := mutate(change.State()); err != nil {
		return nil, err
	}
	prepared, err = change.Prepare()
	if err != nil {
		return nil, err
	}
	settled = true
	return prepared, nil
}

func (s *mutableReplicatedState[T]) Replace(ctx context.Context, value T) error {
	s.mu.Lock()
	if s.changing {
		s.mu.Unlock()
		return errReplaceInsideChg
	}
	prepared, err := s.tracker.PrepareReplace(value)
	if err == nil {
		err = s.tracker.Adopt(prepared)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if len(prepared.Ops()) > 0 {
		s.publisher.publish(prepared.Value(), ctx)
	}
	return nil
}

type attachedReplicatedState[T any] struct {
	publisher   *statePublisher[T]
	attachment  ReplicatedStateSourceAttachment[T]
	reportError func(error)

	mu       sync.Mutex
	cursor   int64
	disposed bool
}

// AttachReplicatedState attaches a publication-only replicated state to one
// authoritative immutable source stream.
func AttachReplicatedState[T any](source ReplicatedStateSource[T], opts ReplicatedStateSourceOptions) AttachedReplicatedState[T] {
	attachment := source.Attach()
	value, cursor := attachment.Snapshot()
	s := &attachedReplicatedState[T]{attachment: attachment, cursor: cursor, reportError: opts.OnError}
	if s.reportError == nil {
		s.reportError = func(err error) { ReportError(err) }
	}
	s.publisher = newStatePublisher(value, s.report)
	// pi disposes the attachment when activation fails, and rethrows.
	activated := false
	defer func() {
		if !activated {
			attachment.Dispose()
		}
	}()
	attachment.Activate(s.receive)
	activated = true
	return s
}

func (s *attachedReplicatedState[T]) Value() T { return s.publisher.current() }

func (s *attachedReplicatedState[T]) Subscribe(listener StateListener[T]) func() {
	return s.publisher.subscribe(listener)
}

func (s *attachedReplicatedState[T]) Dispose() {
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return
	}
	s.disposed = true
	s.mu.Unlock()
	s.attachment.Dispose()
}

func (s *attachedReplicatedState[T]) receive(frame ReplicatedStateSourceFrame[T]) {
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return
	}
	if expected := s.cursor + 1; frame.Cursor != expected {
		s.mu.Unlock()
		s.fail(fmt.Errorf("Replicated state source cursor has a gap: expected %d, received %d", expected, frame.Cursor))
		return
	}
	s.cursor = frame.Cursor
	s.mu.Unlock()
	ctx := frame.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s.publisher.publish(frame.Value, ctx)
}

func (s *attachedReplicatedState[T]) fail(err error) {
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return
	}
	s.disposed = true
	s.mu.Unlock()
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = errors.Join(errors.New("Replicated state source contract failed"), err, panicError(r))
			}
		}()
		s.attachment.Dispose()
	}()
	s.report(err)
}

func (s *attachedReplicatedState[T]) report(err error) {
	defer func() {
		if r := recover(); r != nil {
			ReportError(panicError(r))
		}
	}()
	s.reportError(err)
}
