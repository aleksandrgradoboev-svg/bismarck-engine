package harness

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi test/harness-sleep.test.ts.

// millisClock is a Harness clock tests move by hand, in epoch milliseconds.
type millisClock struct{ ms atomic.Int64 }

func newClock(ms int64) *millisClock {
	c := &millisClock{}
	c.ms.Store(ms)
	return c
}

func (c *millisClock) set(ms int64)   { c.ms.Store(ms) }
func (c *millisClock) now() time.Time { return time.UnixMilli(c.ms.Load()) }

type napState struct {
	Phase string `json:"phase"`
	Slept int64  `json:"slept,omitempty"`
}

// napTask sleeps delay ms on the Harness clock, then completes with the time
// it resumed at; runs counts its after phases.
func napTask(t *testing.T, delay int64, runs *atomic.Int32) Task {
	t.Helper()
	task, err := DefineTask(TaskSpec[any, napState, int64]{
		Name:    "test.nap",
		Version: 1,
		Initial: func(any) napState { return napState{Phase: "start"} },
		Phases: map[string]func(context.Context, *Running[any, napState], *TaskRuntime) error{
			"start": func(ctx context.Context, _ *Running[any, napState], rt *TaskRuntime) error {
				now, err := rt.Now()
				if err != nil {
					return err
				}
				return rt.SleepUntil(ctx, now.Add(time.Duration(delay)*time.Millisecond), napState{Phase: "after", Slept: delay})
			},
			"after": func(ctx context.Context, _ *Running[any, napState], rt *TaskRuntime) error {
				if runs != nil {
					runs.Add(1)
				}
				resumed, err := rt.Now()
				if err != nil {
					return err
				}
				return rt.Commit(ctx, completeWith(resumed.UnixMilli()))
			},
		},
		Abort: func(ctx context.Context, _ *Running[any, napState], rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith("test"))
		},
	})
	must(t, err)
	return task
}

func idString(id durable.TaskID) string { return strconv.FormatInt(int64(id), 10) }

func wakeOf(t *testing.T, h *Harness) Wake {
	t.Helper()
	wake, err := h.Wake()
	must(t, err)
	return wake
}

// isWake compares a wake with pi's {active, at}; at < 0 is no sleep.
func isWake(w Wake, active bool, at int64) bool {
	if at < 0 {
		return w.Active == active && w.At.IsZero()
	}
	return w.Active == active && w.At.Equal(time.UnixMilli(at))
}

// wakeLog collects what OnWake receives.
type wakeLog struct {
	mu    sync.Mutex
	wakes []Wake
}

func (l *wakeLog) add(w Wake) {
	l.mu.Lock()
	l.wakes = append(l.wakes, w)
	l.mu.Unlock()
}

func (l *wakeLog) all() []Wake {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Wake(nil), l.wakes...)
}

func (l *wakeLog) last() (Wake, bool) {
	all := l.all()
	if len(all) == 0 {
		return Wake{}, false
	}
	return all[len(all)-1], true
}

type fakeTimer struct {
	delay    time.Duration
	callback func()
	disarmed atomic.Bool
}

// fakeTimers is a host SetTimer that records the timers that pace work and
// declines background ones.
type fakeTimers struct {
	mu    sync.Mutex
	armed []*fakeTimer
}

func (f *fakeTimers) setTimer(callback func(), delay time.Duration, background bool) func() {
	if background {
		return nil
	}
	timer := &fakeTimer{delay: delay, callback: callback}
	f.mu.Lock()
	f.armed = append(f.armed, timer)
	f.mu.Unlock()
	return func() { timer.disarmed.Store(true) }
}

func (f *fakeTimers) all() []*fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeTimer(nil), f.armed...)
}

func (f *fakeTimers) live() []*fakeTimer {
	var out []*fakeTimer
	for _, timer := range f.all() {
		if !timer.disarmed.Load() {
			out = append(out, timer)
		}
	}
	return out
}

func (f *fakeTimers) liveWith(delay time.Duration) *fakeTimer {
	for _, timer := range f.live() {
		if timer.delay == delay {
			return timer
		}
	}
	return nil
}

// settleTimers lets pending goroutines and a few timer turns run.
func settleTimers() {
	flush()
	time.Sleep(20 * time.Millisecond)
	flush()
}

