package harness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-ownership.test.ts. Its cases with queued
// submissions come with the submissions stage, and the "owned conversations
// from tools and supervisors" block with the tool and generation stages; the
// SQLite reopens run over memory storage that survives Close.

type holdInput struct {
	Name      string `json:"name"`
	SlowAbort bool   `json:"slowAbort,omitempty"`
}

type holdState struct {
	Phase string `json:"phase"`
}

type waiterInput struct {
	On []durable.TaskID `json:"on"`
}

// ending is the outcome a held task commits once its gate opens.
type ending struct {
	once  sync.Once
	ch    chan struct{}
	value string
}

// ownership holds the tasks of the ownership tests and their named gates.
type ownership struct {
	mu    sync.Mutex
	gates map[string]*ending
	runs  map[string]int

	// hold holds until its named gate opens or it is aborted; with
	// SlowAbort, its abort handler first waits for the gate "abort.<name>".
	hold Task
	// waiter waits on the tasks in its input, then completes.
	waiter Task
	// spawner tries to create, in the commit that ends it, a conversation it
	// owns with a held task in it.
	spawner Task
	// unregistered is never registered: aborting it can only orphan it.
	unregistered Task
}

func newOwnership(t *testing.T) *ownership {
	t.Helper()
	f := &ownership{gates: map[string]*ending{}, runs: map[string]int{}}
	abortTask := func(ctx context.Context, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) }
	f.hold = MustDefineTask(TaskSpec[holdInput, holdState, any]{
		Name:    "test.hold",
		Version: 1,
		Initial: func(holdInput) holdState { return holdState{Phase: "hold"} },
		Phases: map[string]func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error{
			"hold": func(ctx context.Context, task *Running[holdInput, holdState], rt *TaskRuntime) error {
				f.mu.Lock()
				f.runs[task.Input.Name]++
				f.mu.Unlock()
				g := f.gate(task.Input.Name)
				select {
				case <-g.ch:
				case <-ctx.Done():
					return context.Cause(ctx)
				}
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					if g.value == durable.OutcomeCompleted {
						return Complete(nil)
					}
					return Fail("gate failed"), nil
				})
			},
		},
		Abort: func(ctx context.Context, task *Running[holdInput, holdState], rt *TaskRuntime) error {
			if task.Input.SlowAbort {
				// Pi's handler ignores its signal; its close always wins the
				// race against the handler's start, Go's does not, so the
				// handler leaves without settling once its invocation ends.
				select {
				case <-f.gate("abort." + task.Input.Name).ch:
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			return abortTask(ctx, rt)
		},
	})
	f.waiter = MustDefineTask(TaskSpec[waiterInput, holdState, any]{
		Name:    "test.waiter",
		Version: 1,
		Initial: func(waiterInput) holdState { return holdState{Phase: "wait"} },
		Phases: map[string]func(context.Context, *Running[waiterInput, holdState], *TaskRuntime) error{
			"wait": func(ctx context.Context, task *Running[waiterInput, holdState], rt *TaskRuntime) error {
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Wait(task.Input.On, durable.JoinAllSettled, holdState{Phase: "done"})
				})
			},
			"done": func(ctx context.Context, _ *Running[waiterInput, holdState], rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(ctx context.Context, _ *Running[waiterInput, holdState], rt *TaskRuntime) error {
			return abortTask(ctx, rt)
		},
	})
	f.spawner = MustDefineTask(TaskSpec[holdInput, holdState, any]{
		Name:    "test.spawner",
		Version: 1,
		Initial: func(holdInput) holdState { return holdState{Phase: "spawn"} },
		Phases: map[string]func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error{
			"spawn": func(ctx context.Context, task *Running[holdInput, holdState], rt *TaskRuntime) error {
				return rt.Commit(ctx, func(tx *durable.Tx, record durable.TaskRecord) (*durable.TaskState, error) {
					child, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: record.ID})
					if err != nil {
						return nil, err
					}
					if _, err := CreateTask(tx, f.hold, holdInput{Name: task.Input.Name}, inConversation(child.ID, false)); err != nil {
						return nil, err
					}
					return Complete(nil)
				})
			},
		},
		Abort: func(ctx context.Context, _ *Running[holdInput, holdState], rt *TaskRuntime) error {
			return abortTask(ctx, rt)
		},
	})
	f.unregistered = MustDefineTask(TaskSpec[holdInput, holdState, any]{
		Name:    "test.unregistered",
		Version: 1,
		Initial: func(holdInput) holdState { return holdState{Phase: "hold"} },
		Phases: map[string]func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error{
			"hold": func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error { return nil },
		},
		Abort: func(context.Context, *Running[holdInput, holdState], *TaskRuntime) error { return nil },
	})
	return f
}

