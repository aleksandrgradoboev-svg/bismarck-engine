package chord

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Ported from pi packages/chord/test/replicated-state.test.ts and
// replicated-state-delivery.test.ts. pi's listener that returns a promise is a
// listener that returns a channel here; pi's throw is a panic or an error on
// that channel.

type obj = map[string]any

func same(a, b any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

func num(v obj) float64 { return v["value"].(float64) }

// captureReports routes ReportError to a slice for the test.
func captureReports(t *testing.T) func() []error {
	t.Helper()
	var mu sync.Mutex
	var reports []error
	previous := ReportError
	ReportError = func(err error) { mu.Lock(); reports = append(reports, err); mu.Unlock() }
	t.Cleanup(func() { ReportError = previous })
	return func() []error { mu.Lock(); defer mu.Unlock(); return slices.Clone(reports) }
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func failWith(err error) <-chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

func TestReplicatedStatePublishesSharedRevision(t *testing.T) {
	state := NewReplicatedState(obj{"changed": obj{"value": 1.0}, "retained": obj{"value": 2.0}})
	previous := state.Value()
	var deliveries []int
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		deliveries = append(deliveries, d.Sequence)
		return nil
	})
	if err := state.Change(context.Background(), func(d *delta.Draft) error {
		if err := d.At("changed").Set("value", 3.0); err != nil {
			return err
		}
		return d.At("changed").Set("value", 4.0)
	}); err != nil {
		t.Fatal(err)
	}
	value := state.Value()
	if !reflect.DeepEqual(value, obj{"changed": obj{"value": 4.0}, "retained": obj{"value": 2.0}}) {
		t.Fatalf("value %v", value)
	}
	if same(value, previous) || same(value["changed"], previous["changed"]) || !same(value["retained"], previous["retained"]) {
		t.Fatal("not a structurally shared revision")
	}
	if !slices.Equal(deliveries, []int{0, 1}) {
		t.Fatalf("deliveries %v", deliveries)
	}
}

func TestReplicatedStateRollsBackAndRevokesDrafts(t *testing.T) {
	state := NewReplicatedState(obj{"nested": obj{"value": 1.0}})
	previous := state.Value()
	var escaped *delta.Draft
	stop := errors.New("stop")
	err := state.Change(context.Background(), func(d *delta.Draft) error {
		escaped = d.At("nested")
		_ = escaped.Set("value", 2.0)
		return stop
	})
	if !errors.Is(err, stop) || !same(state.Value(), previous) {
		t.Fatalf("err %v, value %v", err, state.Value())
	}
	defer func() {
		if r := recover(); r != delta.ErrDraftSettled {
			t.Fatalf("escaped draft: recovered %v", r)
		}
	}()
	escaped.Get("value")
}

func TestReplicatedStateRollsBackAPanickingChange(t *testing.T) {
	state := NewReplicatedState(obj{"value": 0.0})
	previous := state.Value()
	func() {
		defer func() { _ = recover() }()
		_ = state.Change(context.Background(), func(d *delta.Draft) error {
			_ = d.Set("value", 1.0)
			panic("boom")
		})
	}()
	if !same(state.Value(), previous) {
		t.Fatal("a panicking change was published")
	}
	// The state is usable afterwards.
	if err := state.Change(context.Background(), func(d *delta.Draft) error { return d.Set("value", 2.0) }); err != nil {
		t.Fatal(err)
	}
}

func TestReplicatedStateRejectsNestedChanges(t *testing.T) {
	state := NewReplicatedState(obj{"left": 0.0, "right": 0.0})
	err := state.Change(context.Background(), func(d *delta.Draft) error {
		_ = d.Set("left", 1.0)
		return state.Change(context.Background(), func(n *delta.Draft) error { return n.Set("right", 2.0) })
	})
	if err == nil || !strings.Contains(err.Error(), "reentrantly") {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(state.Value(), obj{"left": 0.0, "right": 0.0}) {
		t.Fatalf("value %v", state.Value())
	}
	err = state.Change(context.Background(), func(*delta.Draft) error {
		return state.Replace(context.Background(), obj{"left": 1.0, "right": 2.0})
	})
	if err == nil || !strings.Contains(err.Error(), "change callback") {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(state.Value(), obj{"left": 0.0, "right": 0.0}) {
		t.Fatalf("value %v", state.Value())
	}
}

func TestReplicatedStateQueuesListenerChanges(t *testing.T) {
	state := NewReplicatedState(obj{"value": 0.0})
	type got struct {
		value    float64
		sequence int
	}
	var deliveries []got
	var late []ReplicatedStateDelivery
	nested := false
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind != DeliveryUpdate || nested {
			return nil
		}
		nested = true
		if err := state.Change(context.Background(), func(d *delta.Draft) error { return d.Set("value", 2.0) }); err != nil {
			return failWith(err)
		}
		state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
			late = append(late, d)
			return nil
		})
		return nil
	})
	state.Subscribe(func(v obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			deliveries = append(deliveries, got{num(v), d.Sequence})
		}
		return nil
	})
	if err := state.Change(context.Background(), func(d *delta.Draft) error { return d.Set("value", 1.0) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deliveries, []got{{1, 1}, {2, 2}}) {
		t.Fatalf("deliveries %v", deliveries)
	}
	if !slices.Equal(late, []ReplicatedStateDelivery{{DeliveryHydrate, 2}}) {
		t.Fatalf("late %v", late)
	}
}

