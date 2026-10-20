package harness

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi harness-task-graph.test.ts.

type parentState struct {
	Phase string         `json:"phase"`
	Child durable.TaskID `json:"child,omitempty"`
}

type childInput struct {
	Late bool `json:"late"`
}

// graphFamily is a parent that creates a child task and a conversation it
// owns, waits for the child, creates a second child, and completes while that
// child is still live, so its outcome is held as completing.
func graphFamily(child, late <-chan struct{}) (parent, childTask Task) {
	childTask = MustDefineTask(TaskSpec[childInput, stepState, any]{
		Name:    "test.graph-child",
		Version: 1,
		Initial: func(childInput) stepState { return stepState{Phase: "work"} },
		Phases: map[string]func(context.Context, *Running[childInput, stepState], *TaskRuntime) error{
			"work": func(ctx context.Context, task *Running[childInput, stepState], rt *TaskRuntime) error {
				gate := child
				if task.Input.Late {
					gate = late
				}
				select {
				case <-gate:
				case <-ctx.Done():
					return context.Cause(ctx)
				}
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *Running[childInput, stepState], *TaskRuntime) error { return nil },
	})
	parent = MustDefineTask(TaskSpec[any, parentState, any]{
		Name:    "test.graph-parent",
		Version: 1,
		Initial: func(any) parentState { return parentState{Phase: "spawn"} },
		Phases: map[string]func(context.Context, *Running[any, parentState], *TaskRuntime) error{
			"spawn": func(ctx context.Context, task *Running[any, parentState], rt *TaskRuntime) error {
				// Two conversations in a commit that leaves the parent's record
				// unchanged.
				if err := rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					for range 2 {
						if _, err := tx.CreateConversation(ownedBy(current.ID)); err != nil {
							return nil, err
						}
					}
					return nil, nil
				}); err != nil {
					return err
				}
				return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					id, err := CreateTask(tx, childTask, childInput{}, taskOwned(current.ID))
					if err != nil {
						return nil, err
					}
					return Wait([]durable.TaskID{id}, durable.JoinAllSettled, parentState{Phase: "join", Child: id})
				})
			},
			"join": func(ctx context.Context, task *Running[any, parentState], rt *TaskRuntime) error {
				return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					if _, err := CreateTask(tx, childTask, childInput{Late: true}, taskOwned(current.ID)); err != nil {
						return nil, err
					}
					return Next(parentState{Phase: "finish"})
				})
			},
			"finish": func(ctx context.Context, _ *Running[any, parentState], rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *Running[any, parentState], *TaskRuntime) error { return nil },
	})
	return parent, childTask
}

func decodeGraph(t *testing.T, value map[string]any) durable.TaskGraph {
	t.Helper()
	graph, err := durable.DecodeTaskGraph(value)
	must(t, err)
	return graph
}

// graphStatuses are the node statuses by "<kind>#<id>", for compact
// assertions.
func graphStatuses(t *testing.T, value map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, node := range decodeGraph(t, value).Tasks {
		out[node.Kind+"#"+jsonOf(t, node.ID)] = node.State.Status
	}
	return out
}

func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// updates collects the values a state delivers as updates.
type updates struct {
	mu     sync.Mutex
	values []map[string]any
}

func (u *updates) listen(state durable.DocumentState) {
	state.Subscribe(func(value map[string]any, _ context.Context, delivery chord.ReplicatedStateDelivery) <-chan error {
		if delivery.Kind == chord.DeliveryUpdate {
			u.mu.Lock()
			u.values = append(u.values, value)
			u.mu.Unlock()
		}
		return nil
	})
}

func (u *updates) all() []map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.values)
}