func status(t *testing.T, h *Harness, id durable.TaskID) func() bool {
	return func() bool { return stateOf(t, h, id).Status == durable.TaskSleeping }
}

func TestSleepSleepsWithoutAnInvocationAndResumesAtItsCheckpointOnceDue(t *testing.T) {
	clock := newClock(1_000)
	wakes := &wakeLog{}
	var runs atomic.Int32
	nap := napTask(t, 1_000, &runs)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, onWake: wakes.add})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	if got := jsonOf(t, stateOf(t, o.h, id)); got != `{"status":"sleeping","checkpoint":{"phase":"after","slept":1000},"until":2000}` {
		t.Fatalf("state = %s", got)
	}
	inspection, err := o.h.Inspect(bg)
	must(t, err)
	var inspected *TaskInspection
	for i := range inspection.Tasks {
		if inspection.Tasks[i].Record.ID == id {
			inspected = &inspection.Tasks[i]
		}
	}
	if inspected == nil || inspected.Kind != "sleeping" || !inspected.Until.Equal(time.UnixMilli(2_000)) {
		t.Fatalf("inspection = %+v", inspected)
	}
	node := decodeGraph(t, graph.Value()).Tasks[id]
	if got := jsonOf(t, node.State); got != `{"status":"sleeping","phase":"after","until":2000}` {
		t.Fatalf("graph state = %s", got)
	}
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, 2_000) {
		t.Fatalf("wake = %+v", w)
	}
	settleTimers()
	if w, _ := wakes.last(); !isWake(w, false, 2_000) {
		t.Fatalf("last wake = %+v", w)
	}

	// Early and repeated wakes run nothing.
	clock.set(1_999)
	must(t, o.h.Resume())
	must(t, o.h.Resume())
	settleTimers()
	if runs.Load() != 0 {
		t.Fatalf("after ran %d times before its time", runs.Load())
	}
	if got := stateOf(t, o.h, id).Status; got != durable.TaskSleeping {
		t.Fatalf("status = %s", got)
	}

	clock.set(2_000)
	must(t, o.h.Resume())
	must(t, o.h.Resume())
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":2000}` {
		t.Fatalf("outcome = %s", got)
	}
	if runs.Load() != 1 {
		t.Fatalf("after ran %d times", runs.Load())
	}
	settleTimers()
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, -1) {
		t.Fatalf("wake = %+v", w)
	}
	eventually(t, func() bool { w, _ := wakes.last(); return isWake(w, false, -1) })
	// Only changes: idle at open, running, asleep until 2000, running again,
	// idle.
	all := wakes.all()
	sawActive, sawAsleep := false, false
	for i, w := range all {
		if i > 0 && w.Active == all[i-1].Active && w.At.Equal(all[i-1].At) {
			t.Fatalf("wake %d repeats %+v: %+v", i, w, all)
		}
		sawActive = sawActive || isWake(w, true, -1)
		sawAsleep = sawAsleep || isWake(w, false, 2_000)
	}
	if !sawActive || !sawAsleep {
		t.Fatalf("wakes = %+v", all)
	}
}

