package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-lifecycle.test.ts. The cases with a tool
// or a generation hook come with those stages; the cases of views and the
// task graph come with them; the submission operations of the close cases
// come with submissions.

// completing is a one-phase task running run and completing with null.
func completing(t *testing.T, name string, run func()) Task {
	return oneStep(t, name, func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		if run != nil {
			run()
		}
		return rt.Commit(ctx, completeWith(nil))
	}, func(context.Context, *TaskRuntime) error { return nil })
}

// seedRunningTask commits a conversation with a running task, as a crash
// leaves it, so open has a reconciliation commit to fail.
func seedRunningTask(t *testing.T, storage durable.Storage) {
	t.Helper()
	conversation, err := storage.MintID(bg)
	must(t, err)
	id, err := storage.MintID(bg)
	must(t, err)
	_, err = storage.Commit(bg, []durable.StorageWrite{
		durable.ConversationWrite{Value: durable.ConversationRecord{ID: durable.ConversationID(conversation)}},
		durable.TaskWrite{Value: durable.TaskRecord{
			ID: durable.TaskID(id), ConversationID: durable.ConversationID(conversation), Kind: "test.seeded", Version: 1,
			Input: json.RawMessage("null"),
			State: durable.TaskState{Status: durable.TaskRunning, Checkpoint: json.RawMessage(`{"phase":"run"}`)},
		}},
	})
	must(t, err)
}

// countingReader is a registry reader that counts live subscriptions.
type countingReader struct {
	Registry
	mu    sync.Mutex
	count int
}

func (r *countingReader) Subscribe(listener func()) func() {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	unsubscribe := r.Registry.Subscribe(listener)
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.count--
			r.mu.Unlock()
			unsubscribe()
		})
	}
}

func (r *countingReader) subscriptions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func TestOpenClosesWithoutTheCancelledCallerContextAndReturnsTheOriginalError(t *testing.T) {
	storage := newControlledStorage()
	seedRunningTask(t, storage)
	reader := &countingReader{Registry: NewRegistry()}
	held := storage.holdCommits()
	storage.failNextCommit(errors.New("disk full"))
	ctx, cancel := context.WithCancelCause(bg)
	opening := async(func() (*Harness, error) { return Open(ctx, storage, Options{Registry: reader}) })
	held.Entered(t)
	cancel(errors.New("caller gave up"))
	storage.releaseHold(held)
	_, err := await(t, opening)
	expectError(t, err, "disk full")
	if reader.subscriptions() != 0 || storage.open() {
		t.Fatalf("subscriptions %d, storage open %v", reader.subscriptions(), storage.open())
	}
}

func TestOpenReturnsTheOpenErrorAndReportsItOnceThoughClosingThenFailsToo(t *testing.T) {
	storage := newControlledStorage()
	storage.closeError = errors.New("close failed")
	seedRunningTask(t, storage)
	storage.failNextCommit(errors.New("disk full"))
	reports := &reportLog{}
	_, err := Open(bg, storage, Options{Registry: NewRegistry(), OnReport: reports.add})
	expectError(t, err, "disk full")
	// The storage failure that failed open is the cause; the close that fails
	// after it adds no report.
	if got := names(reports.all(), func(err error) string { return err.Error() }); !reflect.DeepEqual(got, []string{"disk full"}) {
		t.Fatalf("reports = %v", got)
	}
}

func memoryOpen(storage *memory.Storage) bool {
	_, err := storage.Task(bg, 1)
	return err == nil
}

func TestCloseJoinsATaskHandlerThatIgnoresItsSignalBeforeClosingStorage(t *testing.T) {
	storage := memory.New()
	reached, release := make(chan struct{}), make(chan struct{})
	var readAfterRelease bool
	stubborn := completing(t, "test.close-stubborn", func() {
		close(reached)
		<-release
		readAfterRelease = memoryOpen(storage)
	})
	o := openRoot(t, []Task{stubborn}, openOptions{storage: storage})
	start(t, o.root, stubborn, false)
	must(t, o.h.Resume())
	<-reached
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	flush()
	select {
	case <-closing:
		t.Fatal("close did not join the handler")
	default:
	}
	if !memoryOpen(storage) {
		t.Fatal("storage closed before the handler returned")
	}
	close(release)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	if !readAfterRelease || memoryOpen(storage) {
		t.Fatalf("read after release %v, storage open %v", readAfterRelease, memoryOpen(storage))
	}
}