func (f *ownership) gate(name string) *ending {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.gates[name]
	if g == nil {
		g = &ending{ch: make(chan struct{})}
		f.gates[name] = g
	}
	return g
}

// open lets the task held at name commit value: "completed" or "failed".
func (f *ownership) open(name, value string) {
	g := f.gate(name)
	g.once.Do(func() {
		g.value = value
		close(g.ch)
	})
}

func (f *ownership) ran(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[name]
}

// openOwned opens a resumed Harness over storage (fresh memory when nil)
// with the ownership tasks.
func (f *ownership) openOwned(t *testing.T, storage durable.Storage) opened {
	t.Helper()
	o := openRoot(t, []Task{f.hold, f.waiter, f.spawner}, openOptions{storage: storage})
	must(t, o.h.Resume())
	return o
}

func conversationOwned(background bool) durable.TaskOptions {
	return durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, Background: background}
}

func inConversation(id durable.ConversationID, background bool) durable.TaskOptions {
	options := conversationOwned(background)
	options.ConversationID = id
	return options
}

func taskOwned(owner durable.TaskID) durable.TaskOptions {
	return durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: owner}}
}

func ownedBy(owner durable.TaskID) durable.ConversationOwnership {
	return durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: owner}
}

// holdIn creates a held task named name.
func (f *ownership) holdIn(t *testing.T, tx *durable.Tx, name string, options durable.TaskOptions) durable.TaskID {
	t.Helper()
	id, err := CreateTask(tx, f.hold, holdInput{Name: name}, options)
	must(t, err)
	return id
}

// create commits a held task named name owned by conversation c.
func (f *ownership) create(t *testing.T, c *Conversation, name string) durable.TaskID {
	t.Helper()
	var id durable.TaskID
	must(t, c.Commit(bg, func(tx *durable.Tx) error {
		id = f.holdIn(t, tx, name, conversationOwned(false))
		return nil
	}))
	return id
}

type tree struct {
	owner, inner durable.TaskID
	child        durable.ConversationID
}

type treeOptions struct {
	background, slowInner, slowOwner bool
}

// ownedChild stages, in parent and one commit, task owner and a
// conversation it owns holding task inner.
func (f *ownership) ownedChild(t *testing.T, parent *Conversation, name string, options treeOptions) tree {
	t.Helper()
	var tr tree
	must(t, parent.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if tr.owner, err = CreateTask(tx, f.hold, holdInput{Name: name, SlowAbort: options.slowOwner}, conversationOwned(options.background)); err != nil {
			return err
		}
		child, err := tx.CreateConversation(ownedBy(tr.owner))
		if err != nil {
			return err
		}
		tr.child = child.ID
		tr.inner, err = CreateTask(tx, f.hold, holdInput{Name: name + ".inner", SlowAbort: options.slowInner}, inConversation(child.ID, false))
		return err
	}))
	return tr
}

func conversation(t *testing.T, h *Harness, id durable.ConversationID) *Conversation {
	t.Helper()
	c, err := h.Conversation(bg, id)
	must(t, err)
	if c == nil {
		t.Fatalf("conversation %d is missing", id)
	}
	return c
}

func statusOf(t *testing.T, h *Harness, id durable.TaskID) string {
	t.Helper()
	return getTask(t, h, id).State.Status
}

func waitStatus(t *testing.T, h *Harness, id durable.TaskID, status string) {
	t.Helper()
	eventually(t, func() bool { return statusOf(t, h, id) == status })
}

func waitMarked(t *testing.T, h *Harness, id durable.TaskID) {
	t.Helper()
	eventually(t, func() bool { return getTask(t, h, id).AbortRequested })
}

// ended waits for the task's terminal record and returns its outcome status.
func ended(t *testing.T, h *Harness, id durable.TaskID) string {
	t.Helper()
	return waitDone(t, h, id).State.Outcome.Status
}

func expectEnded(t *testing.T, h *Harness, id durable.TaskID, want string) {
	t.Helper()
	if got := ended(t, h, id); got != want {
		t.Fatalf("task %d ended %s, want %s", id, got, want)
	}
}

func expectLive(t *testing.T, h *Harness, ids ...durable.TaskID) {
	t.Helper()
	for _, id := range ids {
		if statusOf(t, h, id) == durable.TaskTerminal {
			t.Fatalf("task %d is terminal", id)
		}
	}
}

// idle waits for idle on a goroutine; the wait is cancelled at cleanup.
func idle(t *testing.T, wait func(context.Context) error) <-chan result[struct{}] {
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	return async(func() (struct{}, error) { return struct{}{}, wait(ctx) })
}

