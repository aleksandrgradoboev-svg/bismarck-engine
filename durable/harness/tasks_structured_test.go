package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi harness-structured.test.ts, the cases without a chat: ownership,
// waiting, completing, abort order, boundaries, recovery, definitions and
// waits, conversation abort, recovery through ownership edges. Its tool
// rounds come with the tools stage.

type nodeInput struct {
	Name string `json:"name"`
}

type nodeState struct {
	Phase string `json:"phase"`
	Round int    `json:"round,omitempty"`
}

type nodeTask = Running[nodeInput, nodeState]

// behavior is the per-name behavior of the node task; unscripted parts use
// the defaults.
type behavior struct {
	run    func(ctx context.Context, rt *TaskRuntime) error
	resume func(ctx context.Context, rt *TaskRuntime, round int) error
	abort  func(ctx context.Context, rt *TaskRuntime) error
}

// nodes holds the scriptable node task, its named gates and its log of
// handler starts, such as "run:a" and "abort:a".
type nodes struct {
	mu        sync.Mutex
	behaviors map[string]behavior
	gates     map[string]*ending
	log       []string

	node Task
	// unregistered is never registered: aborting it can only orphan it.
	unregistered Task
}

func newNodes(t *testing.T) *nodes {
	t.Helper()
	n := &nodes{behaviors: map[string]behavior{}, gates: map[string]*ending{}}
	n.node = MustDefineTask(TaskSpec[nodeInput, nodeState, any]{
		Name:    "test.node",
		Version: 1,
		Initial: func(nodeInput) nodeState { return nodeState{Phase: "run"} },
		Phases: map[string]func(context.Context, *nodeTask, *TaskRuntime) error{
			"run": func(ctx context.Context, task *nodeTask, rt *TaskRuntime) error {
				name := task.Input.Name
				n.add("run:" + name)
				if b := n.behavior(name); b.run != nil {
					return b.run(ctx, rt)
				}
				return n.defaultRun(ctx, rt, name)
			},
			"resume": func(ctx context.Context, task *nodeTask, rt *TaskRuntime) error {
				name := task.Input.Name
				n.add("resume:" + name)
				if b := n.behavior(name); b.resume != nil {
					return b.resume(ctx, rt, task.Checkpoint.Round)
				}
				return rt.Commit(ctx, endWith("completed", name))
			},
		},
		Abort: func(ctx context.Context, task *nodeTask, rt *TaskRuntime) error {
			name := task.Input.Name
			n.add("abort:" + name)
			if b := n.behavior(name); b.abort != nil {
				return b.abort(ctx, rt)
			}
			return rt.Commit(ctx, abortWith(""))
		},
	})
	n.unregistered = MustDefineTask(TaskSpec[nodeInput, holdState, any]{
		Name:    "test.unregistered",
		Version: 1,
		Initial: func(nodeInput) holdState { return holdState{Phase: "run"} },
		Phases: map[string]func(context.Context, *Running[nodeInput, holdState], *TaskRuntime) error{
			"run": func(context.Context, *Running[nodeInput, holdState], *TaskRuntime) error { return nil },
		},
		Abort: func(context.Context, *Running[nodeInput, holdState], *TaskRuntime) error { return nil },
	})
	return n
}

func (n *nodes) add(line string) {
	n.mu.Lock()
	n.log = append(n.log, line)
	n.mu.Unlock()
}

func (n *nodes) lines() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.log...)
}

func (n *nodes) logged(line string) bool {
	for _, l := range n.lines() {
		if l == line {
			return true
		}
	}
	return false
}

// prefixed returns the log lines starting with prefix, in order.
func (n *nodes) prefixed(prefix string) []string {
	var out []string
	for _, l := range n.lines() {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func (n *nodes) script(name string, b behavior) {
	n.mu.Lock()
	n.behaviors[name] = b
	n.mu.Unlock()
}

func (n *nodes) behavior(name string) behavior {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.behaviors[name]
}

func (n *nodes) gate(name string) *ending {
	n.mu.Lock()
	defer n.mu.Unlock()
	g := n.gates[name]
	if g == nil {
		g = &ending{ch: make(chan struct{})}
		n.gates[name] = g
	}
	return g
}

// open lets the task held at name end: "completed", "failed", or "throw",
// which faults it.
func (n *nodes) open(name, value string) {
	g := n.gate(name)
	g.once.Do(func() {
		g.value = value
		close(g.ch)
	})
}

// waitGate waits for the gate or the signal; an abort handler blocked on it
// ends with the signal, as pi's never-settling handler ends at close.
func (n *nodes) waitGate(ctx context.Context, name string) (string, error) {
	g := n.gate(name)
	select {
	case <-g.ch:
		return g.value, nil
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

// defaultRun waits for the gate or the abort signal, then ends as the gate
// says.
func (n *nodes) defaultRun(ctx context.Context, rt *TaskRuntime, name string) error {
	value, err := n.waitGate(ctx, name)
	if err != nil {
		return err
	}
	if value == "throw" {
		return fmt.Errorf("%s threw", name)
	}
	return rt.Commit(ctx, endWith(value, name))
}

// slowAbort scripts name's abort handler to wait for the gate
// "abort.<name>".
func (n *nodes) slowAbort(name string) {
	n.script(name, behavior{abort: func(ctx context.Context, rt *TaskRuntime) error {
		if _, err := n.waitGate(ctx, "abort."+name); err != nil {
			return err
		}
		return rt.Commit(ctx, abortWith(""))
	}})
}

// endWith ends with result name, or fails with "<name> failed".
func endWith(value, name string) func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
		if value == "failed" {
			return Fail(name + " failed"), nil
		}
		return Complete(name)
	}
}

// waitOn waits on on, resuming in round.
func waitOn(on []durable.TaskID, policy durable.JoinPolicy, round int) func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
		return Wait(on, policy, nodeState{Phase: "resume", Round: round})
	}
}

// toResume moves on to the resume phase in round 1.
func toResume(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return Next(nodeState{Phase: "resume", Round: 1})
}

// spawn creates a child node named name, owned by owner.
func (n *nodes) spawn(tx *durable.Tx, owner durable.TaskID, name string) (durable.TaskID, error) {
	return CreateTask(tx, n.node, nodeInput{Name: name}, taskOwned(owner))
}

// start creates a node named name in conversation c, owned by it unless
// options say otherwise.
func (n *nodes) start(t *testing.T, c *Conversation, name string, options ...durable.TaskOptions) durable.TaskID {
	t.Helper()
	option := conversationOwned(false)
	if len(options) > 0 {
		option = options[0]
	}
	var id durable.TaskID
	must(t, c.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, n.node, nodeInput{Name: name}, option)
		return err
	}))
	return id
}

// openNodes opens a resumed Harness over storage (fresh memory when nil)
// with the node task.
func (n *nodes) openNodes(t *testing.T, storage durable.Storage) opened {
	t.Helper()
	o := openRoot(t, []Task{n.node}, openOptions{storage: storage})
	must(t, o.h.Resume())
	return o
}

// spawnAndFinish scripts name to spawn child and then complete, holding
// while the child lives; it returns the child's ID once spawned.
func (n *nodes) spawnAndFinish(name, child string) func() durable.TaskID {
	var mu sync.Mutex
	var id durable.TaskID
	n.script(name, behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			spawned, err := n.spawn(tx, rt.TaskID, child)
			mu.Lock()
			id = spawned
			mu.Unlock()
			if err != nil {
				return nil, err
			}
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	return func() durable.TaskID {
		mu.Lock()
		defer mu.Unlock()
		return id
	}
}

// family is a parent's spawned children and the outcomes it read.
type family struct {
	mu       sync.Mutex
	ids      []durable.TaskID
	outcomes []string
}

func (f *family) children() []durable.TaskID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]durable.TaskID(nil), f.ids...)
}

func (f *family) read() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.outcomes...)
}

// parentOf scripts parent to spawn children in one commit and wait on them
// with policy; it records their outcomes.
func (n *nodes) parentOf(parent string, children []string, policy durable.JoinPolicy) *family {
	f := &family{}
	n.script(parent, behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
				var ids []durable.TaskID
				for _, name := range children {
					id, err := n.spawn(tx, rt.TaskID, name)
					if err != nil {
						return nil, err
					}
					ids = append(ids, id)
				}
				f.mu.Lock()
				f.ids = ids
				f.mu.Unlock()
				return waitOn(ids, policy, 1)(tx, current)
			})
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			outcomes, err := rt.Outcomes(ctx, f.children())
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.outcomes = names(outcomes, func(o durable.TaskOutcome) string { return o.Status })
			f.mu.Unlock()
			return rt.Commit(ctx, endWith("completed", parent))
		},
	})
	return f
}

func stateOf(t *testing.T, h *Harness, id durable.TaskID) durable.TaskState {
	t.Helper()
	return getTask(t, h, id).State
}

func outcomeOf(t *testing.T, h *Harness, id durable.TaskID) string {
	t.Helper()
	return waitDone(t, h, id).State.Outcome.Status
}

func faultedWith(t *testing.T, h *Harness, id durable.TaskID, message string) {
	t.Helper()
	outcome := waitDone(t, h, id).State.Outcome
	if outcome.Status != durable.OutcomeFaulted || outcome.Error == nil || !strings.Contains(outcome.Error.Message, message) {
		t.Fatalf("task %d outcome = %s, want faulted with %q", id, jsonOf(t, outcome), message)
	}
}