func TestReplicatedStateIsolatesListenerFailures(t *testing.T) {
	reports := captureReports(t)
	state := NewReplicatedState(obj{"value": 0.0})
	var received []int
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			panic(errors.New("listener failed"))
		}
		return nil
	})
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		received = append(received, d.Sequence)
		return nil
	})
	if err := state.Change(context.Background(), func(d *delta.Draft) error { return d.Set("value", 1.0) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Value(), obj{"value": 1.0}) || !slices.Equal(received, []int{0, 1}) {
		t.Fatalf("value %v received %v", state.Value(), received)
	}
	if r := reports(); len(r) != 1 || r[0].Error() != "listener failed" {
		t.Fatalf("reports %v", r)
	}
}

func TestReplicatedStateCopiesAssignedValues(t *testing.T) {
	external := obj{"value": 1.0}
	state := NewReplicatedState(obj{"left": nil, "right": nil})
	if err := state.Change(context.Background(), func(d *delta.Draft) error {
		if err := d.Set("left", external); err != nil {
			return err
		}
		if err := d.Set("right", external); err != nil {
			return err
		}
		return d.At("left").Set("value", 2.0)
	}); err != nil {
		t.Fatal(err)
	}
	v := state.Value()
	if num(external) != 1 || !reflect.DeepEqual(v, obj{"left": obj{"value": 2.0}, "right": obj{"value": 1.0}}) || same(v["left"], v["right"]) {
		t.Fatalf("external %v value %v", external, v)
	}
}

func TestReplicatedStateTakesOwnershipOfReplacements(t *testing.T) {
	state := NewReplicatedState(obj{"left": obj{"value": 1.0}, "right": obj{"value": 2.0}})
	replacement := obj{"left": obj{"value": 1.0}, "right": obj{"value": 1.0}}
	if err := state.Replace(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if !same(state.Value(), replacement) || same(state.Value()["left"], state.Value()["right"]) {
		t.Fatal("replacement not owned")
	}
	if err := state.Change(context.Background(), func(d *delta.Draft) error { return d.At("left").Set("value", 9.0) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Value(), obj{"left": obj{"value": 9.0}, "right": obj{"value": 1.0}}) {
		t.Fatalf("value %v", state.Value())
	}
}

func TestReplicatedStateIgnoresEqualReplacements(t *testing.T) {
	state := NewReplicatedState(obj{"value": obj{"nested": 1.0}, "retained": obj{"nested": 2.0}})
	previous := state.Value()
	if err := state.Replace(context.Background(), obj{"value": obj{"nested": 1.0}, "retained": obj{"nested": 2.0}}); err != nil {
		t.Fatal(err)
	}
	if !same(state.Value(), previous) {
		t.Fatal("an equal replacement changed the value")
	}
	if err := state.Replace(context.Background(), obj{"value": obj{"nested": 2.0}, "retained": previous["retained"]}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Value(), obj{"value": obj{"nested": 2.0}, "retained": obj{"nested": 2.0}}) || !same(state.Value()["retained"], previous["retained"]) {
		t.Fatalf("value %v", state.Value())
	}
}

// testSource is pi's TestSource.
type testSource struct {
	mu          sync.Mutex
	attachments []*testAttachment
	onAttach    func()
	value       obj
	cursor      int64
}

func (s *testSource) Attach() ReplicatedStateSourceAttachment[obj] {
	s.mu.Lock()
	a := &testAttachment{value: s.value, cursor: s.cursor}
	a.onDispose = func() {
		s.mu.Lock()
		s.attachments = slices.DeleteFunc(s.attachments, func(x *testAttachment) bool { return x == a })
		s.mu.Unlock()
	}
	s.attachments = append(s.attachments, a)
	onAttach := s.onAttach
	s.mu.Unlock()
	if onAttach != nil {
		onAttach()
	}
	return a
}

func (s *testSource) commit(value obj, cursor ...int64) {
	s.mu.Lock()
	s.value = value
	s.cursor++
	if len(cursor) > 0 {
		s.cursor = cursor[0]
	}
	frame := ReplicatedStateSourceFrame[obj]{Cursor: s.cursor, Value: value, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("value")}, Value: value["value"]}}, Context: context.Background()}
	attachments := slices.Clone(s.attachments)
	s.mu.Unlock()
	for _, a := range attachments {
		a.publish(frame)
	}
}

