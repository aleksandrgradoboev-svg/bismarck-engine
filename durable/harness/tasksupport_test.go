package harness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Task test support (pi packages/durable/test/task-support.ts and the
// ControlledStorage of session-support.ts).

// gate holds calls until released and reports when the first held call
// arrives.
type gate struct {
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newGate() *gate { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) enter() {
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gate) Release() { g.releaseOnce.Do(func() { close(g.release) }) }

// Entered waits for the first held call.
func (g *gate) Entered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no call reached the gate")
	}
}

// controlledStorage is memory storage with observable commits, held commits
// and injected commit failures; it survives Close, so a reopen reads its
// records again.
type controlledStorage struct {
	*memory.Storage
	mu            sync.Mutex
	commits       [][]durable.StorageWrite
	commitGate    *gate
	commitFailure error
	// closed stands for a closed database; the records stay, so a reopen
	// reads them again, as a fresh process would.
	closed     bool
	closeError error
}

func newControlledStorage() *controlledStorage { return &controlledStorage{Storage: memory.New()} }

func (s *controlledStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	g := s.commitGate
	failure := s.commitFailure
	s.commitFailure = nil
	s.mu.Unlock()
	if g != nil {
		g.enter()
	}
	if failure != nil {
		return 0, failure
	}
	seq, err := s.Storage.Commit(ctx, writes)
	if err == nil {
		copies := make([]durable.StorageWrite, len(writes))
		for i, write := range writes {
			copies[i], _ = durable.CloneWrite(write)
		}
		s.mu.Lock()
		s.commits = append(s.commits, copies)
		s.mu.Unlock()
	}
	return seq, err
}

func (s *controlledStorage) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.closeError
}

// reopen reopens the storage after Close, keeping every committed record.
func (s *controlledStorage) reopen() *controlledStorage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = false
	return s
}

// Task fails once the storage is closed, as a closed database read does.
func (s *controlledStorage) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, memory.ErrClosed
	}
	return s.Storage.Task(ctx, id)
}

// open reports whether a raw Storage read still succeeds, as code of a
// joined invocation relies on.
func (s *controlledStorage) open() bool {
	_, err := s.Task(bg, 1)
	return err == nil
}

func (s *controlledStorage) holdCommits() *gate {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitGate = newGate()
	return s.commitGate
}

func (s *controlledStorage) releaseHold(g *gate) {
	g.Release()
	s.mu.Lock()
	if s.commitGate == g {
		s.commitGate = nil
	}
	s.mu.Unlock()
}

func (s *controlledStorage) failNextCommit(err error) {
	s.mu.Lock()
	s.commitFailure = err
	s.mu.Unlock()
}

func (s *controlledStorage) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

// taskWrites returns the task writes of every commit for id, in order.
func (s *controlledStorage) taskWrites(id durable.TaskID) []durable.TaskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []durable.TaskRecord
	for _, writes := range s.commits {
		for _, write := range writes {
			if task, ok := write.(durable.TaskWrite); ok && task.Value.ID == id {
				out = append(out, task.Value)
			}
		}
	}
	return out
}

// lastTaskWrites returns the task writes of the last commit.
func (s *controlledStorage) lastTaskWrites() []durable.TaskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []durable.TaskRecord
	if len(s.commits) == 0 {
		return nil
	}
	for _, write := range s.commits[len(s.commits)-1] {
		if task, ok := write.(durable.TaskWrite); ok {
			out = append(out, task.Value)
		}
	}
	return out
}

// reportLog collects what OnReport receives.
type reportLog struct {
	mu      sync.Mutex
	reports []error
}

func (r *reportLog) add(err error) {
	r.mu.Lock()
	r.reports = append(r.reports, err)
	r.mu.Unlock()
}

func (r *reportLog) all() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.reports...)
}

// stepState is the checkpoint of a one-phase task.
type stepState struct {
	Phase string `json:"phase"`
}

type stepTask = Running[any, stepState]