// settledResult reports whether ch delivers within a short wait.
func settledResult[T any](ch <-chan result[T]) bool {
	select {
	case <-ch:
		return true
	case <-time.After(30 * time.Millisecond):
		return false
	}
}

func expectBusy(t *testing.T, waits ...func(context.Context) error) {
	t.Helper()
	for _, wait := range waits {
		if settledResult(idle(t, wait)) {
			t.Fatal("idle while work is live")
		}
	}
}

func TestOwnershipKeepsAConversationBusyWhileItsOwnedForegroundSubtreeHasLiveWorkHoldingTheCompletedOwner(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.ownedChild(t, o.root, "owner", treeOptions{})
	f.open("owner", durable.OutcomeCompleted)
	waitStatus(t, o.h, tr.owner, durable.TaskCompleting)
	expectBusy(t, o.root.WaitForIdle, o.h.WaitForIdle)
	f.open("owner.inner", durable.OutcomeCompleted)
	must(t, o.root.WaitForIdle(bg))
	must(t, o.h.WaitForIdle(bg))
	expectEnded(t, o.h, tr.owner, durable.OutcomeCompleted)
}

func TestOwnershipHoldsAnOwnerForWorkInAConversationItsEndedChildTaskOwns(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	var owner, child durable.TaskID
	var conv durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		owner = f.holdIn(t, tx, "owner", conversationOwned(false))
		child = f.holdIn(t, tx, "child", taskOwned(owner))
		record, err := tx.CreateConversation(ownedBy(child))
		conv = record.ID
		return err
	}))
	f.open("child", durable.OutcomeCompleted)
	expectEnded(t, o.h, child, durable.OutcomeCompleted)
	// The ended child's conversation still belongs to the owner above it.
	f.create(t, conversation(t, o.h, conv), "late")
	f.open("owner", durable.OutcomeCompleted)
	waitStatus(t, o.h, owner, durable.TaskCompleting)
	f.open("late", durable.OutcomeCompleted)
	expectEnded(t, o.h, owner, durable.OutcomeCompleted)
}

func TestOwnershipCascadesThroughEndedTasksIntoAnOwnedBackgroundSubtreeWhenAbortingAcrossBackground(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	// root conversation -> A -> B -> conversation -> background C -> D
	var a, c, d durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		a = f.holdIn(t, tx, "a", conversationOwned(false))
		b := f.holdIn(t, tx, "b", taskOwned(a))
		conv, err := tx.CreateConversation(ownedBy(b))
		if err != nil {
			return err
		}
		c = f.holdIn(t, tx, "c", inConversation(conv.ID, true))
		d = f.holdIn(t, tx, "d", taskOwned(c))
		return nil
	}))
	f.open("b", durable.OutcomeCompleted)
	f.open("a", durable.OutcomeCompleted)
	expectEnded(t, o.h, a, durable.OutcomeCompleted)
	must(t, o.root.Abort(bg, true))
	expectEnded(t, o.h, c, durable.OutcomeAborted)
	expectEnded(t, o.h, d, durable.OutcomeAborted)
}

func TestOwnershipTracksOwnershipChainsDeeperThanTheCallStack(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	// 20,000 tasks, each owning the next, created in one commit above a live
	// leaf; they end top-down and hold.
	const depth = 20_000
	var top, leaf durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		top = f.holdIn(t, tx, "top", conversationOwned(false))
		owner := top
		for n := 0; n < depth; n++ {
			owner = f.holdIn(t, tx, fmt.Sprintf("link%d", n), taskOwned(owner))
		}
		leaf = f.holdIn(t, tx, "leaf", taskOwned(owner))
		return nil
	}))
	for n := 0; n < depth; n++ {
		f.open(fmt.Sprintf("link%d", n), durable.OutcomeCompleted)
	}
	f.open("top", durable.OutcomeCompleted)
	waitStatus(t, o.h, top, durable.TaskCompleting)
	f.open("leaf", durable.OutcomeCompleted)
	expectEnded(t, o.h, leaf, durable.OutcomeCompleted)
	expectEnded(t, o.h, top, durable.OutcomeCompleted)
}

// bridgeTree is live P owning B, which owns A, which owns the empty
// conversation C.
type bridgeTree struct {
	p, b, a durable.TaskID
	c       durable.ConversationID
}

func (f *ownership) bridge(t *testing.T, root *Conversation) bridgeTree {
	t.Helper()
	var tr bridgeTree
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		tr.p = f.holdIn(t, tx, "p", conversationOwned(false))
		tr.b = f.holdIn(t, tx, "b", taskOwned(tr.p))
		tr.a = f.holdIn(t, tx, "a", taskOwned(tr.b))
		conv, err := tx.CreateConversation(ownedBy(tr.a))
		tr.c = conv.ID
		return err
	}))
	return tr
}

