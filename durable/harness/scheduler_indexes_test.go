package harness

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi harness-scheduler-indexes.test.ts and harness-scheduler-retention.test.ts:
// the scheduler's in-memory indexes, where a wrong one changes behaviour, and
// their bound by live work. Retention's cases with queued inputs come with
// submissions.

// indexSizes are the sizes of the scheduler's ownership indexes: pi's
// SCHEDULER_INDEX_SIZES, read here from the fields themselves.
type indexSizes struct {
	Live, Settled, Edges, Below, Roots, Unloaded, DropQueue int
}

func sizesOf(h *Harness) indexSizes {
	s := h.tasks
	s.mu.Lock()
	defer s.mu.Unlock()
	below := 0
	for _, children := range s.below {
		below += children.len()
	}
	return indexSizes{len(s.live), len(s.settled), len(s.edges), below, s.roots.len(), s.unloaded.len(), s.dropQueue.len()}
}

// quietSizes waits until the scheduler is quiet and no sweep is pending,
// then reads the sizes. pi reads them after its awaits, which flush the
// sweep job; a Go commit returns before it runs.
func quietSizes(t *testing.T, h *Harness) indexSizes {
	t.Helper()
	schedulerQuiet(t, h)
	eventually(t, func() bool {
		s := h.tasks
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.sweepScheduled && s.dropQueue.len() == 0
	})
	schedulerQuiet(t, h)
	return sizesOf(h)
}

// spyStorage observes the commits and reads a storage serves; a conversation
// read may be held.
type spyStorage struct {
	durable.Storage
	tasks, conversations atomic.Int64
	mu                   sync.Mutex
	onCommit             func(writes []durable.StorageWrite)
	onConversation       func(id durable.ConversationID)
}

func (s *spyStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	hook := s.onCommit
	s.mu.Unlock()
	if hook != nil {
		hook(writes)
	}
	return s.Storage.Commit(ctx, writes)
}

func (s *spyStorage) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	s.tasks.Add(1)
	return s.Storage.Task(ctx, id)
}

func (s *spyStorage) Conversation(ctx context.Context, id durable.ConversationID) (*durable.ConversationRecord, error) {
	s.conversations.Add(1)
	s.mu.Lock()
	hook := s.onConversation
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return s.Storage.Conversation(ctx, id)
}

func (s *spyStorage) reads() int64 { return s.tasks.Load() + s.conversations.Load() }

func (s *spyStorage) setCommit(hook func(writes []durable.StorageWrite)) {
	s.mu.Lock()
	s.onCommit = hook
	s.mu.Unlock()
}

func (s *spyStorage) setConversation(hook func(id durable.ConversationID)) {
	s.mu.Lock()
	s.onConversation = hook
	s.mu.Unlock()
}

// openPlain opens a paused Harness over storage with tasks, without taking
// the root handle, which a held read would block.
func openPlain(t *testing.T, storage durable.Storage, tasks ...Task) opened {
	t.Helper()
	registry := NewRegistry()
	must(t, registry.Install(&Extension{Name: "tasks", Tasks: tasks}))
	reports := &reportLog{}
	h, err := Open(bg, storage, Options{Registry: registry, OnReport: reports.add})
	must(t, err)
	t.Cleanup(func() { closeSoon(t, h) })
	return opened{h: h, registry: registry, reports: reports}
}

// rewrite replaces a task record through the raw task write.
func rewrite(t *testing.T, h *Harness, id durable.TaskID, change func(*durable.TaskRecord)) {
	t.Helper()
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		record, err := tx.Task(id)
		if err != nil {
			return err
		}
		updated := record.Clone()
		change(&updated)
		return tx.SetTask(updated)
	}))
}

// ─── Indexes ───────────────────────────────────────────────────────────────

func TestIndexesTakeACascadesReasonFromTheNearestCancellingOwner(t *testing.T) {
	// top (abort requested) -> middle (restart-marked) -> leaf: the leaf gets
	// middle's restart first, as the nearest cancelling owner, and the
	// request only once middle carries it.
	f := newOwnership(t)
	directory := t.TempDir()
	session := durable.NewSession(openJSONL(t, directory), durable.SessionOptions{})
	var top, middle, leaf durable.TaskID
	must(t, session.Commit(bg, func(tx *durable.Tx) error {
		conversation, err := tx.CreateConversation(durable.Ownerless)
		if err != nil {
			return err
		}
		top = f.holdIn(t, tx, "top", inConversation(conversation.ID, false))
		middle = f.holdIn(t, tx, "middle", taskOwned(top))
		leaf = f.holdIn(t, tx, "leaf", taskOwned(middle))
		return nil
	}))
	must(t, session.Commit(bg, func(tx *durable.Tx) error {
		topRecord, err := tx.Task(top)
		if err != nil {
			return err
		}
		middleRecord, err := tx.Task(middle)
		if err != nil {
			return err
		}
		marked := topRecord.Clone()
		marked.AbortRequested = true
		if err := tx.SetTask(marked); err != nil {
			return err
		}
		restart := middleRecord.Clone()
		restart.AbortRequested, restart.AbortReason = true, durable.AbortReasonRestart
		return tx.SetTask(restart)
	}))
	must(t, session.Close(bg))
	// Record every mark of the leaf, in commit order.
	storage := &spyStorage{Storage: openJSONL(t, directory)}
	var mu sync.Mutex
	var reasons []string
	storage.setCommit(func(writes []durable.StorageWrite) {
		for _, write := range writes {
			if task, ok := write.(durable.TaskWrite); ok && task.Value.ID == leaf && task.Value.AbortRequested {
				mu.Lock()
				reasons = append(reasons, task.Value.AbortReason)
				mu.Unlock()
			}
		}
	})
	o := f.openOwned(t, storage)
	for _, id := range []durable.TaskID{leaf, middle, top} {
		waitDone(t, o.h, id)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) == 0 || reasons[0] != durable.AbortReasonRestart {
		t.Fatalf("leaf mark reasons = %q", reasons)
	}
}