func TestTaskGraphFollowsEveryLiveTaskThroughItsStatusesOwnerEdgesAndOwnedConversations(t *testing.T) {
	child, late := make(chan struct{}), make(chan struct{})
	parentTask, childTask := graphFamily(child, late)
	o := openRoot(t, []Task{parentTask, childTask}, openOptions{})
	seen := &updates{}
	observe := func() durable.DocumentState {
		state, err := o.h.TaskGraph(bg)
		must(t, err)
		seen.listen(state)
		return state
	}
	graph := observe()
	if got := jsonOf(t, graph.Value()); got != `{"tasks":{}}` {
		t.Fatalf("graph = %s", got)
	}
	// The advanced value equals a fresh build from Storage once the last
	// observer left.
	rebuild := func() {
		t.Helper()
		advanced := graph.Value()
		graph.Dispose()
		graph = observe()
		if sameMap(graph.Value(), advanced) || !reflect.DeepEqual(graph.Value(), advanced) {
			t.Fatalf("rebuilt %s, advanced %s", jsonOf(t, graph.Value()), jsonOf(t, advanced))
		}
	}
	defer func() { graph.Dispose() }()

	parent := start(t, o.root, parentTask, false)
	eventually(t, func() bool { _, ok := decodeGraph(t, graph.Value()).Tasks[parent]; return ok })
	want := durable.TaskGraphNode{
		ID: parent, Kind: "test.graph-parent", ConversationID: o.root.ID,
		State: durable.TaskGraphState{Status: durable.TaskPending, Phase: "spawn"}, Conversations: []durable.ConversationID{},
	}
	if got := decodeGraph(t, graph.Value()).Tasks[parent]; !reflect.DeepEqual(got, want) {
		t.Fatalf("parent node = %+v, want %+v", got, want)
	}

	must(t, o.h.Resume())
	childID := func() durable.TaskID {
		for _, node := range decodeGraph(t, graph.Value()).Tasks {
			if node.Kind == "test.graph-child" {
				return node.ID
			}
		}
		return 0
	}
	eventually(t, func() bool {
		tasks := decodeGraph(t, graph.Value()).Tasks
		return len(tasks) == 2 && tasks[childID()].State.Status == durable.TaskRunning
	})
	first := childID()
	parentNode := decodeGraph(t, graph.Value()).Tasks[parent]
	if !reflect.DeepEqual(parentNode.State, durable.TaskGraphState{Status: durable.TaskWaiting, Phase: "join", On: []durable.TaskID{first}, Policy: durable.JoinAllSettled}) {
		t.Fatalf("parent state = %+v", parentNode.State)
	}
	if len(parentNode.Conversations) != 2 || !slices.IsSorted(parentNode.Conversations) {
		t.Fatalf("parent conversations = %v", parentNode.Conversations)
	}
	if node := decodeGraph(t, graph.Value()).Tasks[first]; node.Owner == nil || *node.Owner != parent || node.ConversationID != o.root.ID {
		t.Fatalf("first child = %+v", node)
	}
	conversation(t, o.h, parentNode.Conversations[0])
	rebuild()

	close(child)
	// The parent completes while the late child lives: its outcome is held.
	// The scheduler starts the late child on its own pass, possibly after the
	// parent completes; the graph is compared with a rebuild only once it
	// runs, as nothing commits after that until late is closed.
	eventually(t, func() bool {
		tasks := decodeGraph(t, graph.Value()).Tasks
		late, ok := tasks[childID()]
		return tasks[parent].State.Status == durable.TaskCompleting && ok && late.State.Status == durable.TaskRunning
	})
	tasks := decodeGraph(t, graph.Value()).Tasks
	if got := tasks[parent].State; !reflect.DeepEqual(got, durable.TaskGraphState{Status: durable.TaskCompleting, Outcome: durable.OutcomeCompleted}) {
		t.Fatalf("held parent = %+v", got)
	}
	if _, ok := tasks[first]; ok {
		t.Fatal("the ended child is still in the graph")
	}
	// Owned conversations stay listed while the owner lives.
	if !reflect.DeepEqual(tasks[parent].Conversations, parentNode.Conversations) {
		t.Fatalf("conversations = %v, want %v", tasks[parent].Conversations, parentNode.Conversations)
	}
	rebuild()

	close(late)
	waitDone(t, o.h, parent)
	eventually(t, func() bool { return jsonOf(t, graph.Value()) == `{"tasks":{}}` })
	// Every revision is one commit that changed a node; none repeats its
	// predecessor.
	values := seen.all()
	for i := 1; i < len(values); i++ {
		if reflect.DeepEqual(values[i], values[i-1]) {
			t.Fatalf("revision %d repeats its predecessor: %s", i, jsonOf(t, values[i]))
		}
	}
	// The commit that created the two conversations published one revision
	// setting them.
	if !slices.ContainsFunc(values, func(value map[string]any) bool {
		return len(decodeGraph(t, value).Tasks[parent].Conversations) == 2
	}) {
		t.Fatal("no revision set both conversations")
	}
}