func (s *testSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attachments)
}

type testAttachment struct {
	value     obj
	cursor    int64
	onDispose func()
	buffer    []ReplicatedStateSourceFrame[obj]
	listener  func(ReplicatedStateSourceFrame[obj])
	activated bool
	disposed  bool
}

func (a *testAttachment) Snapshot() (obj, int64) { return a.value, a.cursor }

func (a *testAttachment) Activate(listener func(ReplicatedStateSourceFrame[obj])) {
	if a.activated {
		panic("attachment is already active")
	}
	a.activated = true
	a.listener = listener
	buffered := a.buffer
	a.buffer = nil
	for _, f := range buffered {
		listener(f)
	}
}

func (a *testAttachment) publish(f ReplicatedStateSourceFrame[obj]) {
	if a.disposed {
		return
	}
	if a.listener == nil {
		a.buffer = append(a.buffer, f)
	} else {
		a.listener(f)
	}
}

func (a *testAttachment) Dispose() {
	if a.disposed {
		return
	}
	a.disposed = true
	a.buffer = nil
	a.onDispose()
}

func TestAttachedStateDrainsQueuedCommits(t *testing.T) {
	second := obj{"value": 2.0}
	source := &testSource{value: obj{"value": 0.0}, cursor: 10}
	source.onAttach = func() {
		source.onAttach = nil
		source.commit(obj{"value": 1.0})
		source.commit(second)
	}
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	var deliveries []int
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		deliveries = append(deliveries, d.Sequence)
		return nil
	})
	if !same(state.Value(), second) || !slices.Equal(deliveries, []int{2}) {
		t.Fatalf("value %v deliveries %v", state.Value(), deliveries)
	}
}

func TestAttachedStateHydratesAtZero(t *testing.T) {
	current := obj{"value": 1.0}
	source := &testSource{value: obj{"value": 0.0}, cursor: 40}
	source.commit(current)
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	var deliveries []int
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		deliveries = append(deliveries, d.Sequence)
		return nil
	})
	if !same(state.Value(), current) || !slices.Equal(deliveries, []int{0}) {
		t.Fatalf("value %v deliveries %v", state.Value(), deliveries)
	}
}

func TestAttachedStatePublishesExactSourceValues(t *testing.T) {
	source := &testSource{value: obj{"value": 0.0}}
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	next := obj{"value": 1.0}
	var published obj
	state.Subscribe(func(v obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			published = v
		}
		return nil
	})
	// The frame's op says 7: chord must publish the source value as is.
	source.mu.Lock()
	source.cursor++
	frame := ReplicatedStateSourceFrame[obj]{Cursor: source.cursor, Value: next, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("value")}, Value: 7.0}}}
	attachments := slices.Clone(source.attachments)
	source.mu.Unlock()
	for _, a := range attachments {
		a.publish(frame)
	}
	if !same(state.Value(), next) || !same(published, next) {
		t.Fatal("not the exact source value")
	}
}

func TestAttachedStateBuffersReentrantFrames(t *testing.T) {
	source := &testSource{value: obj{"value": 0.0}}
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	var received []float64
	type lateDelivery struct {
		d     ReplicatedStateDelivery
		value float64
	}
	var late []lateDelivery
	nested := false
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind != DeliveryUpdate || d.Sequence != 1 || nested {
			return nil
		}
		nested = true
		source.commit(obj{"value": 2.0})
		state.Subscribe(func(v obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
			late = append(late, lateDelivery{d, num(v)})
			return nil
		})
		return nil
	})
	state.Subscribe(func(v obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			received = append(received, num(v))
		}
		return nil
	})
	source.commit(obj{"value": 1.0})
	if !slices.Equal(received, []float64{1, 2}) || !slices.Equal(late, []lateDelivery{{ReplicatedStateDelivery{DeliveryHydrate, 2}, 2}}) {
		t.Fatalf("received %v late %v", received, late)
	}
}