func TestIndexesFinalizeAChainOfHeldOwnersInTheCommitThatFreesTheLastOne(t *testing.T) {
	f := newOwnership(t)
	storage := &spyStorage{Storage: memory.New()}
	// The commit, by count, that wrote each terminal record.
	var mu sync.Mutex
	endedIn := map[durable.TaskID]int{}
	commits := 0
	storage.setCommit(func(writes []durable.StorageWrite) {
		mu.Lock()
		defer mu.Unlock()
		commits++
		for _, write := range writes {
			if task, ok := write.(durable.TaskWrite); ok && task.Value.State.Status == durable.TaskTerminal {
				endedIn[task.Value.ID] = commits
			}
		}
	})
	o := f.openOwned(t, storage)
	var top, middle, leaf durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		top = f.holdIn(t, tx, "top", conversationOwned(false))
		middle = f.holdIn(t, tx, "middle", taskOwned(top))
		leaf = f.holdIn(t, tx, "leaf", taskOwned(middle))
		return nil
	}))
	f.open("top", durable.OutcomeCompleted)
	f.open("middle", durable.OutcomeCompleted)
	waitStatus(t, o.h, top, durable.TaskCompleting)
	waitStatus(t, o.h, middle, durable.TaskCompleting)
	f.open("leaf", durable.OutcomeCompleted)
	expectEnded(t, o.h, top, durable.OutcomeCompleted)
	mu.Lock()
	defer mu.Unlock()
	// Both owners end in one finalize commit, after the leaf's.
	if endedIn[middle] != endedIn[top] || endedIn[middle] <= endedIn[leaf] {
		t.Fatalf("ended in commits: leaf %d, middle %d, top %d", endedIn[leaf], endedIn[middle], endedIn[top])
	}
}

func TestIndexesKeepAScopeBusyAfterReopenForWorkBelowAnEndedTaskWhoseChainIsNotLoadedYet(t *testing.T) {
	f := newOwnership(t)
	directory := t.TempDir()
	o := f.openOwned(t, openJSONL(t, directory))
	// root conversation -> owner (ends) -> conversation C -> inner (created
	// after owner ended), as a subagent call's.
	var owner durable.TaskID
	var child durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		owner = f.holdIn(t, tx, "owner", conversationOwned(false))
		conversation, err := tx.CreateConversation(ownedBy(owner))
		child = conversation.ID
		return err
	}))
	f.open("owner", durable.OutcomeCompleted)
	waitDone(t, o.h, owner)
	inner := f.create(t, conversation(t, o.h, child), "inner")
	must(t, o.h.Close(bg))

	// Hold the read of C's record, so the chain above inner stays unloaded
	// while idle is decided.
	storage := &spyStorage{Storage: openJSONL(t, directory)}
	held := make(chan struct{})
	storage.setConversation(func(id durable.ConversationID) {
		if id == child {
			<-held
		}
	})
	p := openPlain(t, storage, f.hold, f.waiter, f.spawner)
	// A task whose chain is not loaded counts as inside every scope. (The
	// held read blocks the Session line, so the root handle comes after.)
	if settledResult(idle(t, p.h.WaitForIdle)) {
		t.Fatal("idle while the chain of live work was not loaded")
	}
	close(held)
	root, err := p.h.Root(bg, nil, nil)
	must(t, err)
	must(t, p.h.Resume())
	f.open("inner", durable.OutcomeCompleted)
	waitDone(t, p.h, inner)
	must(t, root.WaitForIdle(bg))
	must(t, p.h.Close(bg))
}

// probes is the probe task: it records its owned tasks once its "look.<name>"
// gate opens, then holds until its own gate opens.
type probes struct {
	f        *ownership
	mu       sync.Mutex
	owned    map[string][]durable.TaskID
	runtimes map[string]*TaskRuntime
	task     Task
}