func TestSleepKeepsASleepAcrossReopen(t *testing.T) {
	directory := t.TempDir()
	clock := newClock(1_000)
	nap := napTask(t, 60_000, nil)
	open := func() opened {
		return openRoot(t, []Task{nap}, openOptions{storage: openJSONL(t, directory), now: clock.now, onWake: func(Wake) {}})
	}
	o := open()
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	must(t, o.h.Close(bg))

	o = open()
	// Known from storage before scheduling starts, once open's recovery pass
	// is done.
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, 61_000) {
		t.Fatalf("wake = %+v", w)
	}
	must(t, o.h.Resume())
	settleTimers()
	if got := stateOf(t, o.h, id).Status; got != durable.TaskSleeping {
		t.Fatalf("status = %s", got)
	}
	must(t, o.h.Close(bg))

	clock.set(61_000)
	o = open()
	if w := wakeOf(t, o.h); !isWake(w, true, -1) {
		t.Fatalf("wake = %+v", w)
	}
	must(t, o.h.Resume())
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":61000}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestSleepResumesADueSleepWithItsTimerWithoutOnWakeWithBackgroundTimersDeclined(t *testing.T) {
	clock := newClock(1_000)
	timers := &fakeTimers{}
	nap := napTask(t, 500, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, setTimer: timers.setTimer})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return timers.liveWith(500*time.Millisecond) != nil })
	if got := stateOf(t, o.h, id).Status; got != durable.TaskSleeping {
		t.Fatalf("status = %s", got)
	}
	clock.set(1_500)
	timers.liveWith(500 * time.Millisecond).callback()
	// Polled: WaitForTask would call Resume, which wakes due sleeps itself.
	eventually(t, func() bool { return stateOf(t, o.h, id).Status == durable.TaskTerminal })
	if got := outcomeJSON(t, *getTask(t, o.h, id)); got != `{"status":"completed","result":1500}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestSleepRunsADueSleepAtAnySchedulingPassBeforeItsTimerFires(t *testing.T) {
	clock := newClock(1_000)
	timers := &fakeTimers{}
	nap := napTask(t, 500, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, setTimer: timers.setTimer})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	// The deadline passes, and an unrelated change starts a pass before the
	// timer fires.
	clock.set(1_500)
	must(t, o.registry.Install(&Extension{Name: "unrelated"}))
	// Polled: WaitForTask would call Resume, which wakes due sleeps itself.
	eventually(t, func() bool { return stateOf(t, o.h, id).Status == durable.TaskTerminal })
	if got := outcomeJSON(t, *getTask(t, o.h, id)); got != `{"status":"completed","result":1500}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestSleepDisarmsTheSleepTimerWhenTheSleepIsAbortedAndWhenTheHarnessCloses(t *testing.T) {
	clock := newClock(1_000)
	timers := &fakeTimers{}
	short := napTask(t, 500, nil)
	o := openRoot(t, []Task{short}, openOptions{now: clock.now, setTimer: timers.setTimer})
	aborted := start(t, o.root, short, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return timers.liveWith(500*time.Millisecond) != nil })
	if _, err := o.h.AbortTask(bg, aborted); err != nil {
		t.Fatal(err)
	}
	waitDone(t, o.h, aborted)
	eventually(t, func() bool { return len(timers.live()) == 0 })
	kept := start(t, o.root, short, false)
	eventually(t, status(t, o.h, kept))
	eventually(t, func() bool { return len(timers.live()) == 1 })
	if live := timers.live(); live[0].delay != 500*time.Millisecond {
		t.Fatalf("live timer delay = %v", live[0].delay)
	}
	must(t, o.h.Close(bg))
	if live := timers.live(); len(live) != 0 {
		t.Fatalf("%d timers live after close", len(live))
	}
}

func TestSleepDisarmsAnInvocationsTimersWhenItEndsAndALateFireRunsNothing(t *testing.T) {
	var fired atomic.Int32
	timed := oneStep(t, "test.timed", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		rt.SetTimer(func() { fired.Add(1) }, time.Minute)
		return rt.Commit(ctx, completeWith(nil))
	}, func(context.Context, *TaskRuntime) error { return nil })
	timers := &fakeTimers{}
	o := openRoot(t, []Task{timed}, openOptions{setTimer: timers.setTimer})
	id := start(t, o.root, timed, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	var timer *fakeTimer
	for _, armed := range timers.all() {
		if armed.delay == time.Minute {
			timer = armed
		}
	}
	if timer == nil {
		t.Fatal("the invocation armed no timer")
	}
	eventually(t, timer.disarmed.Load)
	// A host timer that fires anyway: the ended invocation's callback does not
	// run.
	timer.callback()
	if fired.Load() != 0 {
		t.Fatal("an ended invocation's timer ran its callback")
	}
}

func TestSleepArmsNoTimerForSleepsWithOnWake(t *testing.T) {
	clock := newClock(1_000)
	timers := &fakeTimers{}
	nap := napTask(t, 500, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, setTimer: timers.setTimer, onWake: func(Wake) {}})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	settleTimers()
	if armed := timers.all(); len(armed) != 0 {
		t.Fatalf("%d timers armed", len(armed))
	}
	clock.set(1_500)
	if w := wakeOf(t, o.h); !isWake(w, true, -1) {
		t.Fatalf("wake = %+v", w)
	}
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
}