func TestCloseLetsANewHarnessOpenTheSameStorageOnceCloseReturned(t *testing.T) {
	storage := newControlledStorage()
	var log phaseLog
	release := make(chan struct{})
	var generation sync.Map
	generation.Store("n", 1)
	stubborn := completing(t, "test.generations", func() {
		n, _ := generation.Load("n")
		mine := n.(int)
		log.add("start " + string(rune('0'+mine)))
		if mine == 1 {
			<-release
		}
		log.add("end " + string(rune('0'+mine)))
	})
	first := openRoot(t, []Task{stubborn}, openOptions{storage: storage})
	id := start(t, first.root, stubborn, false)
	must(t, first.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 1 })
	closing := async(func() (struct{}, error) {
		err := first.h.Close(bg)
		log.add("closed")
		return struct{}{}, err
	})
	flush()
	select {
	case <-closing:
		t.Fatal("close did not join the handler")
	default:
	}
	close(release)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"start 1", "end 1", "closed"}) {
		t.Fatalf("log = %v", got)
	}
	generation.Store("n", 2)
	second := openRoot(t, []Task{stubborn}, openOptions{storage: storage.reopen()})
	must(t, second.h.Resume())
	waitDone(t, second.h, id)
	if got := log.all(); !reflect.DeepEqual(got, []string{"start 1", "end 1", "closed", "start 2", "end 2"}) {
		t.Fatalf("log = %v", got)
	}
}

func TestCloseKeepsShuttingDownAfterACancelledCloseAndASecondCloseAwaitsTheSameShutdown(t *testing.T) {
	storage := memory.New()
	reached, release := make(chan struct{}), make(chan struct{})
	stubborn := completing(t, "test.close-cancelled", func() {
		close(reached)
		<-release
	})
	o := openRoot(t, []Task{stubborn}, openOptions{storage: storage})
	start(t, o.root, stubborn, false)
	must(t, o.h.Resume())
	<-reached
	ctx, cancel := context.WithCancelCause(bg)
	cancelled := async(func() (struct{}, error) { return struct{}{}, o.h.Close(ctx) })
	cancel(errors.New("stop waiting"))
	_, err := await(t, cancelled)
	expectError(t, err, "stop waiting")
	// Admission stays sealed and the invocation still holds Storage open.
	expectError(t, o.h.Commit(bg, func(*durable.Tx) error { return nil }), "closed")
	if !memoryOpen(storage) {
		t.Fatal("storage closed while the invocation runs")
	}
	second := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	flush()
	select {
	case <-second:
		t.Fatal("the second close did not wait")
	default:
	}
	close(release)
	if _, err := await(t, second); err != nil {
		t.Fatal(err)
	}
	if memoryOpen(storage) {
		t.Fatal("storage still open")
	}
}

func TestCloseSettlesDurablyACommitWhoseCommitterWasCancelledWhileInStorage(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	held := storage.holdCommits()
	ctx, cancel := context.WithCancelCause(bg)
	committing := async(func() (durable.EntryID, error) {
		var id durable.EntryID
		err := o.root.Commit(ctx, func(tx *durable.Tx) error {
			entry, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
			id = entry.ID
			return err
		})
		return id, err
	})
	held.Entered(t)
	cancel(errors.New("committer gave up"))
	storage.releaseHold(held)
	id, err := await(t, committing)
	must(t, err)
	page, err := o.root.Entries(bg, durable.EntryQuery{}, 10, nil)
	must(t, err)
	if got := entryIDs(page.Items); !reflect.DeepEqual(got, []durable.EntryID{id}) {
		t.Fatalf("entries = %v", got)
	}
}

// closedOutcome is how a case reports an operation: "closed" for a closed
// error, the error otherwise, or "resolved".
func closedOutcome(err error) string {
	if err == nil {
		return "resolved"
	}
	if strings.Contains(err.Error(), "closed") {
		return "closed"
	}
	return err.Error()
}

