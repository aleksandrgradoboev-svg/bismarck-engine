package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi harness-scheduler-retention.test.ts: scheduler memory is bounded by
// live work. An ended task or a conversation edge is kept only while live
// work is below it, and a chain dropped that way is loaded again from storage
// when work appears below it. The cases with queued inputs come with
// submissions.

type spawnerInput struct {
	Name string                 `json:"name"`
	Into durable.ConversationID `json:"into"`
}

// retention adds to the ownership tasks a spawner, which ends creating a
// held task "inside.<name>" in conversation Into, and a task whose terminal
// commit the Transaction rejects.
type retention struct {
	*ownership
	spawner, rejected Task
	mu                sync.Mutex
	rejections        []error
}

func newRetention(t *testing.T) *retention {
	t.Helper()
	r := &retention{ownership: newOwnership(t)}
	wait := func(ctx context.Context, name string) error {
		select {
		case <-r.gate(name).ch:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	abort := func(ctx context.Context, _ *Running[spawnerInput, holdState], rt *TaskRuntime) error {
		return rt.Commit(ctx, abortWith(""))
	}
	r.spawner = MustDefineTask(TaskSpec[spawnerInput, holdState, any]{
		Name:    "test.retention.spawner",
		Version: 1,
		Initial: func(spawnerInput) holdState { return holdState{Phase: "hold"} },
		Phases: map[string]func(context.Context, *Running[spawnerInput, holdState], *TaskRuntime) error{
			"hold": func(ctx context.Context, task *Running[spawnerInput, holdState], rt *TaskRuntime) error {
				if err := wait(ctx, task.Input.Name); err != nil {
					return err
				}
				return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
					if _, err := CreateTask(tx, r.hold, holdInput{Name: "inside." + task.Input.Name}, inConversation(task.Input.Into, false)); err != nil {
						return nil, err
					}
					return Complete(nil)
				})
			},
		},
		Abort: abort,
	})
	// One terminal commit creates a task in Into and a child of its own,
	// which the Transaction rejects as the task is ending; it catches the
	// rejection and holds.
	r.rejected = MustDefineTask(TaskSpec[spawnerInput, holdState, any]{
		Name:    "test.retention.rejected",
		Version: 1,
		Initial: func(spawnerInput) holdState { return holdState{Phase: "hold"} },
		Phases: map[string]func(context.Context, *Running[spawnerInput, holdState], *TaskRuntime) error{
			"hold": func(ctx context.Context, task *Running[spawnerInput, holdState], rt *TaskRuntime) error {
				if err := wait(ctx, "rejected"); err != nil {
					return err
				}
				if err := rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					if _, err := CreateTask(tx, r.hold, holdInput{Name: "staged"}, inConversation(task.Input.Into, false)); err != nil {
						return nil, err
					}
					if _, err := CreateTask(tx, r.hold, holdInput{Name: "child"}, taskOwned(current.ID)); err != nil {
						return nil, err
					}
					return Complete(nil)
				}); err != nil {
					r.mu.Lock()
					r.rejections = append(r.rejections, err)
					r.mu.Unlock()
				}
				return signalled(ctx)
			},
		},
		Abort: abort,
	})
	return r
}

func (r *retention) rejectionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rejections)
}

// openHarness opens a resumed Harness over storage (fresh memory when nil)
// with the retention tasks.
func (r *retention) openHarness(t *testing.T, storage durable.Storage) opened {
	t.Helper()
	o := openRoot(t, []Task{r.hold, r.waiter, r.spawner, r.rejected}, openOptions{storage: storage})
	must(t, o.h.Resume())
	return o
}

func (r *retention) done(name string) { r.open(name, durable.OutcomeCompleted) }

// cycle creates under live parent a tool task T owning conversation C with a
// task in it; all of them end. It returns C.
func (r *retention) cycle(t *testing.T, o opened, parent durable.TaskID, n int) durable.ConversationID {
	t.Helper()
	var tool, inner durable.TaskID
	var child durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		tool = r.holdIn(t, tx, fmt.Sprintf("tool%d", n), taskOwned(parent))
		conversation, err := tx.CreateConversation(ownedBy(tool))
		if err != nil {
			return err
		}
		child = conversation.ID
		inner = r.holdIn(t, tx, fmt.Sprintf("inner%d", n), inConversation(child, false))
		return nil
	}))
	r.done(fmt.Sprintf("inner%d", n))
	r.done(fmt.Sprintf("tool%d", n))
	waitDone(t, o.h, inner)
	waitDone(t, o.h, tool)
	return child
}

// chain is live P owning B, which owns A, which owns the empty conversation
// C; B and A end, so C is forgotten. P is created by create.
type chain struct {
	p, b, a durable.TaskID
	c       durable.ConversationID
}