func TestTaskGraphBuildsFromCommittedTasksShowsSurvivingTasksAsPendingAfterReopenAndMarksAborts(t *testing.T) {
	directory := t.TempDir()
	gate := make(chan struct{})
	work := oneStep(t, "test.graph-work", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		select {
		case <-gate:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	first := openRoot(t, []Task{work}, openOptions{storage: openJSONL(t, directory)})
	var foreground, background durable.TaskID
	var owned durable.ConversationID
	must(t, first.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if foreground, err = CreateTask[any](tx, work, nil, conversationOwned(false)); err != nil {
			return err
		}
		if background, err = CreateTask[any](tx, work, nil, conversationOwned(true)); err != nil {
			return err
		}
		conversation, err := tx.CreateConversation(ownedBy(foreground))
		owned = conversation.ID
		return err
	}))
	must(t, first.h.Resume())
	waitStatus(t, first.h, foreground, durable.TaskRunning)
	waitStatus(t, first.h, background, durable.TaskRunning)
	running, err := first.h.TaskGraph(bg)
	must(t, err)
	want := map[string]string{"test.graph-work#" + jsonOf(t, foreground): "running", "test.graph-work#" + jsonOf(t, background): "running"}
	if got := graphStatuses(t, running.Value()); !reflect.DeepEqual(got, want) {
		t.Fatalf("running = %v", got)
	}
	running.Dispose()
	must(t, first.h.Close(bg))

	// Acquired after reopen: built from the committed records and owner
	// edges; open reconciled running to pending.
	o := openRoot(t, []Task{work}, openOptions{storage: openJSONL(t, directory)})
	watch, err := o.h.WatchTaskGraph(bg)
	must(t, err)
	acquired := watch.Value()
	want = map[string]string{"test.graph-work#" + jsonOf(t, foreground): "pending", "test.graph-work#" + jsonOf(t, background): "pending"}
	if got := graphStatuses(t, acquired); !reflect.DeepEqual(got, want) {
		t.Fatalf("after reopen = %v", got)
	}
	nodes := decodeGraph(t, acquired).Tasks
	if !reflect.DeepEqual(nodes[foreground].Conversations, []durable.ConversationID{owned}) || !nodes[background].Background {
		t.Fatalf("nodes = %+v", nodes)
	}

	// Exact frames: replaying their operations from the acquisition revision
	// gives each delivered value.
	var mu sync.Mutex
	replica := acquired
	var frames [][]delta.Op
	must(t, watch.Start(func(_ context.Context, value map[string]any, ops []delta.Op) error {
		mu.Lock()
		defer mu.Unlock()
		next, err := delta.ApplyImmutable(replica, ops)
		if err != nil {
			return err
		}
		replica = next
		if !reflect.DeepEqual(replica, value) {
			return errors.New("the replayed frame differs from the delivered value")
		}
		frames = append(frames, ops)
		return nil
	}))
	must(t, o.h.Resume())
	eventually(t, func() bool {
		return decodeGraph(t, watch.Value()).Tasks[background].State.Status == durable.TaskRunning
	})
	if result, err := o.h.AbortTask(bg, background); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	waitDone(t, o.h, background)
	backgroundKey := jsonOf(t, background)
	deleted := []delta.Op{delta.Delete{Path: delta.Path{delta.Key("tasks"), delta.Key(backgroundKey)}}}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(frames) > 0 && reflect.DeepEqual(frames[len(frames)-1], deleted)
	})
	mu.Lock()
	marked := slices.ContainsFunc(frames, func(ops []delta.Op) bool {
		if len(ops) != 1 {
			return false
		}
		set, ok := ops[0].(delta.Set)
		node, _ := set.Value.(map[string]any)
		return ok && reflect.DeepEqual(set.Path, delta.Path{delta.Key("tasks"), delta.Key(backgroundKey)}) && node["abortRequested"] == true
	})
	keys := slices.Collect(func(yield func(string) bool) {
		for key := range replica["tasks"].(map[string]any) {
			if !yield(key) {
				return
			}
		}
	})
	mu.Unlock()
	if !marked {
		t.Fatal("no frame set the background node's abort mark")
	}
	if !reflect.DeepEqual(keys, []string{jsonOf(t, foreground)}) {
		t.Fatalf("replica keys = %v", keys)
	}
	watch.Stop()
	close(gate)
	waitDone(t, o.h, foreground)
	must(t, o.h.Close(bg))
}