// oneStep is a one-phase task; a nil abort settles aborted with reason
// "test".
func oneStep(t *testing.T, name string, run func(ctx context.Context, task *stepTask, rt *TaskRuntime) error, abort func(ctx context.Context, rt *TaskRuntime) error) Task {
	t.Helper()
	if abort == nil {
		abort = func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Abort("test"), nil })
		}
	}
	task, err := DefineTask(TaskSpec[any, stepState, any]{
		Name:    name,
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"run": run,
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return abort(ctx, rt) },
	})
	must(t, err)
	return task
}

// gatedTask is a one-phase task that waits for release and completes with
// null.
func gatedTask(t *testing.T, name string, release <-chan struct{}) Task {
	return oneStep(t, name, func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		<-release
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
}

// completeWith is a runtime commit change that completes with result.
func completeWith(result any) func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Complete(result) }
}

// abortWith is a runtime commit change that aborts with reason.
func abortWith(reason string) func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Abort(reason), nil }
}

// noChange is a runtime commit change that leaves the state alone.
func noChange(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return nil, nil }

type openOptions struct {
	storage  durable.Storage
	registry Registry
	now      func() time.Time
	settings func() HarnessSettings
}

type opened struct {
	h        *Harness
	registry Registry
	reports  *reportLog
	root     *Conversation
}

// openRoot opens a Harness whose registry holds tasks, with its root
// conversation; reports are collected.
func openRoot(t *testing.T, tasks []Task, options openOptions) opened {
	t.Helper()
	registry := options.registry
	if registry == nil {
		registry = NewRegistry()
	}
	if len(tasks) > 0 {
		must(t, registry.Install(&Extension{Name: "tasks", Tasks: tasks}))
	}
	storage := options.storage
	if storage == nil {
		storage = memory.New()
	}
	reports := &reportLog{}
	h, err := Open(bg, storage, Options{Registry: registry, Now: options.now, Settings: options.settings, OnReport: reports.add})
	must(t, err)
	t.Cleanup(func() { closeSoon(t, h) })
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	return opened{h: h, registry: registry, reports: reports, root: root}
}

// closeSoon closes the Harness, failing the test when close hangs.
func closeSoon(t *testing.T, h *Harness) {
	done := make(chan struct{})
	go func() {
		_ = h.Close(bg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("close hung")
	}
}

// start creates task in the conversation, owned by it.
func start(t *testing.T, conversation *Conversation, task Task, background bool) durable.TaskID {
	t.Helper()
	var id durable.TaskID
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, Background: background})
		return err
	}))
	return id
}

// eventually polls check until it holds, failing after five seconds.
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// flush gives pending goroutines a moment to run.
func flush() { time.Sleep(10 * time.Millisecond) }

// settledWithin reports whether ch is closed within a short wait.
func settledWithin(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(30 * time.Millisecond):
		return false
	}
}

// waitDone waits for a task's terminal record, failing after ten seconds.
func waitDone(t *testing.T, h *Harness, id durable.TaskID) durable.TaskRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	record, err := h.WaitForTask(ctx, id)
	must(t, err)
	return record
}

func outcomeJSON(t *testing.T, record durable.TaskRecord) string {
	t.Helper()
	return jsonOf(t, record.State.Outcome)
}

func getTask(t *testing.T, h *Harness, id durable.TaskID) *durable.TaskRecord {
	t.Helper()
	record, err := h.GetTask(bg, id)
	must(t, err)
	return record
}

// async runs f on a goroutine and returns its result channel.
func async[T any](f func() (T, error)) <-chan result[T] {
	ch := make(chan result[T], 1)
	go func() {
		value, err := f()
		ch <- result[T]{value, err}
	}()
	return ch
}

type result[T any] struct {
	value T
	err   error
}

func await[T any](t *testing.T, ch <-chan result[T]) (T, error) {
	t.Helper()
	select {
	case r := <-ch:
		return r.value, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return")
		var zero T
		return zero, errors.New("timeout")
	}
}