func TestOwnershipHoldsAnOwnerForWorkACommitCreatesBelowEndedTasksLoadedFromStorage(t *testing.T) {
	f := newOwnership(t)
	storage := &survivingStorage{memory.New()}
	first := f.openOwned(t, storage)
	tr := f.bridge(t, first.root)
	f.open("a", durable.OutcomeCompleted)
	f.open("b", durable.OutcomeCompleted)
	waitDone(t, first.h, tr.a)
	waitDone(t, first.h, tr.b)
	must(t, first.h.Close(bg))
	// After reopen, B and A are known only once a chain load reads them.
	o := f.openOwned(t, storage)
	d := f.create(t, conversation(t, o.h, tr.c), "d")
	f.open("p", durable.OutcomeCompleted)
	waitStatus(t, o.h, tr.p, durable.TaskCompleting)
	f.open("d", durable.OutcomeCompleted)
	expectEnded(t, o.h, d, durable.OutcomeCompleted)
	expectEnded(t, o.h, tr.p, durable.OutcomeCompleted)
}

func TestOwnershipHoldsAnOwnerForWorkCreatedLaterBelowEndedTasksWithoutAReopen(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.bridge(t, o.root)
	f.open("a", durable.OutcomeCompleted)
	f.open("b", durable.OutcomeCompleted)
	waitDone(t, o.h, tr.b)
	d := f.create(t, conversation(t, o.h, tr.c), "d")
	f.open("p", durable.OutcomeCompleted)
	waitStatus(t, o.h, tr.p, durable.TaskCompleting)
	f.open("d", durable.OutcomeCompleted)
	expectEnded(t, o.h, d, durable.OutcomeCompleted)
	expectEnded(t, o.h, tr.p, durable.OutcomeCompleted)
}

// endedBridge stages live P owning depth ended tasks in a chain, the last
// owning conversation C, empty when they ended. Work started in C later is
// still P's.
func (f *ownership) endedBridge(t *testing.T, depth int) (opened, durable.TaskID, *Conversation) {
	t.Helper()
	o := f.openOwned(t, nil)
	var p durable.TaskID
	var links []durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if p, err = CreateTask(tx, f.hold, holdInput{Name: "p", SlowAbort: true}, conversationOwned(false)); err != nil {
			return err
		}
		owner := p
		for n := 0; n < depth; n++ {
			owner = f.holdIn(t, tx, fmt.Sprintf("link%d", n), taskOwned(owner))
			links = append(links, owner)
		}
		conv, err := tx.CreateConversation(ownedBy(owner))
		c = conv.ID
		return err
	}))
	for n := depth - 1; n >= 0; n-- {
		f.open(fmt.Sprintf("link%d", n), durable.OutcomeCompleted)
	}
	for _, id := range links {
		waitDone(t, o.h, id)
	}
	return o, p, conversation(t, o.h, c)
}

func TestOwnershipAbortsWorkBelowEndedTasksWithTheirLiveOwnerWhichWaitsForIt(t *testing.T) {
	for _, depth := range []int{2, 5} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			f := newOwnership(t)
			o, p, c := f.endedBridge(t, depth)
			d := f.create(t, c, "d")
			if _, err := o.h.AbortTask(bg, p); err != nil {
				t.Fatal(err)
			}
			expectEnded(t, o.h, d, durable.OutcomeAborted)
			// P's abort handler ran only after D ended.
			f.open("abort.p", durable.OutcomeCompleted)
			expectEnded(t, o.h, p, durable.OutcomeAborted)
		})
	}
}

func TestOwnershipKeepsTheRootBusyAndAbortsWithItWorkBelowEndedTasks(t *testing.T) {
	for _, depth := range []int{2, 5} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			f := newOwnership(t)
			o, p, c := f.endedBridge(t, depth)
			f.open("p", durable.OutcomeCompleted)
			waitDone(t, o.h, p)
			// P has ended: D and D2 hang below ended tasks only, and are still
			// the root conversation's work.
			d := f.create(t, c, "d")
			d2 := f.create(t, c, "d2")
			expectBusy(t, o.root.WaitForIdle, o.h.WaitForIdle)
			must(t, o.root.Abort(bg, false))
			expectEnded(t, o.h, d, durable.OutcomeAborted)
			expectEnded(t, o.h, d2, durable.OutcomeAborted)
		})
	}
}