func inspected(t *testing.T, h *Harness, id durable.TaskID) *TaskInspection {
	t.Helper()
	inspection, err := h.Inspect(bg)
	must(t, err)
	for i := range inspection.Tasks {
		if inspection.Tasks[i].Record.ID == id {
			return &inspection.Tasks[i]
		}
	}
	return nil
}

func taskNotes() *durable.Doc {
	return durable.MustDefineDoc(durable.DocDef{
		Kind:    "test.task-notes",
		Version: 1,
		Scope:   durable.ScopeTask,
		Initial: func(any) map[string]any { return map[string]any{"text": ""} },
	})
}

// ─── Ownership ─────────────────────────────────────────────────────────────

func TestStructuredCreatesAChildInItsOwnersConversationWithItsOwnerRecorded(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	var mu sync.Mutex
	var child durable.TaskID
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			id, err := n.spawn(tx, rt.TaskID, "child")
			if err != nil {
				return nil, err
			}
			mu.Lock()
			child = id
			mu.Unlock()
			return waitOn([]durable.TaskID{id}, durable.JoinAllSettled, 1)(tx, current)
		})
	}})
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return child != 0 })
	record := getTask(t, o.h, child)
	if record.Owner == nil || *record.Owner != parent || record.ConversationID != o.root.ID || record.Background {
		t.Fatalf("child = %s", jsonOf(t, record))
	}
	if owner := getTask(t, o.h, parent).Owner; owner != nil {
		t.Fatalf("parent owner = %d", *owner)
	}
	n.open("child", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
}

func TestStructuredRejectsNoOwnershipAMissingOwnerAChildInAnotherConversationAndABackgroundChild(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	parent := n.start(t, o.root, "parent")
	other, err := o.h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	create := func(options durable.TaskOptions) error {
		return o.root.Commit(bg, func(tx *durable.Tx) error {
			_, err := CreateTask(tx, n.node, nodeInput{Name: "x"}, options)
			return err
		})
	}
	// pi rejects options without ownership only because reading the kind of
	// an undefined ownership throws; an ownership of another kind it takes as
	// conversation-owned, as Go takes the zero TaskOwnership.
	expectError(t, create(taskOwned(999_999)), "does not exist")
	inOther := taskOwned(parent)
	inOther.ConversationID = other.ID
	expectError(t, create(inOther), "owner's conversation")
	background := taskOwned(parent)
	background.Background = true
	expectError(t, create(background), "cannot be background")
	// The owner's conversation is the default, even from a commit bound to
	// another conversation.
	var child durable.TaskID
	must(t, other.Commit(bg, func(tx *durable.Tx) error {
		var err error
		child, err = n.spawn(tx, parent, "child")
		return err
	}))
	if got := getTask(t, o.h, child).ConversationID; got != o.root.ID {
		t.Fatalf("child conversation = %d, want %d", got, o.root.ID)
	}
	n.open("parent", "completed")
	n.open("child", "completed")
	waitDone(t, o.h, parent)
}

func TestStructuredRejectsNewOwnedWorkBelowAnOwnerThatIsCompletingTerminalOrAbortMarked(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			if _, err := n.spawn(tx, rt.TaskID, "child"); err != nil {
				return nil, err
			}
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	createChild := func(owner durable.TaskID) error {
		return o.root.Commit(bg, func(tx *durable.Tx) error {
			_, err := n.spawn(tx, owner, "late")
			return err
		})
	}
	createConversation := func() error {
		return o.root.Commit(bg, func(tx *durable.Tx) error {
			_, err := tx.CreateConversation(ownedBy(parent))
			return err
		})
	}
	expectError(t, createChild(parent), "is completing")
	expectError(t, createConversation(), "is completing")
	n.open("child", "completed")
	waitDone(t, o.h, parent)
	expectError(t, createChild(parent), "is terminal")
	expectError(t, createConversation(), "is terminal")

	n.slowAbort("slow")
	slow := n.start(t, o.root, "slow")
	eventually(t, func() bool { return n.logged("run:slow") })
	if result, err := o.h.AbortTask(bg, slow); err != nil || result != "marked" {
		t.Fatalf("abort slow = %s, %v", result, err)
	}
	expectError(t, createChild(slow), "is abort-marked")
	n.open("abort.slow", "completed")
	if got := outcomeOf(t, o.h, slow); got != durable.OutcomeAborted {
		t.Fatalf("slow = %s", got)
	}
}

func TestStructuredCannotCreateAChildInItsFinishingCommitButWorkItStartsInAnOwnedConversationHoldsIt(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.script("eager", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			if _, err := n.spawn(tx, rt.TaskID, "never"); err != nil {
				return nil, err
			}
			return endWith("completed", "eager")(tx, current)
		})
	}})
	eager := n.start(t, o.root, "eager")
	faultedWith(t, o.h, eager, "is completing")
	// The rejected commit wrote nothing.
	var kinds []string
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		page, err := tx.ScanTasks(durable.TaskQuery{Kind: "test.node"}, 20, nil)
		if err != nil {
			return err
		}
		for _, task := range page.Items {
			var input nodeInput
			if err := json.Unmarshal(task.Input, &input); err != nil {
				return err
			}
			kinds = append(kinds, input.Name)
		}
		return nil
	}))
	if !reflect.DeepEqual(kinds, []string{"eager"}) {
		t.Fatalf("nodes = %v", kinds)
	}

	var mu sync.Mutex
	var child durable.ConversationID
	n.script("host", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				conversation, err := tx.CreateConversation(ownedBy(rt.TaskID))
				if err != nil {
					return nil, err
				}
				mu.Lock()
				child = conversation.ID
				mu.Unlock()
				return toResume(tx, durable.TaskRecord{})
			})
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
				mu.Lock()
				in := child
				mu.Unlock()
				if _, err := CreateTask(tx, n.node, nodeInput{Name: "inner"}, inConversation(in, false)); err != nil {
					return nil, err
				}
				return endWith("completed", "host")(tx, current)
			})
		},
	})
	host := n.start(t, o.root, "host")
	waitStatus(t, o.h, host, durable.TaskCompleting)
	if settledResult(async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, host) })) {
		t.Fatal("the host settled while its work lives")
	}
	n.open("inner", "completed")
	if got := outcomeOf(t, o.h, host); got != durable.OutcomeCompleted {
		t.Fatalf("host = %s", got)
	}
}

// ─── Waiting ───────────────────────────────────────────────────────────────

func TestStructuredResumesOnceEveryAwaitedTaskIsTerminalAndReadsTheirOutcomesInOrder(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	f := &family{}
	n.script("parent", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
				var ids []durable.TaskID
				for _, name := range []string{"ok", "fails", "throws", "aborted"} {
					id, err := n.spawn(tx, rt.TaskID, name)
					if err != nil {
						return nil, err
					}
					ids = append(ids, id)
				}
				orphan, err := CreateTask(tx, n.unregistered, nodeInput{Name: "orphan"}, taskOwned(rt.TaskID))
				if err != nil {
					return nil, err
				}
				ids = append(ids, orphan)
				f.mu.Lock()
				f.ids = ids
				f.mu.Unlock()
				return waitOn(ids, durable.JoinAllSettled, 1)(tx, current)
			})
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			outcomes, err := rt.Outcomes(ctx, f.children())
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.outcomes = names(outcomes, func(o durable.TaskOutcome) string { return o.Status })
			f.mu.Unlock()
			return rt.Commit(ctx, endWith("completed", "parent"))
		},
	})
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { return len(f.children()) == 5 })
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	ids := f.children()
	n.open("ok", "completed")
	n.open("fails", "failed")
	n.open("throws", "throw")
	if _, err := o.h.AbortTask(bg, ids[3]); err != nil {
		t.Fatal(err)
	}
	if result, err := o.h.AbortTask(bg, ids[4]); err != nil || result != "marked" {
		t.Fatalf("abort orphan = %s, %v", result, err)
	}
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	want := []string{"completed", "failed", "faulted", "aborted", "orphaned"}
	if got := f.read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v", got)
	}
	// allSettled never marks siblings.
	if got := n.prefixed("abort:"); !reflect.DeepEqual(got, []string{"abort:aborted"}) {
		t.Fatalf("aborts = %v", got)
	}
}

func TestStructuredFailsFastWhenAChildEndsFailedOrFaulted(t *testing.T) {
	for _, ending := range []string{"failed", "throw"} {
		t.Run(ending, func(t *testing.T) {
			n := newNodes(t)
			o := n.openNodes(t, nil)
			f := n.parentOf("checkout", []string{"p1", "p2", "p3", "p4"}, durable.JoinFailFast)
			parent := n.start(t, o.root, "checkout")
			waitStatus(t, o.h, parent, durable.TaskWaiting)
			n.open("p2", ending)
			if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
				t.Fatalf("parent = %s", got)
			}
			failed := "failed"
			if ending == "throw" {
				failed = "faulted"
			}
			if got := f.read(); !reflect.DeepEqual(got, []string{"aborted", failed, "aborted", "aborted"}) {
				t.Fatalf("outcomes = %v", got)
			}
			if getTask(t, o.h, parent).AbortRequested {
				t.Fatal("the parent was marked")
			}
			aborts := n.prefixed("abort:")
			sort.Strings(aborts)
			if !reflect.DeepEqual(aborts, []string{"abort:p1", "abort:p3", "abort:p4"}) {
				t.Fatalf("aborts = %v", aborts)
			}
		})
	}
}