func TestTaskGraphListsOwnedConversationsInIDOrderWhateverOrderOneCommitCreatesThemIn(t *testing.T) {
	gate := make(chan struct{})
	var mu sync.Mutex
	var created []durable.ConversationID
	type spawnInput struct {
		At durable.EntryID `json:"at"`
	}
	spawner := MustDefineTask(TaskSpec[spawnInput, stepState, any]{
		Name:    "test.graph-spawner",
		Version: 1,
		Initial: func(spawnInput) stepState { return stepState{Phase: "spawn"} },
		Phases: map[string]func(context.Context, *Running[spawnInput, stepState], *TaskRuntime) error{
			"spawn": func(ctx context.Context, task *Running[spawnInput, stepState], rt *TaskRuntime) error {
				if err := rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					// The fork first: it takes the lower ID only if minted first.
					forked, err := tx.ForkConversation(rt.ConversationID, task.Input.At, ownedBy(current.ID))
					if err != nil {
						return nil, err
					}
					fresh, err := tx.CreateConversation(ownedBy(current.ID))
					if err != nil {
						return nil, err
					}
					mu.Lock()
					created = append(created, forked.ID, fresh.ID)
					mu.Unlock()
					return nil, nil
				}); err != nil {
					return err
				}
				<-gate
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *Running[spawnInput, stepState], *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{spawner}, openOptions{})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	var id durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		entry, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		if err != nil {
			return err
		}
		id, err = CreateTask(tx, spawner, spawnInput{At: entry.ID}, conversationOwned(false))
		return err
	}))
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(decodeGraph(t, graph.Value()).Tasks[id].Conversations) == 2 })
	advanced := graph.Value()
	mu.Lock()
	sorted := slices.Sorted(slices.Values(created))
	mu.Unlock()
	if got := decodeGraph(t, advanced).Tasks[id].Conversations; !reflect.DeepEqual(got, sorted) {
		t.Fatalf("conversations = %v, want %v", got, sorted)
	}
	graph.Dispose()
	rebuilt, err := o.h.TaskGraph(bg)
	must(t, err)
	if !reflect.DeepEqual(rebuilt.Value(), advanced) {
		t.Fatalf("rebuilt %s, advanced %s", jsonOf(t, rebuilt.Value()), jsonOf(t, advanced))
	}
	rebuilt.Dispose()
	close(gate)
	waitDone(t, o.h, id)
}