func TestOwnershipReachesWorkBelowEndedTasksAcrossTwoOwnedConversations(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	// Live P -> ended B -> conversation C -> ended X -> ended A ->
	// conversation C2; work starts in C2 later. X owns no conversation, only
	// the task A, so only owning a known task keeps it.
	var p, b durable.TaskID
	var c2 durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		p = f.holdIn(t, tx, "p", conversationOwned(false))
		b = f.holdIn(t, tx, "b", taskOwned(p))
		c, err := tx.CreateConversation(ownedBy(b))
		if err != nil {
			return err
		}
		x := f.holdIn(t, tx, "x", inConversation(c.ID, false))
		a := f.holdIn(t, tx, "a", taskOwned(x))
		conv, err := tx.CreateConversation(ownedBy(a))
		c2 = conv.ID
		return err
	}))
	f.open("a", durable.OutcomeCompleted)
	f.open("x", durable.OutcomeCompleted)
	f.open("b", durable.OutcomeCompleted)
	waitDone(t, o.h, b)
	d := f.create(t, conversation(t, o.h, c2), "d")
	f.open("p", durable.OutcomeCompleted)
	waitStatus(t, o.h, p, durable.TaskCompleting)
	expectBusy(t, o.root.WaitForIdle)
	must(t, o.root.Abort(bg, false))
	expectEnded(t, o.h, d, durable.OutcomeAborted)
	expectEnded(t, o.h, p, durable.OutcomeCompleted)
}

func TestOwnershipFreesAnOwnerOnceTheLastWorkBelowEndedTasksEndsAfterABackgroundTaskThere(t *testing.T) {
	for _, completes := range []bool{true, false} {
		name := "completes"
		if !completes {
			name = "is aborted"
		}
		t.Run(name, func(t *testing.T) {
			f := newOwnership(t)
			o := f.openOwned(t, nil)
			// Live P owns B, which owns A, which owns conversation C with
			// background Z and, later, foreground D.
			tr := f.bridge(t, o.root)
			var z durable.TaskID
			must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
				z = f.holdIn(t, tx, "z", inConversation(tr.c, true))
				return nil
			}))
			f.open("a", durable.OutcomeCompleted)
			f.open("b", durable.OutcomeCompleted)
			waitDone(t, o.h, tr.b)
			var d durable.TaskID
			must(t, conversation(t, o.h, tr.c).Commit(bg, func(tx *durable.Tx) error {
				var err error
				d, err = CreateTask(tx, f.hold, holdInput{Name: "d", SlowAbort: true}, conversationOwned(false))
				return err
			}))
			if completes {
				f.open("p", durable.OutcomeCompleted)
				waitStatus(t, o.h, tr.p, durable.TaskCompleting)
			} else {
				// P waits for D's abort before its own abort handler runs.
				if _, err := o.h.AbortTask(bg, tr.p); err != nil {
					t.Fatal(err)
				}
				waitMarked(t, o.h, d)
			}
			// Z ends first, D last.
			f.open("z", durable.OutcomeCompleted)
			waitDone(t, o.h, z)
			if completes {
				f.open("d", durable.OutcomeCompleted)
			} else {
				f.open("abort.d", durable.OutcomeCompleted)
			}
			waitDone(t, o.h, d)
			want := durable.OutcomeCompleted
			if !completes {
				want = durable.OutcomeAborted
			}
			expectEnded(t, o.h, tr.p, want)
		})
	}
}

// Pins the Transaction rule that a conversation cannot be created in the
// commit that ends its owner task.
func TestOwnershipRejectsAConversationCreatedInTheCommitThatEndsItsOwnerTask(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	var spawner durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		spawner, err = CreateTask(tx, f.spawner, holdInput{Name: "spawned"}, conversationOwned(false))
		return err
	}))
	outcome := waitDone(t, o.h, spawner).State.Outcome
	pattern := regexp.MustCompile(fmt.Sprintf(`^Conversation owner task %d is (completing|terminal)$`, spawner))
	if outcome.Status != durable.OutcomeFaulted || outcome.Error == nil || !pattern.MatchString(outcome.Error.Message) {
		t.Fatalf("spawner = %s", jsonOf(t, outcome))
	}
}

func TestOwnershipStopsIdleTraversalAtABackgroundOwner(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.ownedChild(t, o.root, "background", treeOptions{background: true})
	must(t, o.root.WaitForIdle(bg))
	must(t, o.h.WaitForIdle(bg))
	// The background child is its own scope.
	child := conversation(t, o.h, tr.child)
	expectBusy(t, child.WaitForIdle)
	must(t, child.Abort(bg, false))
}