func TestStructuredFailsFastOnAHeldFailureBeforeTheFailingChildDrains(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	f := &family{}
	var outside durable.TaskID
	n.script("p1", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				if _, err := n.spawn(tx, rt.TaskID, "grandchild"); err != nil {
					return nil, err
				}
				return toResume(tx, durable.TaskRecord{})
			})
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			return rt.Commit(ctx, endWith("failed", "p1"))
		},
	})
	n.slowAbort("grandchild")
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			var ids []durable.TaskID
			for _, name := range []string{"p1", "p2"} {
				id, err := n.spawn(tx, rt.TaskID, name)
				if err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			id, err := n.spawn(tx, rt.TaskID, "outside")
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.ids, outside = ids, id
			f.mu.Unlock()
			return waitOn(ids, durable.JoinFailFast, 1)(tx, current)
		})
	}})
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { return len(f.children()) == 2 })
	ids := f.children()
	// p1 holds failed while its grandchild's abort handler runs; p2 is
	// already aborted.
	if got := outcomeOf(t, o.h, ids[1]); got != durable.OutcomeAborted {
		t.Fatalf("p2 = %s", got)
	}
	if state := stateOf(t, o.h, ids[0]); state.Status != durable.TaskCompleting || state.Outcome == nil || state.Outcome.Status != durable.OutcomeFailed {
		t.Fatalf("p1 = %s", jsonOf(t, state))
	}
	if status := stateOf(t, o.h, parent).Status; status != durable.TaskWaiting {
		t.Fatalf("parent = %s", status)
	}
	n.open("abort.grandchild", "completed")
	eventually(t, func() bool { return n.logged("resume:parent") })
	// Only the other tasks in on are marked: not the failed one, not the
	// parent, not a child outside on.
	if got := outcomeOf(t, o.h, ids[0]); got != durable.OutcomeFailed {
		t.Fatalf("p1 = %s", got)
	}
	f.mu.Lock()
	out := outside
	f.mu.Unlock()
	marked := []bool{getTask(t, o.h, ids[0]).AbortRequested, getTask(t, o.h, ids[1]).AbortRequested, getTask(t, o.h, out).AbortRequested}
	if !reflect.DeepEqual(marked, []bool{false, true, false}) || getTask(t, o.h, parent).AbortRequested {
		t.Fatalf("marked = %v, parent %v", marked, getTask(t, o.h, parent).AbortRequested)
	}
	if status := stateOf(t, o.h, out).Status; status != durable.TaskRunning {
		t.Fatalf("outside = %s", status)
	}
	// Finished, the parent holds for the child outside on.
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	n.open("outside", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
}

func TestStructuredWaitsWithAllSettledOnTasksItDoesNotOwnIncludingAlreadyTerminalOnes(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	done := n.start(t, o.root, "done")
	n.open("done", "completed")
	waitDone(t, o.h, done)
	live := n.start(t, o.root, "live")
	var mu sync.Mutex
	var outcomes []string
	n.script("parent", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, waitOn([]durable.TaskID{done, live}, durable.JoinAllSettled, 1))
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			read, err := rt.Outcomes(ctx, []durable.TaskID{done, live})
			if err != nil {
				return err
			}
			mu.Lock()
			outcomes = names(read, func(o durable.TaskOutcome) string { return o.Status })
			mu.Unlock()
			return rt.Commit(ctx, endWith("completed", "parent"))
		},
	})
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	// A task it does not own is not its work: the parent may finish while it
	// lives, but here it waits for it.
	n.open("live", "failed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(outcomes, []string{"completed", "failed"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestStructuredRejectsWaitsOnItselfItsOwnerAMissingTaskAndFailFastOnATaskItDoesNotOwn(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	other := n.start(t, o.root, "other")
	cases := []struct {
		name    string
		on      func(rt *TaskRuntime) []durable.TaskID
		policy  durable.JoinPolicy
		message string
	}{
		{"self", func(rt *TaskRuntime) []durable.TaskID { return []durable.TaskID{rt.TaskID} }, durable.JoinAllSettled, "is a wait cycle"},
		{"missing", func(*TaskRuntime) []durable.TaskID { return []durable.TaskID{999_999} }, durable.JoinAllSettled, "does not exist"},
		{"foreign", func(*TaskRuntime) []durable.TaskID { return []durable.TaskID{other} }, durable.JoinFailFast, "only on tasks it owns"},
	}
	for _, c := range cases {
		n.script(c.name, behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, waitOn(c.on(rt), c.policy, 1))
		}})
		faultedWith(t, o.h, n.start(t, o.root, c.name), c.message)
	}
	// A child waiting on its owner could never resume.
	var mu sync.Mutex
	var parent, child durable.TaskID
	n.script("child", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		mu.Lock()
		owner := parent
		mu.Unlock()
		return rt.Commit(ctx, waitOn([]durable.TaskID{owner}, durable.JoinAllSettled, 1))
	}})
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			id, err := n.spawn(tx, rt.TaskID, "child")
			if err != nil {
				return nil, err
			}
			mu.Lock()
			parent, child = rt.TaskID, id
			mu.Unlock()
			return waitOn([]durable.TaskID{id}, durable.JoinAllSettled, 1)(tx, current)
		})
	}})
	started := n.start(t, o.root, "parent")
	if got := outcomeOf(t, o.h, started); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	mu.Lock()
	spawned := child
	mu.Unlock()
	faultedWith(t, o.h, spawned, "is a wait cycle")
	n.open("other", "completed")
}

// ids is a goroutine-safe map of task IDs by name.
type ids struct {
	mu sync.Mutex
	m  map[string]durable.TaskID
}

func newIDs() *ids { return &ids{m: map[string]durable.TaskID{}} }

func (i *ids) set(name string, id durable.TaskID) {
	i.mu.Lock()
	i.m[name] = id
	i.mu.Unlock()
}

func (i *ids) get(name string) durable.TaskID {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.m[name]
}

// waitsOn scripts name's run to wait on the task named target.
func (n *nodes) waitsOn(name string, tasks *ids, target string) {
	n.script(name, behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, waitOn([]durable.TaskID{tasks.get(target)}, durable.JoinAllSettled, 1))
	}})
}

// createAll creates conversation-owned nodes in one commit, in order.
func (n *nodes) createAll(t *testing.T, c *Conversation, tasks *ids, names ...string) {
	t.Helper()
	must(t, c.Commit(bg, func(tx *durable.Tx) error {
		for _, name := range names {
			id, err := CreateTask(tx, n.node, nodeInput{Name: name}, conversationOwned(false))
			if err != nil {
				return err
			}
			tasks.set(name, id)
		}
		return nil
	}))
}

// outcomesOf waits for each task and returns its outcome status, sorted.
func outcomesOf(t *testing.T, h *Harness, tasks ...durable.TaskID) []string {
	t.Helper()
	var out []string
	for _, id := range tasks {
		out = append(out, outcomeOf(t, h, id))
	}
	sort.Strings(out)
	return out
}

func TestStructuredRejectsTheWaitThatClosesACycle(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	n.waitsOn("a", tasks, "b")
	n.waitsOn("b", tasks, "a")
	n.createAll(t, o.root, tasks, "a", "b")
	// a -> b and b -> a are waits, and whichever comes second closes the
	// cycle.
	if got := outcomesOf(t, o.h, tasks.get("a"), tasks.get("b")); !reflect.DeepEqual(got, []string{"completed", "faulted"}) {
		t.Fatalf("outcomes = %v", got)
	}
}

func TestStructuredRejectsTheWaitThatClosesACycleThroughAHeldOwner(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	child := n.spawnAndFinish("held", "child")
	n.waitsOn("child", tasks, "other")
	n.waitsOn("other", tasks, "held")
	n.createAll(t, o.root, tasks, "held", "other")
	// held -> child is a hold; child -> other -> held are waits, and
	// whichever wait comes second closes the cycle.
	if got := outcomeOf(t, o.h, tasks.get("held")); got != durable.OutcomeCompleted {
		t.Fatalf("held = %s", got)
	}
	if got := outcomesOf(t, o.h, child(), tasks.get("other")); !reflect.DeepEqual(got, []string{"completed", "faulted"}) {
		t.Fatalf("outcomes = %v", got)
	}
}

func TestStructuredRejectsACycleThroughHoldsTwoLevelsDeep(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	child := n.spawnAndFinish("held", "child")
	grandchild := n.spawnAndFinish("child", "grandchild")
	n.waitsOn("grandchild", tasks, "other")
	n.waitsOn("other", tasks, "held")
	n.createAll(t, o.root, tasks, "held", "other")
	// held -> child -> grandchild are holds; grandchild -> other -> held are
	// waits.
	eventually(t, func() bool { return grandchild() != 0 })
	if got := outcomesOf(t, o.h, grandchild(), tasks.get("other")); !reflect.DeepEqual(got, []string{"completed", "faulted"}) {
		t.Fatalf("outcomes = %v", got)
	}
	if got := outcomeOf(t, o.h, child()); got != durable.OutcomeCompleted {
		t.Fatalf("child = %s", got)
	}
}