func TestAttachedStateReportsListenerFailures(t *testing.T) {
	source := &testSource{value: obj{"value": 0.0}}
	var errs []string
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{OnError: func(err error) { errs = append(errs, err.Error()) }})
	var received []float64
	state.Subscribe(func(_ obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			panic(errors.New("listener failed"))
		}
		return nil
	})
	state.Subscribe(func(v obj, _ context.Context, d ReplicatedStateDelivery) <-chan error {
		if d.Kind == DeliveryUpdate {
			received = append(received, num(v))
		}
		return nil
	})
	source.commit(obj{"value": 1.0})
	source.commit(obj{"value": 2.0})
	if !slices.Equal(received, []float64{1, 2}) || !slices.Equal(errs, []string{"listener failed", "listener failed"}) {
		t.Fatalf("received %v errors %v", received, errs)
	}
}

func TestAttachedStateReportsCursorGaps(t *testing.T) {
	source := &testSource{value: obj{"value": 0.0}, cursor: 5}
	var errs []error
	state := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{OnError: func(err error) { errs = append(errs, err) }})
	source.commit(obj{"value": 2.0}, 7)
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "expected 6, received 7") {
		t.Fatalf("errors %v", errs)
	}
	if source.count() != 0 || num(state.Value()) != 0 {
		t.Fatalf("attachments %d value %v", source.count(), state.Value())
	}
	source.commit(obj{"value": 3.0}, 8)
	if num(state.Value()) != 0 {
		t.Fatalf("value %v after a broken attachment", state.Value())
	}
}

func TestAttachedStatesAreIndependent(t *testing.T) {
	source := &testSource{value: obj{"value": 0.0}}
	first := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	second := AttachReplicatedState[obj](source, ReplicatedStateSourceOptions{})
	if source.count() != 2 {
		t.Fatalf("attachments %d", source.count())
	}
	source.commit(obj{"value": 1.0})
	if num(first.Value()) != 1 || num(second.Value()) != 1 {
		t.Fatal("not both updated")
	}
	first.Dispose()
	first.Dispose()
	if source.count() != 1 {
		t.Fatalf("attachments %d", source.count())
	}
	source.commit(obj{"value": 2.0})
	if num(first.Value()) != 1 || num(second.Value()) != 2 {
		t.Fatal("disposed state updated")
	}
	second.Dispose()
	if source.count() != 0 {
		t.Fatalf("attachments %d", source.count())
	}
}

// deliveryFixture is pi's fixture: a state of either kind and a publish.
func deliveryFixture(t *testing.T, kind string, onError func(error)) (ReplicatedState[obj], func(obj, context.Context)) {
	t.Helper()
	if kind == "mutable" {
		s := NewReplicatedState(obj{"value": 0.0})
		return s, func(v obj, ctx context.Context) {
			if err := s.Replace(ctx, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	var receive func(ReplicatedStateSourceFrame[obj])
	s := AttachReplicatedState[obj](sourceFunc{
		snapshot: obj{"value": 0.0},
		activate: func(l func(ReplicatedStateSourceFrame[obj])) { receive = l },
	}, ReplicatedStateSourceOptions{OnError: onError})
	return s, func(v obj, ctx context.Context) {
		receive(ReplicatedStateSourceFrame[obj]{Cursor: int64(num(v)), Value: v, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("value")}, Value: v["value"]}}, Context: ctx})
	}
}

type sourceFunc struct {
	snapshot obj
	activate func(func(ReplicatedStateSourceFrame[obj]))
}

func (s sourceFunc) Attach() ReplicatedStateSourceAttachment[obj] { return s }
func (s sourceFunc) Snapshot() (obj, int64)                       { return s.snapshot, 0 }
func (s sourceFunc) Activate(l func(ReplicatedStateSourceFrame[obj])) {
	s.activate(l)
}
func (s sourceFunc) Dispose() {}

type guarded[T any] struct {
	mu sync.Mutex
	v  []T
}

func (g *guarded[T]) add(x T) { g.mu.Lock(); g.v = append(g.v, x); g.mu.Unlock() }
func (g *guarded[T]) get() []T {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.v)
}