func (r *retention) chain(t *testing.T, o opened, create func(tx *durable.Tx) durable.TaskID) chain {
	t.Helper()
	var tr chain
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		tr.p = create(tx)
		tr.b = r.holdIn(t, tx, "b", taskOwned(tr.p))
		tr.a = r.holdIn(t, tx, "a", taskOwned(tr.b))
		conversation, err := tx.CreateConversation(ownedBy(tr.a))
		tr.c = conversation.ID
		return err
	}))
	return tr
}

// retarget replaces the input of a task the commit created, for a spawner
// pointed at a conversation created after it. pi rewrites the input in a
// commit of its own, which in Go would race the scheduler: a run step taken
// before it hands the phase the first input.
func retarget(tx *durable.Tx, id durable.TaskID, input spawnerInput) error {
	for _, record := range tx.StagedTasks() {
		if record.ID != id {
			continue
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		record.Input = encoded
		return tx.SetTask(record)
	}
	return fmt.Errorf("Task %d is not staged", id)
}

func TestRetentionForgetsEndedSubagentChainsOnceTheirWorkEnds(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, nil)
	parent := r.create(t, o.root, "parent")
	r.cycle(t, o, parent, 0)
	baseline := quietSizes(t, o.h)
	for n := 1; n <= 200; n++ {
		r.cycle(t, o, parent, n)
	}
	if got := quietSizes(t, o.h); got != baseline {
		t.Fatalf("sizes = %+v, want %+v", got, baseline)
	}
	r.done("parent")
	waitDone(t, o.h, parent)
}

func TestRetentionHoldsALiveOwnerForWorkLaterCreatedInAForgottenConversationLoadingItsChainAgain(t *testing.T) {
	r := newRetention(t)
	storage := &spyStorage{Storage: openJSONL(t, t.TempDir())}
	o := r.openHarness(t, storage)
	tr := r.chain(t, o, func(tx *durable.Tx) durable.TaskID { return r.holdIn(t, tx, "p", conversationOwned(false)) })
	r.done("a")
	r.done("b")
	waitDone(t, o.h, tr.b)
	before := quietSizes(t, o.h)
	loaded := storage.reads()
	d := r.create(t, conversation(t, o.h, tr.c), "d")
	r.done("p")
	waitStatus(t, o.h, tr.p, durable.TaskCompleting)
	if storage.reads() <= loaded {
		t.Fatal("the forgotten chain was not loaded again")
	}
	r.done("d")
	expectEnded(t, o.h, d, durable.OutcomeCompleted)
	expectEnded(t, o.h, tr.p, durable.OutcomeCompleted)
	if got := quietSizes(t, o.h); got.Settled > before.Settled {
		t.Fatalf("settled = %d, before %d", got.Settled, before.Settled)
	}
}

func TestRetentionHoldsAnOwnerForWorkItsOwnTerminalCommitCreatesBelowAForgottenChain(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, nil)
	// Live P (a spawner) -> ended B -> ended A -> conversation C, empty:
	// forgotten. P's terminal commit creates D in C.
	var tr chain
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if tr.p, err = CreateTask(tx, r.spawner, spawnerInput{Name: "p"}, conversationOwned(false)); err != nil {
			return err
		}
		tr.b = r.holdIn(t, tx, "b", taskOwned(tr.p))
		tr.a = r.holdIn(t, tx, "a", taskOwned(tr.b))
		conversation, err := tx.CreateConversation(ownedBy(tr.a))
		tr.c = conversation.ID
		if err != nil {
			return err
		}
		// Point the spawner at C, now that its ID is known.
		return retarget(tx, tr.p, spawnerInput{Name: "p", Into: tr.c})
	}))
	r.done("a")
	r.done("b")
	waitDone(t, o.h, tr.b)
	r.done("p")
	eventually(t, func() bool { return statusOf(t, o.h, tr.p) != durable.TaskRunning })
	if status := statusOf(t, o.h, tr.p); status != durable.TaskCompleting {
		t.Fatalf("p = %s", status)
	}
	r.done("inside.p")
	expectEnded(t, o.h, tr.p, durable.OutcomeCompleted)
}