func TestOwnershipCascadesAFailedOwnerButNotACompletedOne(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	failed := f.ownedChild(t, o.root, "failed", treeOptions{})
	done := f.ownedChild(t, o.root, "done", treeOptions{})
	f.open("failed", durable.OutcomeFailed)
	f.open("done", durable.OutcomeCompleted)
	expectEnded(t, o.h, failed.inner, durable.OutcomeAborted)
	expectEnded(t, o.h, failed.owner, durable.OutcomeFailed)
	waitStatus(t, o.h, done.owner, durable.TaskCompleting)
	expectLive(t, o.h, done.inner)
	f.open("done.inner", durable.OutcomeCompleted)
	expectEnded(t, o.h, done.owner, durable.OutcomeCompleted)
}

func TestOwnershipAbortsABackgroundTaskDirectlyWithItsOrdinarySubtree(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.ownedChild(t, o.root, "background", treeOptions{background: true})
	if _, err := o.h.AbortTask(bg, tr.owner); err != nil {
		t.Fatal(err)
	}
	expectEnded(t, o.h, tr.inner, durable.OutcomeAborted)
}

func TestOwnershipAbortsWorkCreatedBelowAHeldFailedOwnerButNotBelowATerminalOne(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.ownedChild(t, o.root, "owner", treeOptions{slowInner: true})
	f.open("owner", durable.OutcomeFailed)
	waitMarked(t, o.h, tr.inner)
	if status := statusOf(t, o.h, tr.owner); status != durable.TaskCompleting {
		t.Fatalf("owner is %s", status)
	}
	child := conversation(t, o.h, tr.child)
	during := f.create(t, child, "during")
	expectEnded(t, o.h, during, durable.OutcomeAborted)
	f.open("abort.owner.inner", durable.OutcomeCompleted)
	expectEnded(t, o.h, tr.owner, durable.OutcomeFailed)
	// A terminal owner never cascades: interrogating its conversation runs
	// normally.
	after := f.create(t, child, "after")
	f.open("after", durable.OutcomeCompleted)
	expectEnded(t, o.h, after, durable.OutcomeCompleted)
}