func TestReplicatedStateDelivery(t *testing.T) {
	for _, kind := range []string{"mutable", "attached"} {
		t.Run(kind+"/awaits each listener independently", func(t *testing.T) {
			state, publish := deliveryFixture(t, kind, func(error) {})
			hydration, update := make(chan error), make(chan error)
			var events guarded[string]
			var fast guarded[float64]
			state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
				events.add(fmt.Sprintf("start:%v", num(v)))
				done := make(chan error)
				go func() {
					switch num(v) {
					case 0:
						<-hydration
					case 1:
						<-update
					}
					events.add(fmt.Sprintf("end:%v", num(v)))
					close(done)
				}()
				return done
			})
			state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
				fast.add(num(v))
				return nil
			})
			publish(obj{"value": 1.0}, context.Background())
			publish(obj{"value": 2.0}, context.Background())
			if e := events.get(); !slices.Equal(e, []string{"start:0"}) {
				t.Fatalf("events %v", e)
			}
			if f := fast.get(); !slices.Equal(f, []float64{0, 1, 2}) || num(state.Value()) != 2 {
				t.Fatalf("fast %v value %v", f, state.Value())
			}
			close(hydration)
			waitFor(t, func() bool { return slices.Equal(events.get(), []string{"start:0", "end:0", "start:1"}) })
			close(update)
			waitFor(t, func() bool {
				return slices.Equal(events.get(), []string{"start:0", "end:0", "start:1", "end:1", "start:2", "end:2"})
			})
		})
		for _, count := range []int{100, 101, 102, 201, 202} {
			t.Run(fmt.Sprintf("%s/bounds %d pending deliveries", kind, count), func(t *testing.T) {
				state, publish := deliveryFixture(t, kind, func(error) {})
				hydration := make(chan error)
				var received guarded[float64]
				state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
					received.add(num(v))
					if num(v) == 0 {
						return hydration
					}
					return nil
				})
				for v := 1; v <= count; v++ {
					publish(obj{"value": float64(v)}, context.Background())
				}
				if r := received.get(); !slices.Equal(r, []float64{0}) {
					t.Fatalf("received %v", r)
				}
				close(hydration)
				first := (count-1)/100*100 + 1
				want := []float64{0}
				for v := first; v <= count; v++ {
					want = append(want, float64(v))
				}
				waitFor(t, func() bool { return slices.Equal(received.get(), want) })
			})
		}
		t.Run(kind+"/keeps the newest pending delivery exactly", func(t *testing.T) {
			state, publish := deliveryFixture(t, kind, func(error) {})
			gate := make(chan error)
			type got struct {
				value obj
				ctx   context.Context
				d     ReplicatedStateDelivery
			}
			var received guarded[got]
			state.Subscribe(func(v obj, ctx context.Context, d ReplicatedStateDelivery) <-chan error {
				received.add(got{v, ctx, d})
				if num(v) == 1 {
					return gate
				}
				return nil
			})
			for v := 1; v < 102; v++ {
				publish(obj{"value": float64(v)}, context.Background())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			publish(obj{"value": 102.0}, ctx)
			adopted := state.Value()
			publish(obj{"value": 103.0}, context.Background())
			values := func() []float64 {
				var out []float64
				for _, r := range received.get() {
					out = append(out, num(r.value))
				}
				return out
			}
			if v := values(); !slices.Equal(v, []float64{0, 1}) {
				t.Fatalf("received %v", v)
			}
			close(gate)
			waitFor(t, func() bool { return slices.Equal(values(), []float64{0, 1, 102, 103}) })
			r := received.get()[2]
			if !same(r.value, adopted) || r.ctx != ctx || r.d != (ReplicatedStateDelivery{DeliveryUpdate, 102}) {
				t.Fatalf("delivery %+v", r.d)
			}
		})
		t.Run(kind+"/serializes reentrant callbacks", func(t *testing.T) {
			state, publish := deliveryFixture(t, kind, func(error) {})
			var events []string
			state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
				events = append(events, fmt.Sprintf("start:%v", num(v)))
				if num(v) < 2 {
					publish(obj{"value": num(v) + 1}, context.Background())
				}
				events = append(events, fmt.Sprintf("end:%v", num(v)))
				return nil
			})
			if !slices.Equal(events, []string{"start:0", "end:0", "start:1", "end:1", "start:2", "end:2"}) {
				t.Fatalf("events %v", events)
			}
		})
		t.Run(kind+"/subscriptions of one callback are independent", func(t *testing.T) {
			state, publish := deliveryFixture(t, kind, func(error) {})
			gate := make(chan error)
			var received guarded[float64]
			listener := func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
				received.add(num(v))
				if num(v) == 0 {
					return gate
				}
				return nil
			}
			stopFirst := state.Subscribe(listener)
			stopSecond := state.Subscribe(listener)
			publish(obj{"value": 1.0}, context.Background())
			stopFirst()
			stopFirst()
			close(gate)
			waitFor(t, func() bool { return slices.Equal(received.get(), []float64{0, 0, 1}) })
			stopSecond()
		})
		t.Run(kind+"/unsubscribe drops queued deliveries", func(t *testing.T) {
			state, publish := deliveryFixture(t, kind, func(error) {})
			gate := make(chan error)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var received guarded[float64]
			var completed guarded[bool]
			stop := state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
				received.add(num(v))
				if num(v) != 1 {
					return nil
				}
				done := make(chan error)
				go func() { <-gate; completed.add(true); close(done) }()
				return done
			})
			publish(obj{"value": 1.0}, ctx)
			publish(obj{"value": 2.0}, ctx)
			stop()
			publish(obj{"value": 3.0}, ctx)
			if ctx.Err() != nil || len(completed.get()) != 0 {
				t.Fatal("the running callback was cancelled or joined")
			}
			close(gate)
			waitFor(t, func() bool { return len(completed.get()) == 1 })
			if r := received.get(); !slices.Equal(r, []float64{0, 1}) {
				t.Fatalf("received %v", r)
			}
		})
	}
}