func TestStructuredLetsABackgroundTaskWaitOnTheOwnerOfItsConversation(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	n.waitsOn("background", tasks, "owner")
	var background durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		owner, err := CreateTask(tx, n.node, nodeInput{Name: "owner"}, conversationOwned(false))
		if err != nil {
			return err
		}
		tasks.set("owner", owner)
		conversation, err := tx.CreateConversation(ownedBy(owner))
		if err != nil {
			return err
		}
		background, err = CreateTask(tx, n.node, nodeInput{Name: "background"}, inConversation(conversation.ID, true))
		return err
	}))
	// background -> owner is a wait but owner -> background is no hold, so
	// there is no cycle.
	waitStatus(t, o.h, background, durable.TaskWaiting)
	n.open("owner", "completed")
	if got := outcomeOf(t, o.h, background); got != durable.OutcomeCompleted {
		t.Fatalf("background = %s", got)
	}
}

func TestStructuredRejectsACycleOfAHoldAndTwoWaitsThatEachLookReasonable(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	reviewerWaits := newClosedOnce()
	// The orchestrator spawns a worker and finishes; the running worker holds
	// it as completing.
	n.script("orchestrator", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			worker, err := n.spawn(tx, rt.TaskID, "worker")
			if err != nil {
				return nil, err
			}
			tasks.set("worker", worker)
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	// The reviewer waits for the orchestrator's result.
	n.script("reviewer", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		err := rt.Commit(ctx, waitOn([]durable.TaskID{tasks.get("orchestrator")}, durable.JoinAllSettled, 1))
		reviewerWaits.close()
		return err
	}})
	// The worker waits for the reviewer, closing the cycle.
	n.script("worker", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		select {
		case <-reviewerWaits.ch:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		return rt.Commit(ctx, waitOn([]durable.TaskID{tasks.get("reviewer")}, durable.JoinAllSettled, 1))
	}})
	tasks.set("orchestrator", n.start(t, o.root, "orchestrator"))
	waitStatus(t, o.h, tasks.get("orchestrator"), durable.TaskCompleting)
	tasks.set("reviewer", n.start(t, o.root, "reviewer"))
	var got []string
	for _, name := range []string{"orchestrator", "worker", "reviewer"} {
		got = append(got, outcomeOf(t, o.h, tasks.get(name)))
	}
	if !reflect.DeepEqual(got, []string{"completed", "faulted", "completed"}) {
		t.Fatalf("outcomes = %v", got)
	}
	worker, reviewer, orchestrator := tasks.get("worker"), tasks.get("reviewer"), tasks.get("orchestrator")
	faultedWith(t, o.h, worker, fmt.Sprintf("%d -> %d -> %d -> %d is a wait cycle", worker, reviewer, orchestrator, worker))
}

func TestStructuredAllowsADiamondOfWaits(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	// A diamond: top waits on left and right, both wait on bottom. No cycle.
	n.script("top", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, waitOn([]durable.TaskID{tasks.get("left"), tasks.get("right")}, durable.JoinAllSettled, 1))
	}})
	n.waitsOn("left", tasks, "bottom")
	n.waitsOn("right", tasks, "bottom")
	n.createAll(t, o.root, tasks, "bottom", "left", "right", "top")
	n.open("bottom", "completed")
	if got := outcomeOf(t, o.h, tasks.get("top")); got != durable.OutcomeCompleted {
		t.Fatalf("top = %s", got)
	}
}

func TestStructuredFollowsNoWaitOfAnAbortMarkedTaskWhichWaitsOnlyForItsOwnedWork(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	cleanup, aWaits := make(chan struct{}), make(chan struct{})
	// b owns c and waits on a; aborting b leaves c's cleanup pending, so b
	// waits only for c.
	n.script("b", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			c, err := n.spawn(tx, rt.TaskID, "c")
			if err != nil {
				return nil, err
			}
			tasks.set("c", c)
			return waitOn([]durable.TaskID{tasks.get("a")}, durable.JoinAllSettled, 1)(tx, current)
		})
	}})
	n.script("c", behavior{abort: func(ctx context.Context, rt *TaskRuntime) error {
		select {
		case <-cleanup:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		return rt.Commit(ctx, abortWith("c"))
	}})
	n.script("a", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			select {
			case <-aWaits:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			return rt.Commit(ctx, waitOn([]durable.TaskID{tasks.get("b")}, durable.JoinAllSettled, 1))
		},
		resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
			return rt.Commit(ctx, endWith("completed", "a"))
		},
	})
	n.createAll(t, o.root, tasks, "a", "b")
	waitStatus(t, o.h, tasks.get("b"), durable.TaskWaiting)
	if result, err := o.h.AbortTask(bg, tasks.get("b")); err != nil || result != "marked" {
		t.Fatalf("abort b = %s, %v", result, err)
	}
	eventually(t, func() bool { return n.logged("abort:c") })
	// a -> b would close a -> b -> a through b's stale wait; b now waits only
	// for c.
	close(aWaits)
	waitStatus(t, o.h, tasks.get("a"), durable.TaskWaiting)
	close(cleanup)
	if got := outcomeOf(t, o.h, tasks.get("b")); got != durable.OutcomeAborted {
		t.Fatalf("b = %s", got)
	}
	if got := outcomeOf(t, o.h, tasks.get("a")); got != durable.OutcomeCompleted {
		t.Fatalf("a = %s", got)
	}
}

func TestStructuredRejectsALongerCycleOfWaits(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	tasks := newIDs()
	cycle := []string{"x", "y", "z"}
	for i, name := range cycle {
		n.waitsOn(name, tasks, cycle[(i+1)%len(cycle)])
	}
	n.createAll(t, o.root, tasks, cycle...)
	got := outcomesOf(t, o.h, tasks.get("x"), tasks.get("y"), tasks.get("z"))
	if !reflect.DeepEqual(got, []string{"completed", "completed", "faulted"}) {
		t.Fatalf("outcomes = %v", got)
	}
}

func TestStructuredResumesAtTheNextPassWhenItWaitsOnNothing(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, waitOn([]durable.TaskID{}, durable.JoinFailFast, 1))
	}})
	if got := outcomeOf(t, o.h, n.start(t, o.root, "parent")); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	if got := n.lines(); !reflect.DeepEqual(got, []string{"run:parent", "resume:parent"}) {
		t.Fatalf("log = %v", got)
	}
}

func TestStructuredKeepsRunningPhasesAfterSpawningAndWaitsOnSubsetsInSequence(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	f := &family{}
	n.script("parent", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				var spawned []durable.TaskID
				for _, name := range []string{"a", "b", "c"} {
					id, err := n.spawn(tx, rt.TaskID, name)
					if err != nil {
						return nil, err
					}
					spawned = append(spawned, id)
				}
				f.mu.Lock()
				f.ids = spawned
				f.mu.Unlock()
				return Next(nodeState{Phase: "resume", Round: 0})
			})
		},
		resume: func(ctx context.Context, rt *TaskRuntime, round int) error {
			n.add(fmt.Sprintf("round:%d", round))
			children := f.children()
			switch round {
			case 0:
				return rt.Commit(ctx, waitOn(children[:1], durable.JoinAllSettled, 1))
			case 1:
				return rt.Commit(ctx, waitOn(children[1:], durable.JoinFailFast, 2))
			}
			return rt.Commit(ctx, endWith("completed", "parent"))
		},
	})
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { return n.logged("round:0") })
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	children := f.children()
	n.open("b", "completed")
	waitDone(t, o.h, children[1])
	if status := stateOf(t, o.h, parent).Status; status != durable.TaskWaiting {
		t.Fatalf("parent = %s", status)
	}
	n.open("a", "completed")
	eventually(t, func() bool { return n.logged("round:1") })
	n.open("c", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	if got := n.prefixed("round:"); !reflect.DeepEqual(got, []string{"round:0", "round:1", "round:2"}) {
		t.Fatalf("rounds = %v", got)
	}
}

// ─── Completing ────────────────────────────────────────────────────────────