func TestSleepArmsAForegroundTimerTheHostDeclinesWithAfterFuncReportingItOnce(t *testing.T) {
	nap := napTask(t, 10, nil)
	o := openRoot(t, []Task{nap}, openOptions{setTimer: func(func(), time.Duration, bool) func() { return nil }})
	first := start(t, o.root, nap, false)
	second := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, first).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("first outcome = %s", got)
	}
	if got := waitDone(t, o.h, second).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("second outcome = %s", got)
	}
	var messages []string
	for _, err := range o.reports.all() {
		messages = append(messages, err.Error())
	}
	if want := []string{"SetTimer declined a timer that paces work; using time.AfterFunc"}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("reports = %q, want %q", messages, want)
	}
}

func TestSleepCountsNeitherAWaitOnASleepingTaskNorAnOutcomeHeldOverOneAsActive(t *testing.T) {
	clock := newClock(1_000)
	child := napTask(t, 500, nil)
	type parentState struct {
		Phase string         `json:"phase"`
		Child durable.TaskID `json:"child,omitempty"`
	}
	type parentInput struct {
		Hold bool `json:"hold"`
	}
	parent, err := DefineTask(TaskSpec[parentInput, parentState, any]{
		Name:    "test.parent",
		Version: 1,
		Initial: func(parentInput) parentState { return parentState{Phase: "start"} },
		Phases: map[string]func(context.Context, *Running[parentInput, parentState], *TaskRuntime) error{
			"start": func(ctx context.Context, task *Running[parentInput, parentState], rt *TaskRuntime) error {
				return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					id, err := CreateTask[any](tx, child, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: current.ID}})
					if err != nil {
						return nil, err
					}
					next := parentState{Phase: "joined", Child: id}
					// Held: completes next, holding its outcome over the sleeping
					// child. Otherwise waits for it.
					if task.Input.Hold {
						return Next(next)
					}
					return Wait([]durable.TaskID{id}, durable.JoinAllSettled, next)
				})
			},
			"joined": func(ctx context.Context, _ *Running[parentInput, parentState], rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *Running[parentInput, parentState], *TaskRuntime) error { return nil },
	})
	must(t, err)
	for _, hold := range []bool{false, true} {
		o := openRoot(t, []Task{parent, child}, openOptions{now: clock.now, onWake: func(Wake) {}})
		var id durable.TaskID
		must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
			var err error
			id, err = CreateTask(tx, parent, parentInput{Hold: hold}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
			return err
		}))
		must(t, o.h.Resume())
		held := durable.TaskWaiting
		if hold {
			held = durable.TaskCompleting
		}
		eventually(t, func() bool { return stateOf(t, o.h, id).Status == held })
		eventually(t, func() bool { return wakeOf(t, o.h).At.Equal(time.UnixMilli(1_500)) })
		eventually(t, func() bool { return !wakeOf(t, o.h).Active })
		if w := wakeOf(t, o.h); !isWake(w, false, 1_500) {
			t.Fatalf("hold %v: wake = %+v", hold, w)
		}
		must(t, o.h.Close(bg))
	}
}