func TestAttachedListenerErrors(t *testing.T) {
	t.Run("a synchronous hydration failure keeps the subscription", func(t *testing.T) {
		var errs guarded[error]
		state, publish := deliveryFixture(t, "attached", errs.add)
		failure := errors.New("sync hydration")
		var received []float64
		state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
			received = append(received, num(v))
			if num(v) == 0 {
				panic(failure)
			}
			return nil
		})
		publish(obj{"value": 1.0}, context.Background())
		if e := errs.get(); len(e) != 1 || e[0] != failure || !slices.Equal(received, []float64{0, 1}) {
			t.Fatalf("errors %v received %v", e, received)
		}
	})
	t.Run("hydration rejection, throw and update rejection", func(t *testing.T) {
		var errs guarded[error]
		state, publish := deliveryFixture(t, "attached", errs.add)
		gate := make(chan error, 1)
		var received, fast guarded[float64]
		state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
			received.add(num(v))
			switch num(v) {
			case 0:
				return gate
			case 1:
				panic(errors.New("sync update"))
			case 2:
				return failWith(errors.New("async update"))
			}
			return nil
		})
		state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
			fast.add(num(v))
			return nil
		})
		for v := 1; v <= 3; v++ {
			publish(obj{"value": float64(v)}, context.Background())
		}
		gate <- errors.New("async hydration")
		waitFor(t, func() bool { return slices.Equal(received.get(), []float64{0, 1, 2, 3}) })
		var messages []string
		for _, e := range errs.get() {
			messages = append(messages, e.Error())
		}
		if !slices.Equal(messages, []string{"async hydration", "sync update", "async update"}) || !slices.Equal(fast.get(), []float64{0, 1, 2, 3}) {
			t.Fatalf("errors %v fast %v", messages, fast.get())
		}
	})
	t.Run("a rejection after unsubscribe is still reported", func(t *testing.T) {
		var errs guarded[error]
		state, publish := deliveryFixture(t, "attached", errs.add)
		gate := make(chan error, 1)
		var received guarded[float64]
		stop := state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
			received.add(num(v))
			return gate
		})
		publish(obj{"value": 1.0}, context.Background())
		stop()
		failure := errors.New("stopped callback")
		gate <- failure
		waitFor(t, func() bool { e := errs.get(); return len(e) == 1 && e[0] == failure })
		if r := received.get(); !slices.Equal(r, []float64{0}) {
			t.Fatalf("received %v", r)
		}
	})
}

func TestMutableStateReportsRejectedCallbacks(t *testing.T) {
	reports := captureReports(t)
	state := NewReplicatedState(obj{"value": 0.0})
	failure := errors.New("async mutable hydration")
	var received guarded[float64]
	state.Subscribe(func(v obj, _ context.Context, _ ReplicatedStateDelivery) <-chan error {
		received.add(num(v))
		if num(v) == 0 {
			return failWith(failure)
		}
		return nil
	})
	if err := state.Replace(context.Background(), obj{"value": 1.0}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return slices.Equal(received.get(), []float64{0, 1}) })
	if r := reports(); len(r) != 1 || r[0] != failure {
		t.Fatalf("reports %v", r)
	}
}
