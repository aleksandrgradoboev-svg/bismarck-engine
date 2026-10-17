package durable

import (
	"cmp"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// The task graph view (pi packages/durable/src/harness/task-graph.ts): every
// live task of the Session, with its durable status and the conversations it
// owns (spec §9.5). pi keeps it in the harness; it needs only the Session and
// its Storage, so here it lives with the Session's other observation, and a
// Harness has it as a Session method.
//
// The graph is a chord value, as a document is, so it is observed the same
// way: {"tasks": {"<decimal ID>": node}}. DecodeTaskGraph reads it typed.

// TaskGraphWatch is a serialized exact-frame watch of the task graph.
type TaskGraphWatch = DocumentWatch

// TaskGraph is every live task, keyed by its ID.
type TaskGraph struct {
	Tasks map[TaskID]TaskGraphNode
}

// TaskGraphNode is one live task.
type TaskGraphNode struct {
	ID             TaskID         `json:"id"`
	Kind           string         `json:"kind"`
	ConversationID ConversationID `json:"conversationId"`
	// Owner is the owner task; nil for a conversation-owned task.
	Owner          *TaskID        `json:"owner,omitempty"`
	Background     bool           `json:"background"`
	AbortRequested bool           `json:"abortRequested"`
	State          TaskGraphState `json:"state"`
	// Conversations are the conversations the task owns, in ID order.
	Conversations []ConversationID `json:"conversations"`
}

// TaskGraphState is a live task's durable status without its checkpoint and
// outcome payloads: Phase for pending, running and waiting; On and Policy for
// waiting; Outcome, the held outcome's status, for completing.
type TaskGraphState struct {
	Status  string     `json:"status"`
	Phase   string     `json:"phase,omitempty"`
	On      []TaskID   `json:"on,omitempty"`
	Policy  JoinPolicy `json:"policy,omitempty"`
	Outcome string     `json:"outcome,omitempty"`
}

// DecodeTaskGraph reads a task graph value.
func DecodeTaskGraph(value map[string]any) (TaskGraph, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return TaskGraph{}, err
	}
	var decoded struct {
		Tasks map[string]TaskGraphNode `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return TaskGraph{}, err
	}
	graph := TaskGraph{Tasks: make(map[TaskID]TaskGraphNode, len(decoded.Tasks))}
	for _, node := range decoded.Tasks {
		graph.Tasks[node.ID] = node
	}
	return graph, nil
}

// TaskGraph attaches a disposable read-only chord state of the task graph.
func (s *Session) TaskGraph(ctx context.Context) (DocumentState, error) {
	var state DocumentState
	err := s.enqueue(true, func() error {
		var source *committedStateSource
		detach, err := s.taskGraphView().attach(ctx, func(value map[string]any, release func()) documentObserver {
			source = newCommittedStateSource(value, release)
			return source
		})
		if err != nil {
			return err
		}
		defer func() {
			if r := recover(); r != nil {
				detach()
				panic(r)
			}
		}()
		state = chord.AttachReplicatedState[map[string]any](source, chord.ReplicatedStateSourceOptions{OnError: s.report})
		return nil
	})
	return state, err
}

// WatchTaskGraph acquires a serialized exact-frame watch of the task graph.
// Cancelling ctx cancels acquisition, and afterwards the watch.
func (s *Session) WatchTaskGraph(ctx context.Context) (*TaskGraphWatch, error) {
	var watch *DocumentWatch
	err := s.enqueue(true, func() error {
		_, err := s.taskGraphView().attach(ctx, func(value map[string]any, release func()) documentObserver {
			watch = newDocumentWatch(value, release, s.report)
			return watch
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		watch.cancel()
		return nil, context.Cause(ctx)
	}
	if ctx.Done() != nil {
		watch.observeCancellation(ctx)
	}
	return watch, nil
}

// taskGraphView returns the Session's view, subscribing it at first use. It
// runs on the line, so no commit falls between the subscription and the
// first build.
func (s *Session) taskGraphView() *taskGraphView {
	s.taskGraphOnce.Do(func() {
		v := &taskGraphView{s: s}
		// A Session already closing rejects these; attach then finds it
		// unhealthy.
		_, _ = s.ObserveCommits(func(publication CommitPublication, ctx context.Context) { v.advance(publication, ctx) })
		_, _ = s.SubscribeClose(v.close)
		s.taskGraph = v
	})
	return s.taskGraph
}

// taskGraphView is the graph's mount: built on the line by its first
// observer and dropped with its last. It advances from the Session's commit
// publications, which are durable.
type taskGraphView struct {
	s      *Session
	mu     sync.Mutex
	mount  *taskGraphMount
	closed bool
}

type taskGraphMount struct {
	value     map[string]any
	observers []documentObserver
}

// attach registers an observer created from the current revision, on the
// line.
func (v *taskGraphView) attach(ctx context.Context, create func(value map[string]any, release func()) documentObserver) (func(), error) {
	if err := v.s.healthy(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	mount := v.mount
	v.mu.Unlock()
	if mount == nil {
		value, err := v.build(ctx)
		if err != nil {
			return nil, err
		}
		mount = &taskGraphMount{value: value}
	}
	var observer documentObserver
	detach := func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		for i, o := range mount.observers {
			if o == observer {
				mount.observers = slices.Delete(mount.observers, i, i+1)
				break
			}
		}
		if len(mount.observers) == 0 && v.mount == mount {
			v.mount = nil
		}
	}
	observer = create(mount.value, detach)
	// Close or cancellation may begin while the mount builds; register
	// nothing then.
	if err := v.s.healthy(); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, v.s.healthy()
	}
	v.mount = mount
	mount.observers = append(mount.observers, observer)
	return detach, nil
}

// liveStatuses are the statuses of the tasks the graph holds.
var liveStatuses = []string{TaskPending, TaskRunning, TaskWaiting, TaskCompleting}

const taskGraphPageSize = 256

func (v *taskGraphView) build(ctx context.Context) (map[string]any, error) {
	var records []TaskRecord
	for _, status := range liveStatuses {
		var cursor Cursor
		for {
			page, err := v.s.storage.ScanTasks(ctx, TaskQuery{Status: status}, taskGraphPageSize, cursor)
			if err != nil {
				return nil, err
			}
			records = append(records, page.Items...)
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
	}
	slices.SortFunc(records, func(a, b TaskRecord) int { return cmp.Compare(a.ID, b.ID) })
	tasks := map[string]any{}
	for _, record := range records {
		var owned []ConversationID
		var cursor Cursor
		id := record.ID
		for {
			page, err := v.s.storage.ScanConversations(ctx, ConversationQuery{OwnerTaskID: &id}, taskGraphPageSize, cursor)
			if err != nil {
				return nil, err
			}
			for _, conversation := range page.Items {
				owned = append(owned, conversation.ID)
			}
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		slices.Sort(owned)
		tasks[graphKey(record.ID)] = nodeValue(record, conversationValues(owned))
	}
	return map[string]any{"tasks": tasks}, nil
}

// advance derives the mount's operations from one publication, applies them,
// and hands the revision to every observer; one that panics is reported, and
// the others still get it.
func (v *taskGraphView) advance(publication CommitPublication, ctx context.Context) {
	v.mu.Lock()
	mount := v.mount
	if mount == nil {
		v.mu.Unlock()
		return
	}
	tasks := mount.value["tasks"].(map[string]any)
	var ops []delta.Op
	// Nodes this publication set or deleted, over the mount's value.
	changed := map[string]map[string]any{}
	node := func(key string) map[string]any {
		if n, ok := changed[key]; ok {
			return n
		}
		n, _ := tasks[key].(map[string]any)
		return n
	}
	for _, change := range publication.Changes {
		write, ok := change.(TaskWrite)
		if !ok {
			continue
		}
		record := write.Value
		key := graphKey(record.ID)
		previous := node(key)
		if record.State.Status == TaskTerminal {
			if previous == nil {
				continue
			}
			ops = append(ops, delta.Delete{Path: delta.Path{delta.Key("tasks"), delta.Key(key)}})
			changed[key] = nil
			continue
		}
		conversations := []any{}
		if previous != nil {
			conversations = previous["conversations"].([]any)
		}
		next := nodeValue(record, conversations)
		if previous != nil && reflect.DeepEqual(next, previous) {
			continue
		}
		ops = append(ops, delta.Set{Path: delta.Path{delta.Key("tasks"), delta.Key(key)}, Value: next})
		changed[key] = next
	}
	// After the tasks, so a conversation created with its owner task in one
	// commit finds the owner's node. Change order within a publication is
	// unspecified, so each owner's list is sorted again.
	created := map[string][]ConversationID{}
	var owners []string
	for _, change := range publication.Changes {
		write, ok := change.(ConversationWrite)
		if !ok || write.Value.Owner == nil {
			continue
		}
		key := graphKey(write.Value.Owner.TaskID)
		if node(key) == nil {
			continue
		}
		if _, seen := created[key]; !seen {
			owners = append(owners, key)
		}
		created[key] = append(created[key], write.Value.ID)
	}
	for _, key := range owners {
		var ids []ConversationID
		for _, id := range node(key)["conversations"].([]any) {
			ids = append(ids, ConversationID(id.(float64)))
		}
		ids = append(ids, created[key]...)
		slices.Sort(ids)
		ops = append(ops, delta.Set{Path: delta.Path{delta.Key("tasks"), delta.Key(key), delta.Key("conversations")}, Value: conversationValues(ids)})
	}
	if len(ops) == 0 {
		v.mu.Unlock()
		return
	}
	value, err := delta.ApplyImmutable(mount.value, ops)
	if err != nil {
		v.mu.Unlock()
		v.s.report(err)
		return
	}
	mount.value = value
	observers := slices.Clone(mount.observers)
	v.mu.Unlock()
	frame := context.WithoutCancel(ctx)
	for _, observer := range observers {
		contained(func() { observer.advance(frame, value, ops) }, v.s.report)
	}
}

// close ends every observer as the Session closes.
func (v *taskGraphView) close() {
	v.mu.Lock()
	v.closed = true
	var observers []documentObserver
	if v.mount != nil {
		observers = slices.Clone(v.mount.observers)
	}
	v.mount = nil
	v.mu.Unlock()
	failure := v.s.Failure()
	for _, observer := range observers {
		contained(func() { observer.closeSession(failure) }, v.s.report)
	}
}

func graphKey(id TaskID) string { return strconv.FormatInt(int64(id), 10) }

func conversationValues(ids []ConversationID) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = float64(id)
	}
	return out
}

func nodeValue(record TaskRecord, conversations []any) map[string]any {
	node := map[string]any{
		"id":             float64(record.ID),
		"kind":           record.Kind,
		"conversationId": float64(record.ConversationID),
		"background":     record.Background,
		"abortRequested": record.AbortRequested,
		"state":          stateValue(record.State),
		"conversations":  conversations,
	}
	if record.Owner != nil {
		node["owner"] = float64(*record.Owner)
	}
	return node
}

func stateValue(state TaskState) map[string]any {
	switch state.Status {
	case TaskPending, TaskRunning:
		return map[string]any{"status": state.Status, "phase": phaseOf(state.Checkpoint)}
	case TaskWaiting:
		on := make([]any, len(state.On))
		for i, id := range state.On {
			on[i] = float64(id)
		}
		return map[string]any{"status": TaskWaiting, "phase": phaseOf(state.Checkpoint), "on": on, "policy": string(state.Policy)}
	}
	// Terminal records never reach here: they leave the graph.
	outcome := ""
	if state.Outcome != nil {
		outcome = state.Outcome.Status
	}
	return map[string]any{"status": TaskCompleting, "outcome": outcome}
}

func phaseOf(checkpoint json.RawMessage) string {
	var head struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal(checkpoint, &head)
	return head.Phase
}