func TestSleepRunsTheAbortHandlerOfASleepingTaskWithoutWaitingForItsTime(t *testing.T) {
	clock := newClock(1_000)
	nap := napTask(t, 60_000, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, onWake: func(Wake) {}})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	if _, err := o.h.AbortTask(bg, id); err != nil {
		t.Fatal(err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"test"}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestSleepRejectsASleepUntilSomethingThatIsNotATimeAndASleepInAnAbortHandler(t *testing.T) {
	var mu sync.Mutex
	var failures []string
	note := func(err error) {
		mu.Lock()
		failures = append(failures, err.Error())
		mu.Unlock()
	}
	next := map[string]string{"phase": "next"}
	invalid := oneStep(t, "test.invalid-sleep", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		// pi also tries NaN and Infinity, which a time.Time cannot hold; a
		// state without until is the Go case pi's types rule out.
		if err := rt.SleepUntil(ctx, time.UnixMilli(-1), next); err != nil {
			note(err)
		}
		if err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
			return &durable.TaskState{Status: durable.TaskSleeping, Checkpoint: []byte(`{"phase":"next"}`)}, nil
		}); err != nil {
			note(err)
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	aborting := oneStep(t, "test.sleep-in-abort", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(ctx context.Context, rt *TaskRuntime) error {
		now, err := rt.Now()
		if err != nil {
			return err
		}
		if err := rt.SleepUntil(ctx, now.Add(10*time.Millisecond), map[string]string{"phase": "run"}); err != nil {
			note(err)
		}
		return rt.Commit(ctx, abortWith("test"))
	})
	o := openRoot(t, []Task{invalid, aborting}, openOptions{})
	id := start(t, o.root, invalid, false)
	abortingID := start(t, o.root, aborting, false)
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
	if _, err := o.h.AbortTask(bg, abortingID); err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, o.h, abortingID).State.Outcome.Status; got != durable.OutcomeAborted {
		t.Fatalf("aborting outcome = %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"Task " + idString(id) + " cannot sleep until -1: not a time in milliseconds",
		"Task " + idString(id) + " cannot sleep until undefined: not a time in milliseconds",
		"Abort handler of task " + idString(abortingID) + " cannot sleep",
	}
	if !reflect.DeepEqual(failures, want) {
		t.Fatalf("failures = %q, want %q", failures, want)
	}
}

func TestSleepDoesNotCountATaskWithoutADefinitionAsActive(t *testing.T) {
	missing := napTask(t, 10, nil)
	o := openRoot(t, nil, openOptions{})
	// Created through a task the registry does not hold: nothing can run it.
	start(t, o.root, missing, false)
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, -1) {
		t.Fatalf("wake = %+v", w)
	}
	must(t, o.h.Resume())
	settleTimers()
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, -1) {
		t.Fatalf("wake = %+v", w)
	}
}

func TestSleepReportsAPanickingOnWakeAndOffersTheSameWakeAgainAtTheNextPass(t *testing.T) {
	clock := newClock(1_000)
	wakes := &wakeLog{}
	failure := errors.New("alarm failed")
	var failing atomic.Bool
	failing.Store(true)
	nap := napTask(t, 1_000, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, onWake: func(w Wake) {
		wakes.add(w)
		if failing.Load() && !w.At.IsZero() {
			panic(failure)
		}
	}})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, func() bool {
		for _, w := range wakes.all() {
			if isWake(w, false, 2_000) {
				return true
			}
		}
		return false
	})
	eventually(t, func() bool {
		for _, err := range o.reports.all() {
			if errors.Is(err, failure) {
				return true
			}
		}
		return false
	})
	// The host recovers; an unrelated change offers the same, undelivered wake
	// again.
	failing.Store(false)
	offered := len(wakes.all())
	must(t, o.registry.Install(&Extension{Name: "unrelated"}))
	eventually(t, func() bool { return len(wakes.all()) > offered })
	if w, _ := wakes.last(); !isWake(w, false, 2_000) {
		t.Fatalf("last wake = %+v", w)
	}
	clock.set(2_000)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	eventually(t, func() bool { w, _ := wakes.last(); return isWake(w, false, -1) })
}

// Beyond pi, whose graph case observes a task before it sleeps: a graph
// first observed while a task sleeps builds it from storage.
func TestSleepShowsASleepingTaskInAGraphFirstObservedWhileItSleeps(t *testing.T) {
	clock := newClock(1_000)
	nap := napTask(t, 60_000, nil)
	o := openRoot(t, []Task{nap}, openOptions{now: clock.now, onWake: func(Wake) {}})
	id := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	eventually(t, status(t, o.h, id))
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	node, ok := decodeGraph(t, graph.Value()).Tasks[id]
	if !ok {
		t.Fatal("the sleeping task is missing from the graph")
	}
	if got := jsonOf(t, node.State); got != `{"status":"sleeping","phase":"after","until":61000}` {
		t.Fatalf("graph state = %s", got)
	}
}

// Beyond pi, whose two sleeps share one deadline and so one timer: a host
// that keeps declining is reported once, not at every timer.
func TestSleepReportsAHostThatDeclinesEveryForegroundTimerOnce(t *testing.T) {
	nap := napTask(t, 10, nil)
	o := openRoot(t, []Task{nap}, openOptions{setTimer: func(func(), time.Duration, bool) func() { return nil }})
	first := start(t, o.root, nap, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, first)
	// A later, separate sleep arms the timer again.
	second := start(t, o.root, nap, false)
	waitDone(t, o.h, second)
	if reports := o.reports.all(); len(reports) != 1 {
		t.Fatalf("reports = %v", reports)
	}
}