func TestRetentionAbortsWorkCreatedInAForgottenConversationBelowAnAbortMarkedOwnerBeforeItRuns(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, nil)
	var p, b, keep durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		p = r.holdIn(t, tx, "p", conversationOwned(false))
		b = r.holdIn(t, tx, "b", taskOwned(p))
		conversation, err := tx.CreateConversation(ownedBy(b))
		if err != nil {
			return err
		}
		c = conversation.ID
		// A slow abort keeps P holding until the test lets keep end: in Go
		// its abort could otherwise end, and P with it, before late exists.
		keep, err = CreateTask(tx, r.hold, holdInput{Name: "keep", SlowAbort: true}, taskOwned(p))
		return err
	}))
	r.done("b")
	waitDone(t, o.h, b)
	// P cancels; its live child keeps it holding.
	rewrite(t, o.h, p, func(record *durable.TaskRecord) { record.AbortRequested = true })
	waitMarked(t, o.h, keep)
	if status := statusOf(t, o.h, p); status == durable.TaskTerminal {
		t.Fatal("P ended before late was created")
	}
	late := r.create(t, conversation(t, o.h, c), "late")
	expectEnded(t, o.h, late, durable.OutcomeAborted)
	if runs := r.ran("late"); runs != 0 {
		t.Fatalf("late ran %d times", runs)
	}
	r.done("abort.keep")
	expectEnded(t, o.h, p, durable.OutcomeAborted)
}

func TestRetentionKeepsAScopeBusyWhileTheChainOfNewWorkBelowAForgottenConversationLoads(t *testing.T) {
	r := newRetention(t)
	storage := &spyStorage{Storage: memory.New()}
	o := r.openHarness(t, storage)
	var b durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		b = r.holdIn(t, tx, "b", conversationOwned(false))
		conversation, err := tx.CreateConversation(ownedBy(b))
		c = conversation.ID
		return err
	}))
	r.done("b")
	waitDone(t, o.h, b)
	schedulerQuiet(t, o.h)
	handle := conversation(t, o.h, c)
	// Hold the chain load's read of C: the second read once holding, after
	// the creating commit's own.
	held, reached := make(chan struct{}), newClosedOnce()
	var mu sync.Mutex
	holding, readsOfC := false, 0
	storage.setConversation(func(id durable.ConversationID) {
		mu.Lock()
		hold := false
		if holding && id == c {
			readsOfC++
			hold = readsOfC == 2
		}
		mu.Unlock()
		if hold {
			reached.close()
			<-held
		}
	})
	mu.Lock()
	holding = true
	mu.Unlock()
	r.create(t, handle, "d")
	select {
	case <-reached.ch:
	case <-time.After(5 * time.Second):
		mu.Lock()
		reads := readsOfC
		mu.Unlock()
		t.Fatalf("no second read of C after the creating commit (%d reads)", reads)
	}
	idling := idle(t, o.h.WaitForIdle)
	if settledResult(idling) {
		t.Fatal("idle while the chain of new work loads")
	}
	close(held)
	r.done("d")
	if _, err := await(t, idling); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionKeepsAChainWhenOneCommitEndsItsLastTaskAndCreatesNewWorkBelowIt(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, nil)
	// Live P -> ended B -> conversation C with background task old, which
	// keeps the chain known without holding B.
	var p, b, old durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		p = r.holdIn(t, tx, "p", conversationOwned(false))
		b = r.holdIn(t, tx, "b", taskOwned(p))
		conversation, err := tx.CreateConversation(ownedBy(b))
		if err != nil {
			return err
		}
		c = conversation.ID
		old = r.holdIn(t, tx, "old", inConversation(c, true))
		return nil
	}))
	r.done("b")
	waitDone(t, o.h, b)
	// One commit: the old task ends, which leaves nothing live below B, and a
	// replacement starts in C.
	var replacement durable.TaskID
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		record, err := tx.Task(old)
		if err != nil {
			return err
		}
		replacement = r.holdIn(t, tx, "new", inConversation(c, false))
		ended := record.Clone()
		state, err := Complete(nil)
		if err != nil {
			return err
		}
		ended.State = *state
		return tx.SetTask(ended)
	}))
	// The chain stayed known through the publication, so P holds for the
	// replacement.
	r.done("p")
	waitStatus(t, o.h, p, durable.TaskCompleting)
	r.done("new")
	waitDone(t, o.h, replacement)
	expectEnded(t, o.h, p, durable.OutcomeCompleted)
}

func TestRetentionDropsWhatARejectedCommitsChainLoadReadWithoutALaterPublication(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, nil)
	// Live P owns ended B, which owns conversation C: forgotten. P's terminal
	// commit stages work in C, which loads C's chain, and a child of P,
	// which the Transaction rejects, as P is ending.
	var p, b durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if p, err = CreateTask(tx, r.rejected, spawnerInput{}, conversationOwned(false)); err != nil {
			return err
		}
		b = r.holdIn(t, tx, "b", taskOwned(p))
		conversation, err := tx.CreateConversation(ownedBy(b))
		c = conversation.ID
		if err != nil {
			return err
		}
		return retarget(tx, p, spawnerInput{Into: c})
	}))
	r.done("b")
	waitDone(t, o.h, b)
	baseline := quietSizes(t, o.h)
	r.done("rejected")
	eventually(t, func() bool { return r.rejectionCount() > 0 })
	// The rejected commit published nothing; the sweep job still cleans up
	// what its chain load read.
	if got := quietSizes(t, o.h); got != baseline {
		t.Fatalf("sizes = %+v, want %+v", got, baseline)
	}
}