func TestCloseFailsConversationAndHarnessOperationsOnceItBeginsAndAQueuedInspectReportsClosing(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	var entry durable.EntryRecord
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		return err
	}))
	held := holdLine(t, o.h, storage, o.root.ID)
	queuedInspect := async(func() (HarnessInspection, error) { return o.h.Inspect(bg) })
	flush()
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	operations := map[string]func() error{
		"agent":     func() error { _, err := o.root.Agent(bg); return err },
		"configure": func() error { return o.root.Configure(bg, AgentChange{Instructions: Set("x")}) },
		"commit":    func() error { return o.root.Commit(bg, func(*durable.Tx) error { return nil }) },
		"context":   func() error { _, err := o.root.Context(bg, nil); return err },
		"entries":   func() error { _, err := o.root.Entries(bg, durable.EntryQuery{}, 10, nil); return err },
		"fork": func() error {
			_, err := o.root.Fork(bg, entry.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
			return err
		},
		"abort":            func() error { return o.root.Abort(bg, false) },
		"conversationIdle": func() error { return o.root.WaitForIdle(bg) },
		"root":             func() error { _, err := o.h.Root(bg, nil, nil); return err },
		"conversation":     func() error { _, err := o.h.Conversation(bg, o.root.ID); return err },
		"createConversation": func() error {
			_, err := o.h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
			return err
		},
		"getTask":     func() error { _, err := o.h.GetTask(bg, 1); return err },
		"inspect":     func() error { _, err := o.h.Inspect(bg); return err },
		"abortTask":   func() error { _, err := o.h.AbortTask(bg, 1); return err },
		"waitForTask": func() error { _, err := o.h.WaitForTask(bg, 1); return err },
		"harnessIdle": func() error { return o.h.WaitForIdle(bg) },
		"usage":       func() error { _, err := o.h.Usage(bg); return err },
		"resume":      func() error { return o.h.Resume() },
	}
	var keys []string
	for name := range operations {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if got := closedOutcome(operations[name]()); got != "closed" {
			t.Errorf("%s = %s, want closed", name, got)
		}
	}
	storage.releaseHold(held)
	inspection, err := await(t, queuedInspect)
	must(t, err)
	if inspection.Scheduling != "closing" {
		t.Fatalf("queued inspect = %s", inspection.Scheduling)
	}
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCompletesReadsQueuedAtTheSealAndFailsQueuedWaitsAndAcquisitions(t *testing.T) {
	notes := sessionNotes("test.queued-at-seal")
	absentDoc := sessionNotes("test.queued-absent")
	pending := completing(t, "test.queued-pending", nil)
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(notes.Session()); return err }))
	taskID := start(t, o.root, pending, false)

	held := holdLine(t, o.h, storage, o.root.ID)
	type outcome = <-chan result[string]
	settle := func(f func() (bool, error)) outcome {
		return async(func() (string, error) {
			present, err := f()
			if err != nil {
				return closedOutcome(err), nil
			}
			if !present {
				return "nil", nil
			}
			return "resolved", nil
		})
	}
	queued := map[string]outcome{
		"context":  settle(func() (bool, error) { _, err := o.root.Context(bg, nil); return true, err }),
		"snapshot": settle(func() (bool, error) { v, err := o.h.Snapshot(bg, notes.Session()); return v != nil, err }),
		"absentWatch": settle(func() (bool, error) {
			w, err := o.h.WatchDoc(bg, absentDoc.Session())
			return w != nil, err
		}),
		"absentState": settle(func() (bool, error) {
			s, err := o.h.DocumentState(bg, absentDoc.Session())
			return s != nil, err
		}),
		"waitForTask": settle(func() (bool, error) { _, err := o.h.tasks.waitForTask(bg, taskID); return true, err }),
		"documentState": settle(func() (bool, error) {
			s, err := o.h.DocumentState(bg, notes.Session())
			return s != nil, err
		}),
		"watchDoc": settle(func() (bool, error) { w, err := o.h.WatchDoc(bg, notes.Session()); return w != nil, err }),
	}
	flush()
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	got := map[string]string{}
	for name, ch := range queued {
		got[name], _ = await(t, ch)
	}
	want := map[string]string{
		"context":       "resolved",
		"snapshot":      "resolved",
		"absentWatch":   "nil",
		"absentState":   "nil",
		"waitForTask":   "closed",
		"documentState": "closed",
		"watchDoc":      "closed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
}

// pausedHarness opens a paused Harness with one background task pending and
// a queued write submission.
type pausedHarness struct {
	opened
	id  durable.TaskID
	ran *sync.Map
}