func TestOwnershipKeepsANestedBackgroundOwnersSubtreeWhenItsCancelledBackgroundOwnerIsAborted(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	outer := f.ownedChild(t, o.root, "outer", treeOptions{background: true})
	inner := f.ownedChild(t, conversation(t, o.h, outer.child), "inner", treeOptions{background: true})
	if _, err := o.h.AbortTask(bg, outer.owner); err != nil {
		t.Fatal(err)
	}
	expectEnded(t, o.h, outer.inner, durable.OutcomeAborted)
	expectLive(t, o.h, inner.owner, inner.inner)
	if _, err := o.h.AbortTask(bg, inner.owner); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipDecidesIdleAfterReopenFromOwnerEdgesItHasToLoadFirst(t *testing.T) {
	f := newOwnership(t)
	storage := &survivingStorage{memory.New()}
	first := f.openOwned(t, storage)
	foreground := f.ownedChild(t, first.root, "fg", treeOptions{})
	deeper := f.ownedChild(t, conversation(t, first.h, foreground.child), "fg2", treeOptions{})
	background := f.ownedChild(t, first.root, "bg", treeOptions{background: true})
	f.open("fg", durable.OutcomeCompleted)
	f.open("fg2", durable.OutcomeCompleted)
	// Both owners hold their outcomes while the work below them runs.
	waitStatus(t, first.h, foreground.owner, durable.TaskCompleting)
	waitStatus(t, first.h, deeper.owner, durable.TaskCompleting)
	must(t, first.h.Close(bg))

	o := f.openOwned(t, storage)
	// Two levels below completed foreground owners, the inner tasks keep the
	// root busy.
	expectBusy(t, o.root.WaitForIdle, o.h.WaitForIdle)
	f.open("fg.inner", durable.OutcomeCompleted)
	f.open("fg2.inner", durable.OutcomeCompleted)
	// The background subtree does not count.
	must(t, o.root.WaitForIdle(bg))
	must(t, o.h.WaitForIdle(bg))
	if status := statusOf(t, o.h, foreground.owner); status != durable.TaskTerminal {
		t.Fatalf("foreground owner is %s", status)
	}
	expectLive(t, o.h, background.inner)
	if _, err := o.h.AbortTask(bg, background.owner); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipDerivesMarksACrashLeftUnappliedAtOpen(t *testing.T) {
	cases := []struct {
		name           string
		abortRequested bool
		state          durable.TaskState
		want           string
	}{
		{"below a held failed owner", false, durable.TaskState{Status: durable.TaskCompleting, Outcome: &durable.TaskOutcome{Status: durable.OutcomeFailed, Error: &durable.TaskOutcomeError{Message: "crash"}}}, durable.OutcomeFailed},
		{"below an abort-marked owner", true, durable.TaskState{Status: durable.TaskPending, Checkpoint: []byte(`{"phase":"hold"}`)}, durable.OutcomeAborted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOwnership(t)
			storage := memory.New()
			// Without a Harness, nothing derives marks: a cancelled owner with a
			// live task below it.
			session := durable.NewSession(&survivingStorage{storage}, durable.SessionOptions{})
			var owner, inner durable.TaskID
			must(t, session.Commit(bg, func(tx *durable.Tx) error {
				root, err := tx.CreateConversation(durable.Ownerless)
				if err != nil {
					return err
				}
				owner = f.holdIn(t, tx, "gone", inConversation(root.ID, false))
				child, err := tx.CreateConversation(ownedBy(owner))
				if err != nil {
					return err
				}
				inner = f.holdIn(t, tx, "orphan", inConversation(child.ID, false))
				return nil
			}))
			must(t, session.Commit(bg, func(tx *durable.Tx) error {
				record, err := tx.Task(owner)
				if err != nil {
					return err
				}
				record.AbortRequested, record.State = c.abortRequested, c.state
				return tx.SetTask(*record)
			}))
			must(t, session.Close(bg))

			o := f.openOwned(t, &survivingStorage{storage})
			expectEnded(t, o.h, inner, durable.OutcomeAborted)
			// The owner finishes only after the work below it.
			expectEnded(t, o.h, owner, c.want)
		})
	}
}

func TestOwnershipMarksWorkAdmittedAfterReopenBelowACancelledOwnerWhoseEdgeWasNotLoaded(t *testing.T) {
	f := newOwnership(t)
	storage := &survivingStorage{memory.New()}
	first := f.openOwned(t, storage)
	tr := f.ownedChild(t, first.root, "owner", treeOptions{slowOwner: true})
	f.open("owner.inner", durable.OutcomeCompleted)
	waitDone(t, first.h, tr.inner)
	// The owner stays live and cancelled in its slow abort handler.
	if _, err := first.h.AbortTask(bg, tr.owner); err != nil {
		t.Fatal(err)
	}
	closeSoon(t, first.h)
	o := f.openOwned(t, storage)
	// The child is empty at open, so nothing loads its edge until new work
	// arrives.
	late := f.create(t, conversation(t, o.h, tr.child), "late")
	expectEnded(t, o.h, late, durable.OutcomeAborted)
	f.open("abort.owner", durable.OutcomeCompleted)
	expectEnded(t, o.h, tr.owner, durable.OutcomeAborted)
}

func TestOwnershipCascadesFromAnOwnerTheSchedulerOrphans(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	var owner, inner durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if owner, err = CreateTask(tx, f.unregistered, holdInput{Name: "unregistered"}, conversationOwned(false)); err != nil {
			return err
		}
		child, err := tx.CreateConversation(ownedBy(owner))
		if err != nil {
			return err
		}
		inner = f.holdIn(t, tx, "below", inConversation(child.ID, false))
		return nil
	}))
	if marked, err := o.h.AbortTask(bg, owner); err != nil || marked != "marked" {
		t.Fatalf("abort = %q, %v", marked, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, owner)); got != `{"status":"orphaned","reason":"missing_task"}` {
		t.Fatalf("owner = %s", got)
	}
	expectEnded(t, o.h, inner, durable.OutcomeAborted)
}

func TestOwnershipCancelsOnlyTheCallersWaitNeverTheSharedWork(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	tr := f.ownedChild(t, o.root, "owner", treeOptions{})
	stop := errors.New("stop waiting")
	waiting, cancel := context.WithCancelCause(bg)
	waited := async(func() (struct{}, error) { return struct{}{}, o.root.WaitForIdle(waiting) })
	cancel(stop)
	if _, err := await(t, waited); !errors.Is(err, stop) {
		t.Fatalf("wait = %v", err)
	}
	expectLive(t, o.h, tr.inner)

	// Cancelling an abort after its commit leaves the marks in place.
	slow := f.ownedChild(t, o.root, "slow", treeOptions{})
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		record, err := tx.Task(slow.owner)
		if err != nil {
			return err
		}
		record.Input = []byte(`{"name":"slow","slowAbort":true}`)
		return tx.SetTask(*record)
	}))
	stopAborting := errors.New("stop aborting")
	aborting, cancelAbort := context.WithCancelCause(bg)
	abort := async(func() (struct{}, error) { return struct{}{}, o.root.Abort(aborting, false) })
	waitMarked(t, o.h, slow.owner)
	cancelAbort(stopAborting)
	if _, err := await(t, abort); !errors.Is(err, stopAborting) {
		t.Fatalf("abort = %v", err)
	}
	f.open("abort.slow", durable.OutcomeCompleted)
	for _, id := range []durable.TaskID{tr.owner, tr.inner, slow.owner, slow.inner} {
		expectEnded(t, o.h, id, durable.OutcomeAborted)
	}
}