func newProbes(f *ownership) *probes {
	p := &probes{f: f, owned: map[string][]durable.TaskID{}, runtimes: map[string]*TaskRuntime{}}
	wait := func(ctx context.Context, name string) error {
		select {
		case <-f.gate(name).ch:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	p.task = MustDefineTask(TaskSpec[holdInput, holdState, any]{
		Name:    "test.index.probe",
		Version: 1,
		Initial: func(holdInput) holdState { return holdState{Phase: "look"} },
		Phases: map[string]func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error{
			"look": func(ctx context.Context, task *Running[holdInput, holdState], rt *TaskRuntime) error {
				name := task.Input.Name
				p.mu.Lock()
				p.runtimes[name] = rt
				p.mu.Unlock()
				if err := wait(ctx, "look."+name); err != nil {
					return err
				}
				records, err := rt.OwnedTasks(ctx)
				if err != nil {
					return err
				}
				p.mu.Lock()
				p.owned[name] = names(records, func(r durable.TaskRecord) durable.TaskID { return r.ID })
				p.mu.Unlock()
				if err := wait(ctx, name); err != nil {
					return err
				}
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(ctx context.Context, _ *Running[holdInput, holdState], rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith(""))
		},
	})
	return p
}

func (p *probes) ownedBy(name string) ([]durable.TaskID, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids, ok := p.owned[name]
	return ids, ok
}

func (p *probes) runtime(name string) *TaskRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtimes[name]
}

func (p *probes) reset() {
	p.mu.Lock()
	p.owned = map[string][]durable.TaskID{}
	p.mu.Unlock()
}

func TestOwnedTasksListsTheLiveTasksATaskOwnsDirectlyInIDOrderAlsoAfterAReopen(t *testing.T) {
	f := newOwnership(t)
	p := newProbes(f)
	directory := t.TempDir()
	o := openRoot(t, []Task{f.hold, p.task}, openOptions{storage: openJSONL(t, directory)})
	must(t, o.h.Resume())
	// The probe owns: two held tasks, one that ends, a grandchild below the
	// first, and a conversation with a task.
	var probe, first, ends, second durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if probe, err = CreateTask(tx, p.task, holdInput{Name: "probe"}, conversationOwned(false)); err != nil {
			return err
		}
		first = f.holdIn(t, tx, "first", taskOwned(probe))
		ends = f.holdIn(t, tx, "ends", taskOwned(probe))
		second = f.holdIn(t, tx, "second", taskOwned(probe))
		f.holdIn(t, tx, "grandchild", taskOwned(first))
		child, err := tx.CreateConversation(ownedBy(probe))
		if err != nil {
			return err
		}
		f.holdIn(t, tx, "inside", inConversation(child.ID, false))
		return nil
	}))
	f.open("ends", durable.OutcomeCompleted)
	waitDone(t, o.h, ends)
	// The first ends too, but holds its outcome as completing while its
	// grandchild runs: still live, so listed.
	f.open("first", durable.OutcomeCompleted)
	waitStatus(t, o.h, first, durable.TaskCompleting)
	f.open("look.probe", durable.OutcomeCompleted)
	eventually(t, func() bool { _, ok := p.ownedBy("probe"); return ok })
	if got, _ := p.ownedBy("probe"); !reflect.DeepEqual(got, []durable.TaskID{first, second}) {
		t.Fatalf("owned = %v, want %v", got, []durable.TaskID{first, second})
	}
	must(t, o.h.Close(bg))

	// After a reopen the rerun sees the same tasks, which an earlier
	// invocation created.
	p.reset()
	o = openRoot(t, []Task{f.hold, p.task}, openOptions{storage: openJSONL(t, directory)})
	must(t, o.h.Resume())
	eventually(t, func() bool { _, ok := p.ownedBy("probe"); return ok })
	if got, _ := p.ownedBy("probe"); !reflect.DeepEqual(got, []durable.TaskID{first, second}) {
		t.Fatalf("owned after reopen = %v, want %v", got, []durable.TaskID{first, second})
	}
	must(t, o.h.Close(bg))
}

func TestOwnedTasksRejectsOnceItsInvocationHasEnded(t *testing.T) {
	f := newOwnership(t)
	p := newProbes(f)
	o := openRoot(t, []Task{f.hold, p.task}, openOptions{})
	must(t, o.h.Resume())
	var probe durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		probe, err = CreateTask(tx, p.task, holdInput{Name: "probe"}, conversationOwned(false))
		return err
	}))
	eventually(t, func() bool { return p.runtime("probe") != nil })
	rt := p.runtime("probe")
	// Aborting joins the run invocation, which has ended by then.
	if _, err := o.h.AbortTask(bg, probe); err != nil {
		t.Fatal(err)
	}
	_, err := rt.OwnedTasks(bg)
	expectError(t, err, "invocation has ended")
}