func openPaused(t *testing.T) pausedHarness {
	t.Helper()
	ran := &sync.Map{}
	marker := completing(t, "test.marker", func() { ran.Store("ran", true) })
	o := openRoot(t, []Task{marker}, openOptions{})
	// Background, so idle waits and conversation abort leave it alone.
	id := start(t, o.root, marker, true)
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.CreateSubmission(durable.SubmissionRecord{ConversationID: o.root.ID, Type: durable.SubmissionTypeWrite, Status: durable.SubmissionQueued})
		return err
	}))
	return pausedHarness{opened: o, id: id, ran: ran}
}

func (p pausedHarness) hasRun() bool {
	_, ok := p.ran.Load("ran")
	return ok
}

func TestPausedHarnessNeverSchedulesFromAReadOnlyViewer(t *testing.T) {
	p := openPaused(t)
	notes := sessionNotes("test.viewer-notes")
	must(t, p.h.Commit(bg, func(tx *durable.Tx) error { _, err := tx.Doc(notes.Session()); return err }))
	_, err := p.h.Inspect(bg)
	must(t, err)
	_, err = p.h.GetTask(bg, p.id)
	must(t, err)
	_, err = p.h.Usage(bg)
	must(t, err)
	_, err = p.h.Conversation(bg, p.root.ID)
	must(t, err)
	_, err = p.h.Snapshot(bg, notes.Session())
	must(t, err)
	state, err := p.h.DocumentState(bg, notes.Session())
	must(t, err)
	state.Dispose()
	watch, err := p.h.WatchDoc(bg, notes.Session())
	must(t, err)
	watch.Stop()
	_, err = p.root.Agent(bg)
	must(t, err)
	_, err = p.root.Context(bg, nil)
	must(t, err)
	_, err = p.root.Entries(bg, durable.EntryQuery{}, 10, nil)
	must(t, err)
	flush()
	flush()
	inspection, err := p.h.Inspect(bg)
	must(t, err)
	if p.hasRun() || inspection.Scheduling != "paused" {
		t.Fatalf("ran %v, scheduling %s", p.hasRun(), inspection.Scheduling)
	}
}

func TestPausedHarnessSchedulesFromEveryProgressCall(t *testing.T) {
	calls := map[string]func(p pausedHarness){
		"abort":            func(p pausedHarness) { _ = p.root.Abort(bg, false) },
		"conversationIdle": func(p pausedHarness) { _ = p.root.WaitForIdle(bg) },
		"waitForTask":      func(p pausedHarness) { _, _ = p.h.WaitForTask(bg, p.id) },
		"harnessIdle":      func(p pausedHarness) { _ = p.h.WaitForIdle(bg) },
	}
	for name, call := range calls {
		p := openPaused(t)
		done := make(chan struct{})
		go func() {
			call(p)
			close(done)
		}()
		deadline := 500
		for !p.hasRun() && deadline > 0 {
			flush()
			deadline--
		}
		if !p.hasRun() {
			t.Fatalf("%s did not enable scheduling", name)
		}
		must(t, p.h.Close(bg))
		<-done
	}
}

func TestRegistryChangesBeforeResumeRunWhatTheRegistryHoldsAtResume(t *testing.T) {
	var log phaseLog
	define := func(label string) Task {
		return completing(t, "test.late-definition", func() { log.add(label) })
	}
	o := openRoot(t, nil, openOptions{})
	missing := start(t, o.root, define("unused"), false)
	inspect := func() TaskInspection {
		inspection, err := o.h.Inspect(bg)
		must(t, err)
		if len(inspection.Tasks) != 1 {
			t.Fatalf("tasks = %+v", inspection.Tasks)
		}
		return inspection.Tasks[0]
	}
	if got := inspect(); got.Kind != "blocked" || got.Reason != blockedMissingTask {
		t.Fatalf("before install = %+v", got)
	}
	extension := &Extension{Name: "task:test.late-definition", Tasks: []Task{define("v1")}}
	must(t, o.registry.Install(extension))
	if got := inspect(); got.Kind != "ready" {
		t.Fatalf("after install = %+v", got)
	}
	// The same extension name replaces the definition in place, still before
	// resume.
	o.registry.Uninstall(extension)
	must(t, o.registry.Install(&Extension{Name: "task:test.late-definition", Tasks: []Task{define("v2")}}))
	must(t, o.h.Resume())
	waitDone(t, o.h, missing)
	if got := log.all(); !reflect.DeepEqual(got, []string{"v2"}) {
		t.Fatalf("log = %v", got)
	}
}