func TestStructuredHoldsAFinishedTaskUntilItsOwnedWorkDrainsAndWaitersAndTaskDocumentsWaitForTheFinalCommit(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	notes := taskNotes()
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			draft, err := tx.Doc(notes.Task(rt.TaskID))
			if err != nil {
				return nil, err
			}
			if err := draft.Set("text", "notes"); err != nil {
				return nil, err
			}
			if _, err := n.spawn(tx, rt.TaskID, "child"); err != nil {
				return nil, err
			}
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	if got := jsonOf(t, stateOf(t, o.h, parent)); got != `{"status":"completing","outcome":{"status":"completed","result":"parent"}}` {
		t.Fatalf("parent = %s", got)
	}
	waiter := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, parent) })
	if settledResult(waiter) {
		t.Fatal("the waiter settled before the final commit")
	}
	if got := snapshotOf(t, o.h, notes.Task(parent)); got["text"] != "notes" {
		t.Fatalf("notes = %v", got)
	}
	if got := inspected(t, o.h, parent); got == nil || got.Kind != "completing" {
		t.Fatalf("inspected = %+v", got)
	}
	idling := idle(t, o.root.WaitForIdle)
	if settledResult(idling) {
		t.Fatal("the root was idle while the parent holds")
	}
	n.open("child", "completed")
	record, err := await(t, waiter)
	must(t, err)
	if got := outcomeJSON(t, record); got != `{"status":"completed","result":"parent"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := snapshotOf(t, o.h, notes.Task(parent)); got != nil {
		t.Fatalf("notes after the final commit = %v", got)
	}
	if _, err := await(t, idling); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredKeepsHoldingForOrdinaryWorkCreatedDuringTheHold(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	var mu sync.Mutex
	var conversation durable.ConversationID
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			created, err := tx.CreateConversation(ownedBy(rt.TaskID))
			if err != nil {
				return nil, err
			}
			mu.Lock()
			conversation = created.ID
			mu.Unlock()
			if _, err := CreateTask(tx, n.node, nodeInput{Name: "first"}, inConversation(created.ID, false)); err != nil {
				return nil, err
			}
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	mu.Lock()
	child := conversationOf(t, o.h, conversation)
	mu.Unlock()
	n.start(t, child, "second")
	n.open("first", "completed")
	eventually(t, func() bool { return n.logged("run:second") })
	if status := stateOf(t, o.h, parent).Status; status != durable.TaskCompleting {
		t.Fatalf("parent = %s", status)
	}
	n.open("second", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
}

func conversationOf(t *testing.T, h *Harness, id durable.ConversationID) *Conversation {
	t.Helper()
	return conversation(t, h, id)
}

func TestStructuredAbortsTheWorkBelowAHeldOutcomeThenFinishesWithIt(t *testing.T) {
	for _, c := range []struct{ label, ending, want string }{
		{"a held failure", "failed", durable.OutcomeFailed},
		{"a held scheduler fault", "throw", durable.OutcomeFaulted},
	} {
		t.Run(c.label, func(t *testing.T) {
			n := newNodes(t)
			o := n.openNodes(t, nil)
			var mu sync.Mutex
			var child durable.TaskID
			n.script("parent", behavior{
				run: func(ctx context.Context, rt *TaskRuntime) error {
					return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
						id, err := n.spawn(tx, rt.TaskID, "child")
						mu.Lock()
						child = id
						mu.Unlock()
						if err != nil {
							return nil, err
						}
						return toResume(tx, durable.TaskRecord{})
					})
				},
				resume: func(ctx context.Context, rt *TaskRuntime, _ int) error {
					if c.ending == "throw" {
						return errors.New("parent threw")
					}
					return rt.Commit(ctx, endWith("failed", "parent"))
				},
			})
			parent := n.start(t, o.root, "parent")
			eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return child != 0 })
			mu.Lock()
			spawned := child
			mu.Unlock()
			if got := outcomeOf(t, o.h, spawned); got != durable.OutcomeAborted {
				t.Fatalf("child = %s", got)
			}
			if got := outcomeOf(t, o.h, parent); got != c.want {
				t.Fatalf("parent = %s, want %s", got, c.want)
			}
		})
	}
}

func TestStructuredOnlyMarksACompletingTaskWhenAbortedAndTheHeldOutcomeStays(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	child := n.spawnAndFinish("parent", "child")
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	if result, err := o.h.AbortTask(bg, parent); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeOf(t, o.h, child()); got != durable.OutcomeAborted {
		t.Fatalf("child = %s", got)
	}
	record := waitDone(t, o.h, parent)
	if got := outcomeJSON(t, record); got != `{"status":"completed","result":"parent"}` || !record.AbortRequested {
		t.Fatalf("parent = %s", jsonOf(t, record))
	}
	if n.logged("abort:parent") {
		t.Fatal("the parent's abort handler ran")
	}
	if result, err := o.h.AbortTask(bg, parent); err != nil || result != "terminal" {
		t.Fatalf("second abort = %s, %v", result, err)
	}
}

// ─── Abort order ───────────────────────────────────────────────────────────

// chain scripts name to spawn child and wait on it; its abort handler logs
// the child's status first.
func (n *nodes) chain(h func() *Harness, name, child string) func() durable.TaskID {
	var mu sync.Mutex
	var id durable.TaskID
	get := func() durable.TaskID { mu.Lock(); defer mu.Unlock(); return id }
	n.script(name, behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
				spawned, err := n.spawn(tx, rt.TaskID, child)
				if err != nil {
					return nil, err
				}
				mu.Lock()
				id = spawned
				mu.Unlock()
				return waitOn([]durable.TaskID{spawned}, durable.JoinAllSettled, 1)(tx, current)
			})
		},
		abort: func(ctx context.Context, rt *TaskRuntime) error {
			record, err := h().GetTask(ctx, get())
			if err != nil {
				return err
			}
			n.add(fmt.Sprintf("%s saw %s", name, record.State.Status))
			return rt.Commit(ctx, abortWith(""))
		},
	})
	return get
}

func TestStructuredRunsAbortHandlersBottomUpAcrossThreeLevelsEachAfterTheLevelBelowIsTerminal(t *testing.T) {
	n := newNodes(t)
	var h *Harness
	harness := func() *Harness { return h }
	b := n.chain(harness, "a", "b")
	c := n.chain(harness, "b", "c")
	o := n.openNodes(t, nil)
	h = o.h
	a := n.start(t, o.root, "a")
	eventually(t, func() bool { return n.logged("run:c") })
	eventually(t, func() bool { return c() != 0 })
	waitStatus(t, o.h, c(), durable.TaskRunning)
	if result, err := o.h.AbortTask(bg, a); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeOf(t, o.h, a); got != durable.OutcomeAborted {
		t.Fatalf("a = %s", got)
	}
	var got []string
	for _, line := range n.lines() {
		if strings.HasPrefix(line, "abort:") || strings.Contains(line, " saw ") {
			got = append(got, line)
		}
	}
	if want := []string{"abort:c", "abort:b", "b saw terminal", "abort:a", "a saw terminal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aborts = %v", got)
	}
	if b() == 0 {
		t.Fatal("b was not spawned")
	}
}

func TestStructuredAbortDoesNotWaitForATaskItWaitsOnButDoesNotOwn(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	other := n.start(t, o.root, "other")
	n.script("parent", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, waitOn([]durable.TaskID{other}, durable.JoinAllSettled, 1))
	}})
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	if _, err := o.h.AbortTask(bg, parent); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeAborted {
		t.Fatalf("parent = %s", got)
	}
	if status := stateOf(t, o.h, other).Status; status != durable.TaskRunning {
		t.Fatalf("other = %s", status)
	}
	n.open("other", "completed")
}

func TestStructuredReportsAnAbortMarkedTaskAsWaitingForItsLiveOwnedWork(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.slowAbort("child")
	child := n.chainless("parent", "child")
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	if _, err := o.h.AbortTask(bg, parent); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return n.logged("abort:child") })
	got := inspected(t, o.h, parent)
	if got == nil || got.Kind != "waiting" || !reflect.DeepEqual(got.On, []durable.TaskID{child()}) {
		t.Fatalf("inspected = %+v", got)
	}
	n.open("abort.child", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeAborted {
		t.Fatalf("parent = %s", got)
	}
}

// chainless scripts name to spawn child and wait on it, keeping the child's
// own behavior.
func (n *nodes) chainless(name, child string) func() durable.TaskID {
	var mu sync.Mutex
	var id durable.TaskID
	n.script(name, behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			spawned, err := n.spawn(tx, rt.TaskID, child)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			id = spawned
			mu.Unlock()
			return waitOn([]durable.TaskID{spawned}, durable.JoinAllSettled, 1)(tx, current)
		})
	}})
	return func() durable.TaskID { mu.Lock(); defer mu.Unlock(); return id }
}

func TestStructuredFaultsAnAbortHandlerThatTriesToWait(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.script("parent", behavior{abort: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, waitOn([]durable.TaskID{}, durable.JoinAllSettled, 1))
	}})
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { return n.logged("run:parent") })
	if _, err := o.h.AbortTask(bg, parent); err != nil {
		t.Fatal(err)
	}
	faultedWith(t, o.h, parent, "cannot wait")
}

func TestStructuredOrphansABlockedTaskOnlyAfterItsOwnedWorkDrained(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	var owner, child durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if owner, err = CreateTask(tx, n.unregistered, nodeInput{Name: "owner"}, conversationOwned(false)); err != nil {
			return err
		}
		child, err = n.spawn(tx, owner, "child")
		return err
	}))
	eventually(t, func() bool { return n.logged("run:child") })
	if result, err := o.h.AbortTask(bg, owner); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeOf(t, o.h, child); got != durable.OutcomeAborted {
		t.Fatalf("child = %s", got)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, owner)); got != `{"status":"orphaned","reason":"missing_task"}` {
		t.Fatalf("owner = %s", got)
	}
}

func TestStructuredCascadesThroughTaskAndConversationEdgesBottomUp(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.chainless("a", "b")
	var mu sync.Mutex
	var x durable.TaskID
	n.script("b", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				conversation, err := tx.CreateConversation(ownedBy(rt.TaskID))
				if err != nil {
					return nil, err
				}
				id, err := CreateTask(tx, n.node, nodeInput{Name: "x"}, inConversation(conversation.ID, false))
				mu.Lock()
				x = id
				mu.Unlock()
				if err != nil {
					return nil, err
				}
				return toResume(tx, durable.TaskRecord{})
			})
		},
		resume: func(ctx context.Context, _ *TaskRuntime, _ int) error { return signalled(ctx) },
	})
	a := n.start(t, o.root, "a")
	eventually(t, func() bool { return n.logged("run:x") })
	if _, err := o.h.AbortTask(bg, a); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, o.h, a); got != durable.OutcomeAborted {
		t.Fatalf("a = %s", got)
	}
	mu.Lock()
	below := x
	mu.Unlock()
	if got := outcomeOf(t, o.h, below); got != durable.OutcomeAborted {
		t.Fatalf("x = %s", got)
	}
	if got := n.prefixed("abort:"); !reflect.DeepEqual(got, []string{"abort:x", "abort:b", "abort:a"}) {
		t.Fatalf("aborts = %v", got)
	}
}

// ─── Background and terminal owners ────────────────────────────────────────

func TestStructuredKeepsBackgroundWorkThroughConversationAbortAndBackgroundTrueAbortsItAndWaits(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	foreground := n.start(t, o.root, "foreground")
	var mu sync.Mutex
	var child, below durable.TaskID
	n.script("background", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			spawned, err := n.spawn(tx, rt.TaskID, "child")
			if err != nil {
				return nil, err
			}
			conversation, err := tx.CreateConversation(ownedBy(rt.TaskID))
			if err != nil {
				return nil, err
			}
			under, err := CreateTask(tx, n.node, nodeInput{Name: "below"}, inConversation(conversation.ID, false))
			if err != nil {
				return nil, err
			}
			mu.Lock()
			child, below = spawned, under
			mu.Unlock()
			return waitOn([]durable.TaskID{spawned}, durable.JoinAllSettled, 1)(tx, current)
		})
	}})
	background := n.start(t, o.root, "background", conversationOwned(true))
	eventually(t, func() bool { return n.logged("run:below") && n.logged("run:child") })
	must(t, o.root.Abort(bg, false))
	if got := outcomeOf(t, o.h, foreground); got != durable.OutcomeAborted {
		t.Fatalf("foreground = %s", got)
	}
	mu.Lock()
	kept := []durable.TaskID{background, child, below}
	mu.Unlock()
	for _, id := range kept {
		if status := stateOf(t, o.h, id).Status; status == durable.TaskTerminal {
			t.Fatalf("task %d is terminal after an ordinary abort", id)
		}
	}
	must(t, o.root.Abort(bg, true))
	for _, id := range kept {
		if status := stateOf(t, o.h, id).Status; status != durable.TaskTerminal {
			t.Fatalf("task %d is %s after a background abort", id, status)
		}
	}
}

func TestStructuredNeverCascadesFromATerminalOwnerAndAnAbortedSubagentsConversationRunsNewWork(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	var mu sync.Mutex
	var conversation durable.ConversationID
	n.script("agent", behavior{
		run: func(ctx context.Context, rt *TaskRuntime) error {
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				created, err := tx.CreateConversation(ownedBy(rt.TaskID))
				if err != nil {
					return nil, err
				}
				mu.Lock()
				conversation = created.ID
				mu.Unlock()
				return toResume(tx, durable.TaskRecord{})
			})
		},
		resume: func(ctx context.Context, _ *TaskRuntime, _ int) error { return signalled(ctx) },
	})
	agent := n.start(t, o.root, "agent")
	eventually(t, func() bool { return n.logged("resume:agent") })
	if _, err := o.h.AbortTask(bg, agent); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, o.h, agent); got != durable.OutcomeAborted {
		t.Fatalf("agent = %s", got)
	}
	mu.Lock()
	child := conversationOf(t, o.h, conversation)
	mu.Unlock()
	question := n.start(t, child, "question")
	n.open("question", "completed")
	if got := outcomeOf(t, o.h, question); got != durable.OutcomeCompleted {
		t.Fatalf("question = %s", got)
	}
}

// ─── Recovery ──────────────────────────────────────────────────────────────

type seeded struct {
	parent   durable.TaskID
	children []durable.TaskID
}

// seed writes records a crash could leave, without a Harness, into JSONL
// storage at directory: parent with children named children, then edit
// replaces records through the raw task write.
func (n *nodes) seed(t *testing.T, directory string, children []string, edit func(set func(id durable.TaskID, change func(*durable.TaskRecord)), s seeded)) seeded {
	t.Helper()
	session := durable.NewSession(openJSONL(t, directory), durable.SessionOptions{})
	var s seeded
	must(t, session.Commit(bg, func(tx *durable.Tx) error {
		root, err := tx.CreateConversation(durable.Ownerless)
		if err != nil {
			return err
		}
		if s.parent, err = CreateTask(tx, n.node, nodeInput{Name: "parent"}, inConversation(root.ID, false)); err != nil {
			return err
		}
		for _, name := range children {
			id, err := n.spawn(tx, s.parent, name)
			if err != nil {
				return err
			}
			s.children = append(s.children, id)
		}
		return nil
	}))
	edit(func(id durable.TaskID, change func(*durable.TaskRecord)) {
		must(t, session.Commit(bg, func(tx *durable.Tx) error {
			record, err := tx.Task(id)
			if err != nil {
				return err
			}
			updated := record.Clone()
			change(&updated)
			return tx.SetTask(updated)
		}))
	}, s)
	must(t, session.Close(bg))
	return s
}

func waitingState(t *testing.T, on []durable.TaskID, policy durable.JoinPolicy) durable.TaskState {
	t.Helper()
	state, err := Wait(on, policy, nodeState{Phase: "resume", Round: 1})
	must(t, err)
	return *state
}

func completedState(t *testing.T) durable.TaskState {
	t.Helper()
	state, err := Complete("done")
	must(t, err)
	return *state
}

func failedState() durable.TaskState { return *Fail("declined") }

func TestStructuredRecoveryResumesAParentWhoseAwaitedChildrenFinishedBeforeTheCrash(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	s := n.seed(t, directory, []string{"c1"}, func(set func(durable.TaskID, func(*durable.TaskRecord)), s seeded) {
		set(s.children[0], func(r *durable.TaskRecord) { r.State = completedState(t) })
		set(s.parent, func(r *durable.TaskRecord) { r.State = waitingState(t, s.children, durable.JoinAllSettled) })
	})
	o := n.openNodes(t, openJSONL(t, directory))
	if got := outcomeOf(t, o.h, s.parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	if got := n.lines(); !reflect.DeepEqual(got, []string{"resume:parent"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestStructuredRecoveryMarksFailFastSiblingsACrashLeftUnmarked(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	s := n.seed(t, directory, []string{"c1", "c2"}, func(set func(durable.TaskID, func(*durable.TaskRecord)), s seeded) {
		set(s.children[0], func(r *durable.TaskRecord) { r.State = failedState() })
		set(s.parent, func(r *durable.TaskRecord) { r.State = waitingState(t, s.children, durable.JoinFailFast) })
	})
	o := n.openNodes(t, openJSONL(t, directory))
	if got := outcomeOf(t, o.h, s.children[1]); got != durable.OutcomeAborted {
		t.Fatalf("c2 = %s", got)
	}
	if got := outcomeOf(t, o.h, s.parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	if n.logged("run:c2") {
		t.Fatal("c2 ran")
	}
	must(t, o.h.Close(bg))
}

func TestStructuredRecoveryFinalizesAHeldOutcomeWhoseWorkDrainedBeforeTheCrashAndKeepsOneWhoseWorkLives(t *testing.T) {
	n := newNodes(t)
	held := durable.TaskState{Status: durable.TaskCompleting, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: json.RawMessage(`"parent"`)}}
	directory := t.TempDir()
	s := n.seed(t, directory, []string{"c1"}, func(set func(durable.TaskID, func(*durable.TaskRecord)), s seeded) {
		set(s.parent, func(r *durable.TaskRecord) { r.State = held })
	})
	o := n.openNodes(t, openJSONL(t, directory))
	eventually(t, func() bool { return n.logged("run:c1") })
	if got, want := jsonOf(t, stateOf(t, o.h, s.parent)), jsonOf(t, held); got != want {
		t.Fatalf("parent = %s, want %s", got, want)
	}
	must(t, o.h.Close(bg))

	o = n.openNodes(t, openJSONL(t, directory))
	n.open("c1", "completed")
	if got := outcomeOf(t, o.h, s.children[0]); got != durable.OutcomeCompleted {
		t.Fatalf("c1 = %s", got)
	}
	if got := outcomeOf(t, o.h, s.parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	must(t, o.h.Close(bg))

	drained := t.TempDir()
	second := n.seed(t, drained, []string{"c1"}, func(set func(durable.TaskID, func(*durable.TaskRecord)), s seeded) {
		set(s.children[0], func(r *durable.TaskRecord) { r.State = completedState(t) })
		set(s.parent, func(r *durable.TaskRecord) { r.State = held })
	})
	o = n.openNodes(t, openJSONL(t, drained))
	if got := outcomeOf(t, o.h, second.parent); got != durable.OutcomeCompleted {
		t.Fatalf("drained parent = %s", got)
	}
	must(t, o.h.Close(bg))
}

func TestStructuredRecoveryResumesABottomUpAbortACrashInterruptedBeforeTheCascade(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	s := n.seed(t, directory, []string{"c1"}, func(set func(durable.TaskID, func(*durable.TaskRecord)), s seeded) {
		set(s.parent, func(r *durable.TaskRecord) {
			r.AbortRequested = true
			r.State = waitingState(t, s.children, durable.JoinAllSettled)
		})
	})
	o := n.openNodes(t, openJSONL(t, directory))
	if got := outcomeOf(t, o.h, s.parent); got != durable.OutcomeAborted {
		t.Fatalf("parent = %s", got)
	}
	if got := outcomeOf(t, o.h, s.children[0]); got != durable.OutcomeAborted {
		t.Fatalf("c1 = %s", got)
	}
	if got := n.prefixed("abort:"); !reflect.DeepEqual(got, []string{"abort:c1", "abort:parent"}) {
		t.Fatalf("aborts = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestStructuredRecoveryReopensACheckoutWaitingOnLivePaymentsAndFinishesIt(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	f := n.parentOf("checkout", []string{"p1", "p2"}, durable.JoinFailFast)
	o := n.openNodes(t, openJSONL(t, directory))
	parent := n.start(t, o.root, "checkout")
	waitStatus(t, o.h, parent, durable.TaskWaiting)
	eventually(t, func() bool { return n.logged("run:p1") && n.logged("run:p2") })
	must(t, o.h.Close(bg))

	o = n.openNodes(t, openJSONL(t, directory))
	f.mu.Lock()
	f.ids = stateOf(t, o.h, parent).On
	f.mu.Unlock()
	n.open("p1", "completed")
	n.open("p2", "completed")
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	if got := f.read(); !reflect.DeepEqual(got, []string{"completed", "completed"}) {
		t.Fatalf("outcomes = %v", got)
	}
	must(t, o.h.Close(bg))
}

// ─── Definitions and waits ─────────────────────────────────────────────────

type waiterOn struct {
	On []durable.TaskID `json:"on"`
}

// waiterVersion is a waiter of its own kind, so its definition can be
// removed or replaced; version 2 migrates to a phase that completes "v2".
func waiterVersion(version int) Task {
	type running = Running[waiterOn, holdState]
	spec := TaskSpec[waiterOn, holdState, any]{
		Name:    "test.versioned",
		Version: version,
		Initial: func(waiterOn) holdState { return holdState{Phase: "wait"} },
		Phases: map[string]func(context.Context, *running, *TaskRuntime) error{
			"wait": func(ctx context.Context, task *running, rt *TaskRuntime) error {
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Wait(task.Input.On, durable.JoinAllSettled, holdState{Phase: "resume"})
				})
			},
			"resume": func(ctx context.Context, _ *running, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith("v1"))
			},
			"migrated": func(ctx context.Context, _ *running, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith("v2"))
			},
		},
		Abort: func(ctx context.Context, _ *running, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	}
	if version == 2 {
		spec.Migrate = func(input, _ json.RawMessage, _ int) (waiterOn, holdState, error) {
			var in waiterOn
			return in, holdState{Phase: "migrated"}, json.Unmarshal(input, &in)
		}
	}
	return MustDefineTask(spec)
}

func (n *nodes) startWaiter(t *testing.T, c *Conversation, version int, on durable.TaskID) durable.TaskID {
	t.Helper()
	var id durable.TaskID
	must(t, c.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, waiterVersion(version), waiterOn{On: []durable.TaskID{on}}, conversationOwned(false))
		return err
	}))
	return id
}

func TestStructuredKeepsAWaitingTaskBlockedWithoutItsDefinitionAndResumesItMigratedUnderANewerOne(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	registration := addTask(t, o.registry, waiterVersion(1))
	other := n.start(t, o.root, "other")
	waiter := n.startWaiter(t, o.root, 1, other)
	waitStatus(t, o.h, waiter, durable.TaskWaiting)
	o.registry.Uninstall(registration)
	n.open("other", "completed")
	waitDone(t, o.h, other)
	eventually(t, func() bool {
		got := inspected(t, o.h, waiter)
		return got != nil && got.Kind == "blocked" && got.Reason == "missing_task"
	})
	if status := stateOf(t, o.h, waiter).Status; status != durable.TaskWaiting {
		t.Fatalf("waiter = %s", status)
	}
	addTask(t, o.registry, waiterVersion(2))
	if got := outcomeJSON(t, waitDone(t, o.h, waiter)); got != `{"status":"completed","result":"v2"}` {
		t.Fatalf("waiter = %s", got)
	}
}

func TestStructuredOrphansAnAbortedWaitingTaskWithoutItsDefinitionAtOnceLeavingTheTaskItWaitsOnRunning(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	registration := addTask(t, o.registry, waiterVersion(1))
	other := n.start(t, o.root, "other")
	waiter := n.startWaiter(t, o.root, 1, other)
	waitStatus(t, o.h, waiter, durable.TaskWaiting)
	o.registry.Uninstall(registration)
	if result, err := o.h.AbortTask(bg, waiter); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := jsonOf(t, stateOf(t, o.h, waiter).Outcome); got != `{"status":"orphaned","reason":"missing_task"}` {
		t.Fatalf("waiter = %s", got)
	}
	if status := stateOf(t, o.h, other).Status; status != durable.TaskRunning {
		t.Fatalf("other = %s", status)
	}
	n.open("other", "completed")
}

func TestStructuredNeverMigratesAHeldOutcomeAndANewerDefinitionLeavesItAndItsVersionAlone(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	holder := func(version int) Task {
		return MustDefineTask(TaskSpec[any, holdState, any]{
			Name:    "test.holder",
			Version: version,
			Initial: func(any) holdState { return holdState{Phase: "run"} },
			Phases: map[string]func(context.Context, *Running[any, holdState], *TaskRuntime) error{
				"run": func(ctx context.Context, _ *Running[any, holdState], rt *TaskRuntime) error {
					if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
						_, err := n.spawn(tx, rt.TaskID, "child")
						return nil, err
					}); err != nil {
						return err
					}
					return rt.Commit(ctx, completeWith("held"))
				},
			},
			Abort: func(ctx context.Context, _ *Running[any, holdState], rt *TaskRuntime) error {
				return rt.Commit(ctx, abortWith(""))
			},
			Migrate: func(json.RawMessage, json.RawMessage, int) (any, holdState, error) {
				return nil, holdState{}, errors.New("never migrates")
			},
		})
	}
	addTask(t, o.registry, holder(1))
	var parent durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		parent, err = CreateTask[any](tx, holder(1), nil, conversationOwned(false))
		return err
	}))
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	// The same extension name replaces the old one in place.
	addTask(t, o.registry, holder(2))
	n.open("child", "completed")
	record := waitDone(t, o.h, parent)
	if got := outcomeJSON(t, record); got != `{"status":"completed","result":"held"}` || record.Version != 1 {
		t.Fatalf("parent = %s", jsonOf(t, record))
	}
	if got := reportTexts(o.reports); len(got) != 0 {
		t.Fatalf("reports = %v", got)
	}
}

func TestStructuredTreatsAHeldTaskAsLiveSoOutcomesRejectsAndAWaiterResumesAtItsFinalCommit(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.spawnAndFinish("held", "child")
	held := n.start(t, o.root, "held")
	waitStatus(t, o.h, held, durable.TaskCompleting)
	var mu sync.Mutex
	var rejection string
	n.script("reader", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		_, err := rt.Outcomes(ctx, []durable.TaskID{held})
		mu.Lock()
		rejection = "resolved"
		if err != nil {
			rejection = err.Error()
		}
		mu.Unlock()
		return rt.Commit(ctx, waitOn([]durable.TaskID{held}, durable.JoinAllSettled, 1))
	}})
	reader := n.start(t, o.root, "reader")
	waitStatus(t, o.h, reader, durable.TaskWaiting)
	mu.Lock()
	got := rejection
	mu.Unlock()
	if want := fmt.Sprintf("Task %d is not terminal", held); got != want {
		t.Fatalf("outcomes = %q, want %q", got, want)
	}
	n.open("child", "completed")
	if got := outcomeOf(t, o.h, reader); got != durable.OutcomeCompleted {
		t.Fatalf("reader = %s", got)
	}
	if !n.logged("resume:reader") {
		t.Fatal("the reader did not resume")
	}
}

// ─── Conversation abort and boundaries ─────────────────────────────────────

func TestStructuredMarksAHeldCompletedTaskWithConversationAbortAndTheOutcomeStays(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	child := n.spawnAndFinish("held", "child")
	held := n.start(t, o.root, "held")
	waitStatus(t, o.h, held, durable.TaskCompleting)
	must(t, o.root.Abort(bg, false))
	if got := outcomeOf(t, o.h, child()); got != durable.OutcomeAborted {
		t.Fatalf("child = %s", got)
	}
	record := waitDone(t, o.h, held)
	if record.State.Outcome.Status != durable.OutcomeCompleted || !record.AbortRequested {
		t.Fatalf("held = %s", jsonOf(t, record))
	}
}

func TestStructuredMarksAndAwaitsOnlyTheBackgroundWorkReachedWhenBackgroundTrueIsAdmitted(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.slowAbort("first")
	first := n.start(t, o.root, "first", conversationOwned(true))
	eventually(t, func() bool { return n.logged("run:first") })
	aborting := async(func() (struct{}, error) { return struct{}{}, o.root.Abort(bg, true) })
	waitMarked(t, o.h, first)
	later := n.start(t, o.root, "later", conversationOwned(true))
	n.open("abort.first", "completed")
	if _, err := await(t, aborting); err != nil {
		t.Fatal(err)
	}
	if record := getTask(t, o.h, later); record.State.Status == durable.TaskTerminal || record.AbortRequested {
		t.Fatalf("later = %s", jsonOf(t, record))
	}
	if _, err := o.h.AbortTask(bg, later); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredStopsACascadeAtAnUnmarkedBackgroundTaskButNotAtAMarkedOne(t *testing.T) {
	n := newNodes(t)
	o := n.openNodes(t, nil)
	n.slowAbort("owner")
	var owner, background durable.TaskID
	var outerID, innerID durable.ConversationID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if owner, err = CreateTask(tx, n.node, nodeInput{Name: "owner"}, conversationOwned(false)); err != nil {
			return err
		}
		outer, err := tx.CreateConversation(ownedBy(owner))
		if err != nil {
			return err
		}
		if background, err = CreateTask(tx, n.node, nodeInput{Name: "background"}, inConversation(outer.ID, true)); err != nil {
			return err
		}
		inner, err := tx.CreateConversation(ownedBy(background))
		if err != nil {
			return err
		}
		outerID, innerID = outer.ID, inner.ID
		return nil
	}))
	eventually(t, func() bool { return n.logged("run:owner") && n.logged("run:background") })
	// The owner's abort handler starts at once: background work is not its
	// ordinary owned work.
	if _, err := o.h.AbortTask(bg, owner); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return n.logged("abort:owner") })
	shielded := n.start(t, conversationOf(t, o.h, innerID), "shielded")
	exposed := n.start(t, conversationOf(t, o.h, outerID), "exposed")
	if got := outcomeOf(t, o.h, exposed); got != durable.OutcomeAborted {
		t.Fatalf("exposed = %s", got)
	}
	if getTask(t, o.h, shielded).AbortRequested {
		t.Fatal("the cascade crossed an unmarked background task")
	}
	// Marked directly, the background task cascades into its own subtree.
	if _, err := o.h.AbortTask(bg, background); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, o.h, shielded); got != durable.OutcomeAborted {
		t.Fatalf("shielded = %s", got)
	}
	if got := outcomeOf(t, o.h, background); got != durable.OutcomeAborted {
		t.Fatalf("background = %s", got)
	}
	n.open("abort.owner", "completed")
	if got := outcomeOf(t, o.h, owner); got != durable.OutcomeAborted {
		t.Fatalf("owner = %s", got)
	}
}

// finalizeFailing fails the commit that makes the task reject terminal,
// once.
type finalizeFailing struct {
	*controlledStorage
	failure error
	mu      sync.Mutex
	reject  durable.TaskID
}

func (s *finalizeFailing) rejectFinal(id durable.TaskID) {
	s.mu.Lock()
	s.reject = id
	s.mu.Unlock()
}

func (s *finalizeFailing) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	for _, write := range writes {
		if task, ok := write.(durable.TaskWrite); ok && s.reject != 0 && task.Value.ID == s.reject && task.Value.State.Status == durable.TaskTerminal {
			s.reject = 0
			s.mu.Unlock()
			return 0, s.failure
		}
	}
	s.mu.Unlock()
	return s.controlledStorage.Commit(ctx, writes)
}

func TestStructuredFailsTheHarnessOnAFailedFinalizationWhichReopeningApplies(t *testing.T) {
	n := newNodes(t)
	failure := errors.New("disk gone")
	storage := &finalizeFailing{controlledStorage: newControlledStorage(), failure: failure}
	o := n.openNodes(t, storage)
	n.spawnAndFinish("parent", "child")
	parent := n.start(t, o.root, "parent")
	waitStatus(t, o.h, parent, durable.TaskCompleting)
	storage.rejectFinal(parent)
	n.open("child", "completed")
	select {
	case <-o.h.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the Harness did not close")
	}
	if !errors.Is(o.h.Failure(), failure) {
		t.Fatalf("failure = %v", o.h.Failure())
	}
	if got := o.reports.all(); len(got) != 1 || !errors.Is(got[0], failure) {
		t.Fatalf("reports = %v", got)
	}
	closeSoon(t, o.h)
	reopened := n.openNodes(t, storage.reopen())
	if got := outcomeOf(t, reopened.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	must(t, reopened.h.Close(bg))
}

// ─── Recovery through ownership edges ──────────────────────────────────────

func TestStructuredShowsAnAbortMarkedOwnerWaitingForWorkInItsOwnedConversationBeforeResumeAfterReopen(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	session := durable.NewSession(openJSONL(t, directory), durable.SessionOptions{})
	var owner, inner durable.TaskID
	must(t, session.Commit(bg, func(tx *durable.Tx) error {
		conversation, err := tx.CreateConversation(durable.Ownerless)
		if err != nil {
			return err
		}
		if owner, err = CreateTask(tx, n.node, nodeInput{Name: "owner"}, inConversation(conversation.ID, false)); err != nil {
			return err
		}
		child, err := tx.CreateConversation(ownedBy(owner))
		if err != nil {
			return err
		}
		inner, err = CreateTask(tx, n.node, nodeInput{Name: "inner"}, inConversation(child.ID, false))
		return err
	}))
	must(t, session.Commit(bg, func(tx *durable.Tx) error {
		record, err := tx.Task(owner)
		if err != nil {
			return err
		}
		marked := record.Clone()
		marked.AbortRequested = true
		return tx.SetTask(marked)
	}))
	must(t, session.Close(bg))

	o := openRoot(t, []Task{n.node}, openOptions{storage: openJSONL(t, directory)})
	got := inspected(t, o.h, owner)
	if got == nil || got.Kind != "waiting" || !reflect.DeepEqual(got.On, []durable.TaskID{inner}) {
		t.Fatalf("inspected = %+v", got)
	}
	must(t, o.h.Resume())
	if got := outcomeOf(t, o.h, owner); got != durable.OutcomeAborted {
		t.Fatalf("owner = %s", got)
	}
	if got := n.prefixed("abort:"); !reflect.DeepEqual(got, []string{"abort:inner", "abort:owner"}) {
		t.Fatalf("aborts = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestStructuredKeepsTheRootBusyAfterReopenForWorkBelowAChildTasksOwnedConversation(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	n.script("child", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			conversation, err := tx.CreateConversation(ownedBy(rt.TaskID))
			if err != nil {
				return nil, err
			}
			if _, err := CreateTask(tx, n.node, nodeInput{Name: "inner"}, inConversation(conversation.ID, false)); err != nil {
				return nil, err
			}
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	n.parentOf("parent", []string{"child"}, durable.JoinAllSettled)
	o := n.openNodes(t, openJSONL(t, directory))
	parent := n.start(t, o.root, "parent")
	eventually(t, func() bool { return n.logged("run:inner") })
	must(t, o.h.Close(bg))

	o = n.openNodes(t, openJSONL(t, directory))
	idling := idle(t, o.root.WaitForIdle)
	if settledResult(idling) {
		t.Fatal("the root was idle after reopen with work below")
	}
	n.open("inner", "completed")
	if _, err := await(t, idling); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, o.h, parent); got != durable.OutcomeCompleted {
		t.Fatalf("parent = %s", got)
	}
	must(t, o.h.Close(bg))
}

func TestStructuredKeepsAFinishedBackgroundAncestorABoundaryAfterReopenWhichBackgroundTrueCrosses(t *testing.T) {
	n := newNodes(t)
	directory := t.TempDir()
	var mu sync.Mutex
	var conversation durable.ConversationID
	n.script("child", behavior{run: func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			created, err := tx.CreateConversation(ownedBy(rt.TaskID))
			if err != nil {
				return nil, err
			}
			mu.Lock()
			conversation = created.ID
			mu.Unlock()
			return toResume(tx, durable.TaskRecord{})
		})
	}})
	n.parentOf("background", []string{"child"}, durable.JoinAllSettled)
	o := n.openNodes(t, openJSONL(t, directory))
	background := n.start(t, o.root, "background", conversationOwned(true))
	waitDone(t, o.h, background)
	must(t, o.h.Close(bg))

	o = n.openNodes(t, openJSONL(t, directory))
	mu.Lock()
	child := conversationOf(t, o.h, conversation)
	mu.Unlock()
	below := n.start(t, child, "below")
	eventually(t, func() bool { return n.logged("run:below") })
	must(t, o.root.WaitForIdle(bg))
	must(t, o.root.Abort(bg, false))
	if status := stateOf(t, o.h, below).Status; status != durable.TaskRunning {
		t.Fatalf("below = %s after an ordinary abort", status)
	}
	must(t, o.root.Abort(bg, true))
	if got := outcomeOf(t, o.h, below); got != durable.OutcomeAborted {
		t.Fatalf("below = %s", got)
	}
	must(t, o.h.Close(bg))
}