func TestTaskGraphRegistersNothingForAnAcquisitionCancelledWhileItWaitsForTheLine(t *testing.T) {
	storage := newControlledStorage()
	_, childTask := graphFamily(nil, nil)
	o := openRoot(t, nil, openOptions{storage: storage})
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		_, err := CreateTask(tx, childTask, childInput{}, conversationOwned(false))
		return err
	}))
	held := storage.holdCommits()
	blocking := async(func() (struct{}, error) {
		return struct{}{}, o.h.Commit(bg, func(tx *durable.Tx) error {
			_, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "blocker"})
			return err
		})
	})
	held.Entered(t)
	ctx, cancel := context.WithCancelCause(bg)
	cancelled := async(func() (*durable.TaskGraphWatch, error) { return o.h.WatchTaskGraph(ctx) })
	parked(t, 1, onLine, "durable.(*Session).WatchTaskGraph(")
	cancel(errors.New("cancelled"))
	storage.releaseHold(held)
	if _, err := await(t, blocking); err != nil {
		t.Fatal(err)
	}
	_, err := await(t, cancelled)
	expectError(t, err, "cancelled")
	// No observer kept the mount: each new observer builds a new revision.
	first, err := o.h.TaskGraph(bg)
	must(t, err)
	value := first.Value()
	first.Dispose()
	second, err := o.h.TaskGraph(bg)
	must(t, err)
	if sameMap(second.Value(), value) || !reflect.DeepEqual(second.Value(), value) {
		t.Fatalf("second %s, first %s", jsonOf(t, second.Value()), jsonOf(t, value))
	}
	second.Dispose()
}

func TestTaskGraphPublishesNoRevisionForACommitThatChangesNoNodeAndSharesOneMountBetweenObservers(t *testing.T) {
	gate, reached := make(chan struct{}), make(chan struct{})
	memo := oneStep(t, "test.graph-memo", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		if _, err := rt.MemoOr(ctx, "seen", true); err != nil {
			return err
		}
		close(reached)
		<-gate
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{memo}, openOptions{})
	id := start(t, o.root, memo, false)
	first, err := o.h.TaskGraph(bg)
	must(t, err)
	second, err := o.h.TaskGraph(bg)
	must(t, err)
	if !sameMap(second.Value(), first.Value()) {
		t.Fatal("the observers do not share one mount")
	}
	var mu sync.Mutex
	var statuses []string
	first.Subscribe(func(value map[string]any, _ context.Context, delivery chord.ReplicatedStateDelivery) <-chan error {
		if delivery.Kind == chord.DeliveryUpdate {
			status := "gone"
			if node, ok := decodeGraph(t, value).Tasks[id]; ok {
				status = node.State.Status
			}
			mu.Lock()
			statuses = append(statuses, status)
			mu.Unlock()
		}
		return nil
	})
	read := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(statuses)
	}
	must(t, o.h.Resume())
	<-reached
	eventually(t, func() bool { return len(read()) >= 1 })
	// A pause for what must not come: the memo commit's revision.
	flush()
	// Reservation changed the node; the memo commit did not.
	if got := read(); !reflect.DeepEqual(got, []string{"running"}) {
		t.Fatalf("updates = %v", got)
	}
	close(gate)
	waitDone(t, o.h, id)
	eventually(t, func() bool { return len(read()) >= 2 })
	if got := read(); !reflect.DeepEqual(got, []string{"running", "gone"}) {
		t.Fatalf("updates = %v", got)
	}
	last := first.Value()
	first.Dispose()
	second.Dispose()
	// No observer is left, so the mount was dropped: a new observer builds a
	// new revision.
	rebuilt, err := o.h.TaskGraph(bg)
	must(t, err)
	if sameMap(rebuilt.Value(), last) || !reflect.DeepEqual(rebuilt.Value(), last) {
		t.Fatalf("rebuilt %s, last %s", jsonOf(t, rebuilt.Value()), jsonOf(t, last))
	}
	rebuilt.Dispose()
}