func TestOwnershipAbortsAWaitingChildWhoseAwaitedTaskCompletesInTheCommitThatMarksItsOwner(t *testing.T) {
	f := newOwnership(t)
	o := f.openOwned(t, nil)
	var owner, dependency, blocked durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		owner = f.holdIn(t, tx, "owner", conversationOwned(false))
		child, err := tx.CreateConversation(ownedBy(owner))
		if err != nil {
			return err
		}
		dependency = f.holdIn(t, tx, "dependency", conversationOwned(false))
		blocked, err = CreateTask(tx, f.waiter, waiterInput{On: []durable.TaskID{dependency}}, inConversation(child.ID, false))
		return err
	}))
	eventually(t, func() bool { return f.ran("dependency") == 1 && f.ran("owner") == 1 })
	waitStatus(t, o.h, blocked, durable.TaskWaiting)
	// One commit completes the dependency and marks the owner.
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		completed, err := tx.Task(dependency)
		if err != nil {
			return err
		}
		marked, err := tx.Task(owner)
		if err != nil {
			return err
		}
		completedState, err := Complete(nil)
		if err != nil {
			return err
		}
		completed.State = *completedState
		marked.AbortRequested = true
		if err := tx.SetTask(*completed); err != nil {
			return err
		}
		return tx.SetTask(*marked)
	}))
	expectEnded(t, o.h, blocked, durable.OutcomeAborted)
}

// failMarks is memory storage whose next commit abort-marking target fails.
type failMarks struct {
	*memory.Storage
	mu      sync.Mutex
	target  durable.TaskID
	failure error
}

func (s *failMarks) failMark(id durable.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = id
}

func (s *failMarks) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	target := s.target
	s.mu.Unlock()
	for _, write := range writes {
		if task, ok := write.(durable.TaskWrite); ok && target != 0 && task.Value.ID == target && task.Value.AbortRequested {
			s.mu.Lock()
			s.target = 0
			s.mu.Unlock()
			return 0, s.failure
		}
	}
	return s.Storage.Commit(ctx, writes)
}

func (*failMarks) Close(context.Context) error { return nil }

func TestOwnershipAppliesMarksFoundThroughAnEdgeLoadedAfterReopenOnceAFailedCascadeCommitIsReopened(t *testing.T) {
	f := newOwnership(t)
	records := memory.New()
	storage := &survivingStorage{records}
	first := f.openOwned(t, storage)
	tr := f.ownedChild(t, first.root, "owner", treeOptions{slowOwner: true})
	f.open("owner.inner", durable.OutcomeCompleted)
	waitDone(t, first.h, tr.inner)
	if _, err := first.h.AbortTask(bg, tr.owner); err != nil {
		t.Fatal(err)
	}
	closeSoon(t, first.h)

	failing := &failMarks{Storage: records, failure: errors.New("disk gone")}
	second := f.openOwned(t, failing)
	var late durable.TaskID
	must(t, conversation(t, second.h, tr.child).Commit(bg, func(tx *durable.Tx) error {
		late = f.holdIn(t, tx, "late", conversationOwned(false))
		failing.failMark(late)
		return nil
	}))
	// The failed cascade commit fails the Harness. Closed settles once its
	// invocations have ended, and the owner's abort handler ignores its
	// signal: let it return.
	f.open("abort.owner", durable.OutcomeCompleted)
	expectEnd(t, second.h, "failed", failing.failure)
	must(t, second.h.Close(bg))
	// Reopening derives the mark again.
	o := f.openOwned(t, storage)
	expectEnded(t, o.h, late, durable.OutcomeAborted)
}

func TestOwnershipFailsTheHarnessOnAFailedCascadeCommitAndReopeningAppliesTheCascade(t *testing.T) {
	f := newOwnership(t)
	failure := errors.New("disk gone")
	records := memory.New()
	storage := &failMarks{Storage: records, failure: failure}
	o := f.openOwned(t, storage)
	tr := f.ownedChild(t, o.root, "owner", treeOptions{})
	storage.failMark(tr.inner)
	f.open("owner", durable.OutcomeFailed)
	expectEnd(t, o.h, "failed", failure)
	if reports := o.reports.all(); len(reports) != 1 || !errors.Is(reports[0], failure) {
		t.Fatalf("reports = %v", reports)
	}
	must(t, o.h.Close(bg))
	reopened := f.openOwned(t, &survivingStorage{records})
	expectEnded(t, reopened.h, tr.inner, durable.OutcomeAborted)
}