func TestRetentionDropsWhatAReadLoadedAfterReopenWithoutALaterPublication(t *testing.T) {
	r := newRetention(t)
	directory := t.TempDir()
	o := r.openHarness(t, openJSONL(t, directory))
	// A live task D in conversation C, owned by ended B: after reopen D's
	// chain is not loaded.
	var b, d durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		b = r.holdIn(t, tx, "b", conversationOwned(false))
		conversation, err := tx.CreateConversation(ownedBy(b))
		if err != nil {
			return err
		}
		d = r.holdIn(t, tx, "d", inConversation(conversation.ID, false))
		return nil
	}))
	r.done("b")
	waitStatus(t, o.h, b, durable.TaskCompleting)
	must(t, o.h.Close(bg))

	o = r.openHarness(t, openJSONL(t, directory))
	if _, err := o.h.Inspect(bg); err != nil {
		t.Fatal(err)
	}
	// D is still live, so its chain stays; once D ends, everything below the
	// root goes, published or not.
	if got := sizesOf(o.h).Unloaded; got != 0 {
		t.Fatalf("unloaded = %d", got)
	}
	r.done("d")
	waitDone(t, o.h, d)
	waitDone(t, o.h, b)
	if got := quietSizes(t, o.h); got.Settled != 0 || got.Below != 0 || got.Unloaded != 0 {
		t.Fatalf("sizes = %+v", got)
	}
	must(t, o.h.Close(bg))
}

func TestRetentionForgetsA20000DeepChainOfEndedTasksWithoutRecursion(t *testing.T) {
	if raceDetector {
		t.Skip("depth, not concurrency; the race detector stretches it to minutes")
	}
	r := newRetention(t)
	o := r.openHarness(t, nil)
	const depth = 20_000
	baseline := quietSizes(t, o.h)
	var links []durable.TaskID
	var leaf durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		owner := r.holdIn(t, tx, "link0", conversationOwned(false))
		links = append(links, owner)
		for n := 1; n < depth; n++ {
			owner = r.holdIn(t, tx, fmt.Sprintf("link%d", n), taskOwned(owner))
			links = append(links, owner)
		}
		leaf = r.holdIn(t, tx, "leaf", taskOwned(owner))
		return nil
	}))
	for n := 0; n < depth; n++ {
		r.done(fmt.Sprintf("link%d", n))
	}
	r.done("leaf")
	waitDone(t, o.h, leaf)
	waitDone(t, o.h, links[0])
	must(t, o.h.WaitForIdle(bg))
	if got := quietSizes(t, o.h); got != baseline {
		t.Fatalf("sizes = %+v, want %+v", got, baseline)
	}
}

func TestRetentionKeepsNoMoreAfterAllWorkEndedThanAFreshReopen(t *testing.T) {
	r := newRetention(t)
	directory := t.TempDir()
	o := r.openHarness(t, openJSONL(t, directory))
	parent := r.create(t, o.root, "parent")
	for n := 0; n < 20; n++ {
		r.cycle(t, o, parent, n)
	}
	r.done("parent")
	waitDone(t, o.h, parent)
	must(t, o.h.WaitForIdle(bg))
	ended := quietSizes(t, o.h)
	must(t, o.h.Close(bg))
	o = r.openHarness(t, openJSONL(t, directory))
	must(t, o.h.WaitForIdle(bg))
	if fresh := quietSizes(t, o.h); ended != fresh {
		t.Fatalf("after all work ended %+v, after a fresh reopen %+v", ended, fresh)
	}
	must(t, o.h.Close(bg))
}

// A wait on an ended task closes no cycle, also when the ended task owns the
// waiter's conversation and the scheduler forgot it: the wait resumes at once.
func TestRetentionLetsATaskWaitOnAForgottenEndedOwner(t *testing.T) {
	r := newRetention(t)
	o := r.openHarness(t, openJSONL(t, t.TempDir()))
	var b durable.TaskID
	var c durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		b = r.holdIn(t, tx, "b", conversationOwned(false))
		conversation, err := tx.CreateConversation(ownedBy(b))
		c = conversation.ID
		return err
	}))
	r.done("b")
	waitDone(t, o.h, b)
	var waiter durable.TaskID
	must(t, conversation(t, o.h, c).Commit(bg, func(tx *durable.Tx) error {
		var err error
		waiter, err = CreateTask(tx, r.waiter, waiterInput{On: []durable.TaskID{b}}, conversationOwned(false))
		return err
	}))
	expectEnded(t, o.h, waiter, durable.OutcomeCompleted)
	must(t, o.h.Close(bg))
}
