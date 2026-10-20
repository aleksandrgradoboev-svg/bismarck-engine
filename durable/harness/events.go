package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Agent events (pi packages/durable/src/harness/events.ts, spec §9.4):
// experimental. One conversation's commits as batches of events shaped like
// the coding agent's session events, translated from the conversation view's
// publications. The tool and compaction events come with their slices of the
// port, with the pi.live slots they read.

// QueuedItem is one queued inbox item: its submission and how it is queued.
type QueuedItem struct {
	ID   durable.SubmissionID `json:"id"`
	Mode string               `json:"mode"`
}

// AgentEvent is one experimental agent event; its JSON carries its type
// under "type", as pi's does.
type AgentEvent interface {
	EventType() string
}

// SnapshotRun is the run of a snapshot: its inputs.
type SnapshotRun struct {
	Inputs []durable.SubmissionID `json:"inputs"`
}

// SnapshotEvent is the conversation at attachment, or after an overflow.
type SnapshotEvent struct {
	Entries []durable.EntryRecord `json:"entries"`
	Run     *SnapshotRun          `json:"run,omitempty"`
	// Generation is the current generation attempt: its in-flight partial,
	// retry backoff, or deferred poll.
	Generation *LiveGeneration `json:"generation,omitempty"`
	Inbox      []QueuedItem    `json:"inbox"`
	// Agent is pi.agent; its initial value when absent.
	Agent AgentState `json:"agent"`
	Usage UsageState `json:"usage"`
}

// RunStartEvent: a run starts for its inputs.
type RunStartEvent struct {
	Inputs []durable.SubmissionID `json:"inputs"`
}

// RunEndEvent: the run of these inputs ended.
type RunEndEvent struct {
	Inputs []durable.SubmissionID `json:"inputs"`
}

// TurnStartEvent: a generation takes the run.
type TurnStartEvent struct{}

// TurnEndEvent: a generation's outcome is committed.
type TurnEndEvent struct{}

// MessageStartEvent: a message starts, at its first committed partial or
// with its entry.
type MessageStartEvent struct {
	Message ai.Message `json:"message"`
}

// MessageUpdateEvent: the in-flight assistant message changed. Usage is the
// partial's current usage, as in the coding agent's JSON mode.
type MessageUpdateEvent struct {
	Usage   ai.Usage        `json:"usage"`
	Changes []MessageChange `json:"changes"`
}

// MessageEndEvent: a message ends with its entry.
type MessageEndEvent struct {
	Entry durable.EntryRecord `json:"entry"`
}

// InboxUpdateEvent: the queued items changed.
type InboxUpdateEvent struct {
	Items []QueuedItem `json:"items"`
}

// SubmissionEvent: a submission record changed.
type SubmissionEvent struct {
	Record durable.SubmissionRecord `json:"record"`
}

// AutoRetryStartEvent: a failed attempt waits for its retry at At (Unix
// milliseconds on the Harness clock).
type AutoRetryStartEvent struct {
	Attempt      int    `json:"attempt"`
	At           int64  `json:"at"`
	ErrorMessage string `json:"errorMessage"`
}

// AutoRetryEndEvent: the backoff of this attempt ended.
type AutoRetryEndEvent struct {
	Attempt int `json:"attempt"`
}

// DeferredPollEvent: a deferred response is polled next at PollAt.
type DeferredPollEvent struct {
	PollAt int64 `json:"pollAt"`
}

// EntryAppendedEvent: an entry without a model message was appended.
type EntryAppendedEvent struct {
	Entry durable.EntryRecord `json:"entry"`
}

// AgentChangedEvent: pi.agent changed; a retired one reads as its initial
// value.
type AgentChangedEvent struct {
	Agent AgentState `json:"agent"`
}

// UsageChangedEvent: pi.usage changed; a retired one reads as its initial
// value.
type UsageChangedEvent struct {
	Usage UsageState `json:"usage"`
}

// TaskFailedEvent: a task of the conversation faulted or was orphaned.
type TaskFailedEvent struct {
	TaskID  durable.TaskID `json:"taskId"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
}

func (SnapshotEvent) EventType() string       { return "snapshot" }
func (RunStartEvent) EventType() string       { return "run_start" }
func (RunEndEvent) EventType() string         { return "run_end" }
func (TurnStartEvent) EventType() string      { return "turn_start" }
func (TurnEndEvent) EventType() string        { return "turn_end" }
func (MessageStartEvent) EventType() string   { return "message_start" }
func (MessageUpdateEvent) EventType() string  { return "message_update" }
func (MessageEndEvent) EventType() string     { return "message_end" }
func (InboxUpdateEvent) EventType() string    { return "inbox_update" }
func (SubmissionEvent) EventType() string     { return "submission" }
func (AutoRetryStartEvent) EventType() string { return "auto_retry_start" }
func (AutoRetryEndEvent) EventType() string   { return "auto_retry_end" }
func (DeferredPollEvent) EventType() string   { return "deferred_poll" }
func (EntryAppendedEvent) EventType() string  { return "entry_appended" }
func (AgentChangedEvent) EventType() string   { return "agent_changed" }
func (UsageChangedEvent) EventType() string   { return "usage_changed" }
func (TaskFailedEvent) EventType() string     { return "task_failed" }

// typedJSON is the JSON of v with "type": kind first.
func typedJSON(kind string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	head, err := json.Marshal(kind)
	if err != nil {
		return nil, err
	}
	out := append([]byte(`{"type":`), head...)
	if string(raw) == "{}" {
		return append(out, '}'), nil
	}
	return append(append(out, ','), raw[1:]...), nil
}

func (e SnapshotEvent) MarshalJSON() ([]byte, error) {
	type plain SnapshotEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e RunStartEvent) MarshalJSON() ([]byte, error) {
	type plain RunStartEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e RunEndEvent) MarshalJSON() ([]byte, error) {
	type plain RunEndEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e TurnStartEvent) MarshalJSON() ([]byte, error) { return typedJSON(e.EventType(), struct{}{}) }
func (e TurnEndEvent) MarshalJSON() ([]byte, error)   { return typedJSON(e.EventType(), struct{}{}) }
func (e MessageStartEvent) MarshalJSON() ([]byte, error) {
	type plain MessageStartEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e MessageUpdateEvent) MarshalJSON() ([]byte, error) {
	type plain MessageUpdateEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e MessageEndEvent) MarshalJSON() ([]byte, error) {
	type plain MessageEndEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e InboxUpdateEvent) MarshalJSON() ([]byte, error) {
	type plain InboxUpdateEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e SubmissionEvent) MarshalJSON() ([]byte, error) {
	type plain SubmissionEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e AutoRetryStartEvent) MarshalJSON() ([]byte, error) {
	type plain AutoRetryStartEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e AutoRetryEndEvent) MarshalJSON() ([]byte, error) {
	type plain AutoRetryEndEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e DeferredPollEvent) MarshalJSON() ([]byte, error) {
	type plain DeferredPollEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e EntryAppendedEvent) MarshalJSON() ([]byte, error) {
	type plain EntryAppendedEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e AgentChangedEvent) MarshalJSON() ([]byte, error) {
	type plain AgentChangedEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e UsageChangedEvent) MarshalJSON() ([]byte, error) {
	type plain UsageChangedEvent
	return typedJSON(e.EventType(), plain(e))
}
func (e TaskFailedEvent) MarshalJSON() ([]byte, error) {
	type plain TaskFailedEvent
	return typedJSON(e.EventType(), plain(e))
}

// MessageChange is one change to the in-flight assistant message, relative
// to that message; its JSON carries its type under "type".
type MessageChange interface {
	ChangeType() string
}

// BlockStartChange inserts Block at ContentIndex: "text_start",
// "thinking_start" or "toolcall_start".
type BlockStartChange struct {
	Kind         string
	ContentIndex int
	Block        ai.Content
}

// TextDeltaChange appends Delta to the text ("text_delta") or thinking
// ("thinking_delta") of the block at ContentIndex.
type TextDeltaChange struct {
	Kind         string
	ContentIndex int
	Delta        string
}

// ToolCallDeltaChange appends Delta to the string at Path in the arguments
// of the tool call at ContentIndex.
type ToolCallDeltaChange struct {
	ContentIndex int
	// Path holds object keys (string) and array indices (int).
	Path  []any
	Delta string
}

// BlockChange replaces the block at ContentIndex.
type BlockChange struct {
	ContentIndex int
	Block        ai.Content
}

// MessageReplaceChange replaces the whole message.
type MessageReplaceChange struct {
	Message ai.AssistantMessage
}

func (c BlockStartChange) ChangeType() string  { return c.Kind }
func (c TextDeltaChange) ChangeType() string   { return c.Kind }
func (ToolCallDeltaChange) ChangeType() string { return "toolcall_delta" }
func (BlockChange) ChangeType() string         { return "block" }
func (MessageReplaceChange) ChangeType() string {
	return "message"
}

// blockJSON is a content block with its "type", as a content list encodes
// it.
func blockJSON(block ai.Content) (json.RawMessage, error) {
	raw, err := json.Marshal(ai.ContentList{block})
	if err != nil {
		return nil, err
	}
	return raw[1 : len(raw)-1], nil
}

func (c BlockStartChange) MarshalJSON() ([]byte, error) {
	block, err := blockJSON(c.Block)
	if err != nil {
		return nil, err
	}
	return typedJSON(c.Kind, struct {
		ContentIndex int             `json:"contentIndex"`
		Block        json.RawMessage `json:"block"`
	}{c.ContentIndex, block})
}

func (c TextDeltaChange) MarshalJSON() ([]byte, error) {
	return typedJSON(c.Kind, struct {
		ContentIndex int    `json:"contentIndex"`
		Delta        string `json:"delta"`
	}{c.ContentIndex, c.Delta})
}

func (c ToolCallDeltaChange) MarshalJSON() ([]byte, error) {
	return typedJSON(c.ChangeType(), struct {
		ContentIndex int    `json:"contentIndex"`
		Path         []any  `json:"path"`
		Delta        string `json:"delta"`
	}{c.ContentIndex, c.Path, c.Delta})
}

func (c BlockChange) MarshalJSON() ([]byte, error) {
	block, err := blockJSON(c.Block)
	if err != nil {
		return nil, err
	}
	return typedJSON(c.ChangeType(), struct {
		ContentIndex int             `json:"contentIndex"`
		Block        json.RawMessage `json:"block"`
	}{c.ContentIndex, block})
}

func (c MessageReplaceChange) MarshalJSON() ([]byte, error) {
	return typedJSON(c.ChangeType(), struct {
		Message ai.AssistantMessage `json:"message"`
	}{c.Message})
}

// AgentEventStream is the serialized stream of one conversation's event
// batches, one per commit that causes events.
type AgentEventStream struct {
	// Snapshot is the snapshot event at attachment.
	Snapshot SnapshotEvent
	watch    *durable.CommittedWatch[[]AgentEvent]
}

// Start installs the sole listener of the batches; an error ends the stream
// with "listener_error". It never calls the listener inline.
func (s *AgentEventStream) Start(listener func(ctx context.Context, events []AgentEvent) error) error {
	return s.watch.Start(func(ctx context.Context, events []AgentEvent, _ []delta.Op) error { return listener(ctx, events) })
}

// Stop idempotently stops the stream and returns its terminal result.
func (s *AgentEventStream) Stop() durable.WatchEnd { return s.watch.Stop() }

// Closed is closed when the stream ends.
func (s *AgentEventStream) Closed() <-chan struct{} { return s.watch.Closed() }

// End is the terminal result; the zero WatchEnd until Closed is closed.
func (s *AgentEventStream) End() durable.WatchEnd { return s.watch.End() }

// WatchEvents attaches to the conversation's agent events (experimental,
// spec §9.4). The snapshot and the registration for later commits are
// captured atomically on the Session line; an overflow replaces undelivered
// batches with one snapshot of the newest view. Cancelling ctx cancels
// acquisition, and afterwards the stream.
func (c *Conversation) WatchEvents(ctx context.Context) (*AgentEventStream, error) {
	return c.h.views.watchEvents(ctx, c.ID)
}

// eventsObserver ends an event stream with the Session; its batches come
// from the publication hook, not from view revisions.
type eventsObserver struct {
	watch *durable.CommittedWatch[[]AgentEvent]
}

func (eventsObserver) Advance(context.Context, map[string]any, []delta.Op) {}
func (o eventsObserver) CloseSession(failure error)                        { o.watch.CloseSession(failure) }

func (v *conversationViews) watchEvents(ctx context.Context, id durable.ConversationID) (*AgentEventStream, error) {
	stream := &AgentEventStream{}
	var failed error
	err := v.attach(ctx, id, func(initial map[string]any, release func()) *viewObserver {
		// Generations whose held outcome already ended their turn, read on
		// the line with the snapshot.
		held := map[durable.TaskID]bool{}
		query := durable.TaskQuery{ConversationID: &id, Kind: GenerationTask.Name(), Status: durable.TaskCompleting}
		var cursor durable.Cursor
		for {
			page, err := v.h.Session.Storage().ScanTasks(ctx, query, 100, cursor)
			if err != nil {
				failed = err
				break
			}
			for _, record := range page.Items {
				held[record.ID] = true
			}
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		var mu sync.Mutex
		current := initial
		snapshot, err := eventSnapshot(initial)
		if err != nil && failed == nil {
			failed = err
		}
		stream.Snapshot = snapshot
		// Batches are the watch's values; an overflow delivers a snapshot of
		// the newest view instead.
		stream.watch = durable.NewCommittedWatchOf[[]AgentEvent](nil, release, v.report, durable.CommittedWatchOptions[[]AgentEvent]{
			Replace: func() []AgentEvent {
				mu.Lock()
				view := current
				mu.Unlock()
				snapshot, err := eventSnapshot(view)
				if err != nil {
					v.report(err)
				}
				return []AgentEvent{snapshot}
			},
		})
		return &viewObserver{
			observer: eventsObserver{watch: stream.watch},
			publication: func(frame context.Context, before, after map[string]any, ops []delta.Op, publication durable.CommitPublication) {
				mu.Lock()
				current = after
				mu.Unlock()
				events, err := translateEvents(id, before, after, ops, publication, held)
				if err != nil {
					v.report(err)
				}
				if len(events) > 0 {
					stream.watch.Advance(frame, events, nil)
				}
			},
		}
	})
	if err == nil {
		err = failed
	}
	if err != nil {
		if stream.watch != nil {
			stream.watch.Cancel()
		}
		return nil, err
	}
	// Like a watch, the acquisition context governs the stream's lifetime.
	if ctx.Err() != nil {
		stream.watch.Cancel()
		return nil, context.Cause(ctx)
	}
	if ctx.Done() != nil {
		stream.watch.ObserveCancellation(ctx)
	}
	return stream, nil
}

// eventParts are the parts of a view revision the events read: the raw
// built-in documents, nil when absent.
type eventParts struct {
	live, inbox, agent, usage map[string]any
}

func partsOf(view map[string]any) eventParts {
	docs, _ := view["docs"].(map[string]any)
	doc := func(kind string) map[string]any {
		value, _ := docs[kind].(map[string]any)
		return value
	}
	return eventParts{live: doc(LiveDoc.Definition().Kind), inbox: doc(InboxDoc.Definition().Kind), agent: doc(AgentDoc.Definition().Kind), usage: doc(UsageDoc.Definition().Kind)}
}

// sameValue reports one immutable value: the view shares what a commit left
// untouched, as pi compares by reference.
func sameValue(a, b map[string]any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// rawAt walks object keys down a raw value; nil when absent.
func rawAt(value map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value == nil {
			return nil
		}
		value, _ = value[key].(map[string]any)
	}
	return value
}

// liveParts is the typed part of pi.live the events read.
type liveParts struct {
	Run        *LiveRun        `json:"run,omitempty"`
	Generation *LiveGeneration `json:"generation,omitempty"`
}

func decodeLiveParts(live map[string]any) (liveParts, error) {
	var parts liveParts
	return parts, fromValue(live, &parts)
}

func queuedOf(inbox map[string]any) []QueuedItem {
	items, _ := inbox["items"].([]any)
	out := make([]QueuedItem, 0, len(items))
	for _, item := range items {
		fields, _ := item.(map[string]any)
		id, _ := fields["id"].(float64)
		mode, _ := fields["mode"].(string)
		out = append(out, QueuedItem{ID: durable.SubmissionID(id), Mode: mode})
	}
	return out
}

// agentOf reads pi.agent; a retired or absent one is its initial value.
func agentOf(value map[string]any) (AgentState, error) {
	if value == nil {
		value = AgentDoc.Definition().Initial(nil)
	}
	var agent AgentState
	return agent, fromValue(value, &agent)
}

// usageOf reads pi.usage; a retired or absent one is its initial value.
func usageOf(value map[string]any) (UsageState, error) {
	if value == nil {
		value = UsageDoc.Definition().Initial(nil)
	}
	var usage UsageState
	return usage, fromValue(value, &usage)
}

func eventSnapshot(view map[string]any) (SnapshotEvent, error) {
	parts := partsOf(view)
	var decoded ConversationView
	if err := fromValue(map[string]any{"entries": view["entries"]}, &decoded); err != nil {
		return SnapshotEvent{}, err
	}
	live, err := decodeLiveParts(parts.live)
	if err != nil {
		return SnapshotEvent{}, err
	}
	agent, err := agentOf(parts.agent)
	if err != nil {
		return SnapshotEvent{}, err
	}
	usage, err := usageOf(parts.usage)
	if err != nil {
		return SnapshotEvent{}, err
	}
	snapshot := SnapshotEvent{Entries: decoded.Entries, Generation: live.Generation, Inbox: queuedOf(parts.inbox), Agent: agent, Usage: usage}
	if snapshot.Entries == nil {
		snapshot.Entries = []durable.EntryRecord{}
	}
	if live.Run != nil {
		snapshot.Run = &SnapshotRun{Inputs: live.Run.Inputs}
	}
	return snapshot, nil
}

// firstInput is the first input of a run; runs are told apart by it.
func firstInput(run *LiveRun) (durable.SubmissionID, bool) {
	if run == nil || len(run.Inputs) == 0 {
		return 0, false
	}
	return run.Inputs[0], true
}

// translateEvents is every event one publication causes, in the order of
// spec §9.4. held tracks the generations whose turn ended at a completing
// hold.
func translateEvents(id durable.ConversationID, before, after map[string]any, viewOps []delta.Op, publication durable.CommitPublication, held map[durable.TaskID]bool) ([]AgentEvent, error) {
	var entries []durable.EntryRecord
	var taskOrder []durable.TaskID
	tasks := map[durable.TaskID]durable.TaskRecord{}
	var submissions []durable.SubmissionRecord
	for _, change := range publication.Changes {
		switch c := change.(type) {
		case durable.EntryWrite:
			if c.Value.ConversationID == id {
				entries = append(entries, c.Value)
			}
		case durable.TaskWrite:
			if c.Value.ConversationID == id {
				if _, seen := tasks[c.Value.ID]; !seen {
					taskOrder = append(taskOrder, c.Value.ID)
				}
				tasks[c.Value.ID] = c.Value
			}
		case durable.SubmissionWrite:
			if c.Value.ConversationID == id {
				submissions = append(submissions, c.Value)
			}
		}
	}
	if len(viewOps) == 0 && len(entries) == 0 && len(tasks) == 0 && len(submissions) == 0 {
		return nil, nil
	}
	// Entries are appended in ID order; submission records are published in
	// the order the commit first touched them.
	slices.SortStableFunc(submissions, func(a, b durable.SubmissionRecord) int { return int(a.ID - b.ID) })
	was, now := partsOf(before), partsOf(after)
	wasLive, err := decodeLiveParts(was.live)
	if err != nil {
		return nil, err
	}
	nowLive, err := decodeLiveParts(now.live)
	if err != nil {
		return nil, err
	}
	var events []AgentEvent

	// Progress: the in-flight message, retry and deferred state.
	partialBefore := rawAt(was.live, "generation", "message")
	partial := rawAt(now.live, "generation", "message")
	if partial != nil {
		message := *nowLive.Generation.Message
		if partialBefore == nil {
			events = append(events, MessageStartEvent{Message: message})
		} else if !sameValue(partial, partialBefore) {
			changes, err := messageChanges(viewOps, message)
			if err != nil {
				return nil, err
			}
			events = append(events, MessageUpdateEvent{Usage: message.Usage, Changes: changes})
		}
	}
	generation, generationBefore := nowLive.Generation, wasLive.Generation
	if generation != nil && generation.Retry != nil && (generationBefore == nil || generationBefore.Retry == nil) {
		events = append(events, AutoRetryStartEvent{Attempt: generation.Attempt, At: generation.Retry.At, ErrorMessage: generation.Retry.Error})
	}
	if generationBefore != nil && generationBefore.Retry != nil && (generation == nil || generation.Retry == nil) {
		events = append(events, AutoRetryEndEvent{Attempt: generationBefore.Attempt})
	}
	if generation != nil && generation.Deferred != nil && (generationBefore == nil || generationBefore.Deferred == nil || generation.Deferred.PollAt != generationBefore.Deferred.PollAt) {
		events = append(events, DeferredPollEvent{PollAt: generation.Deferred.PollAt})
	}

	// Entries in append order.
	assistantAppended := false
	for _, entry := range entries {
		if len(entry.Model) == 0 {
			events = append(events, EntryAppendedEvent{Entry: entry})
			continue
		}
		message := entry.Model[0]
		_, assistant := message.(ai.AssistantMessage)
		// A streamed answer already started with its first partial.
		streamed := assistant && partialBefore != nil && !assistantAppended
		if assistant {
			assistantAppended = true
		}
		if !streamed {
			events = append(events, MessageStartEvent{Message: message})
		}
		events = append(events, MessageEndEvent{Entry: entry})
	}

	// Task failures, then turn and run ends. A generation's turn ends when
	// its outcome is committed: at a completing hold or at terminal,
	// whichever comes first, so a successor created at the hold starts after
	// it.
	turnEnded := false
	for _, taskID := range taskOrder {
		task := tasks[taskID]
		generationTask := task.Kind == GenerationTask.Name()
		if generationTask && task.State.Status == durable.TaskCompleting && !held[task.ID] {
			held[task.ID] = true
			turnEnded = true
		}
		if task.State.Status != durable.TaskTerminal {
			continue
		}
		if generationTask {
			if held[task.ID] {
				delete(held, task.ID)
			} else {
				turnEnded = true
			}
		}
		if outcome := task.State.Outcome; outcome != nil && (outcome.Status == durable.OutcomeFaulted || outcome.Status == durable.OutcomeOrphaned) {
			message := outcome.Reason
			if outcome.Status == durable.OutcomeFaulted && outcome.Error != nil {
				message = outcome.Error.Message
			}
			events = append(events, TaskFailedEvent{TaskID: task.ID, Kind: task.Kind, Message: message})
		}
	}
	if turnEnded {
		events = append(events, TurnEndEvent{})
	}
	run, runBefore := nowLive.Run, wasLive.Run
	first, hasFirst := firstInput(run)
	firstBefore, hadFirst := firstInput(runBefore)
	runChanged := hasFirst != hadFirst || first != firstBefore
	if runBefore != nil && runChanged {
		events = append(events, RunEndEvent{Inputs: runBefore.Inputs})
	}

	// Submissions, document state, then what began.
	for _, record := range submissions {
		events = append(events, SubmissionEvent{Record: record})
	}
	if !sameValue(now.inbox, was.inbox) {
		events = append(events, InboxUpdateEvent{Items: queuedOf(now.inbox)})
	}
	// A retired document reads as its initial value, as in a snapshot.
	if !sameValue(now.agent, was.agent) {
		agent, err := agentOf(now.agent)
		if err != nil {
			return nil, err
		}
		events = append(events, AgentChangedEvent{Agent: agent})
	}
	if !sameValue(now.usage, was.usage) {
		usage, err := usageOf(now.usage)
		if err != nil {
			return nil, err
		}
		events = append(events, UsageChangedEvent{Usage: usage})
	}
	if run != nil && runChanged {
		events = append(events, RunStartEvent{Inputs: run.Inputs})
	}
	if run != nil && (runBefore == nil || run.TaskID != runBefore.TaskID) {
		if task, ok := tasks[run.TaskID]; ok && task.Kind == GenerationTask.Name() {
			events = append(events, TurnStartEvent{})
		}
	}
	return events, nil
}

var partialPath = delta.Path{delta.Key("docs"), delta.Key(LiveDoc.Definition().Kind), delta.Key("generation"), delta.Key("message")}

// opPath is an operation's path; a root replacement has none.
func opPath(op delta.Op) (delta.Path, bool) {
	switch o := op.(type) {
	case delta.Set:
		return o.Path, true
	case delta.Delete:
		return o.Path, true
	case delta.Append:
		return o.Path, true
	case delta.Truncate:
		return o.Path, true
	case delta.Splice:
		return o.Path, true
	case delta.Permute:
		return o.Path, true
	}
	return nil, false
}

func hasPrefix(path, prefix delta.Path) bool {
	return len(prefix) <= len(path) && slices.Equal(path[:len(prefix)], prefix)
}

// decodeBlock reads one raw content block.
func decodeBlock(item any) (ai.Content, error) {
	raw, err := json.Marshal([]any{item})
	if err != nil {
		return nil, err
	}
	var blocks ai.ContentList
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks[0], nil
}

// messageChanges translates the view operations on the in-flight message
// into message changes (spec §9.4).
func messageChanges(viewOps []delta.Op, message ai.AssistantMessage) ([]MessageChange, error) {
	whole := []MessageChange{MessageReplaceChange{Message: message}}
	var changes []MessageChange
	// A block sent whole already holds every later change to it in this
	// batch.
	sent := map[int]bool{}
	for _, op := range viewOps {
		path, ok := opPath(op)
		if !ok {
			// View operations never replace the root.
			continue
		}
		if !hasPrefix(path, partialPath) {
			// The whole message or generation was replaced.
			if hasPrefix(partialPath, path) {
				return whole, nil
			}
			continue
		}
		rest := path[len(partialPath):]
		if len(rest) == 0 || rest[0] != delta.Key("content") {
			if len(rest) > 0 && rest[0] == delta.Key("usage") {
				continue
			}
			return whole, nil
		}
		if len(rest) == 1 {
			splice, ok := op.(delta.Splice)
			if !ok || splice.Remove != 0 {
				return whole, nil
			}
			for offset, item := range splice.Items {
				block, err := decodeBlock(item)
				if err != nil {
					return nil, err
				}
				kind := "toolcall_start"
				switch block.(type) {
				case ai.TextContent:
					kind = "text_start"
				case ai.ThinkingContent:
					kind = "thinking_start"
				}
				changes = append(changes, BlockStartChange{Kind: kind, ContentIndex: splice.Index + offset, Block: block})
			}
			continue
		}
		index, ok := rest[1].(delta.Index)
		if !ok {
			return whole, nil
		}
		contentIndex := int(index)
		if sent[contentIndex] {
			continue
		}
		var field delta.Seg
		if len(rest) > 2 {
			field = rest[2]
		}
		appended, isAppend := op.(delta.Append)
		switch {
		case isAppend && len(rest) == 3 && (field == delta.Key("text") || field == delta.Key("thinking")):
			kind := "text_delta"
			if field == delta.Key("thinking") {
				kind = "thinking_delta"
			}
			changes = append(changes, TextDeltaChange{Kind: kind, ContentIndex: contentIndex, Delta: appended.Text})
		case isAppend && field == delta.Key("arguments"):
			argumentPath := make([]any, 0, len(rest)-3)
			for _, seg := range rest[3:] {
				switch s := seg.(type) {
				case delta.Key:
					argumentPath = append(argumentPath, string(s))
				case delta.Index:
					argumentPath = append(argumentPath, int(s))
				}
			}
			changes = append(changes, ToolCallDeltaChange{ContentIndex: contentIndex, Path: argumentPath, Delta: appended.Text})
		default:
			sent[contentIndex] = true
			if contentIndex >= len(message.Content) {
				return whole, nil
			}
			changes = append(changes, BlockChange{ContentIndex: contentIndex, Block: message.Content[contentIndex]})
		}
	}
	if changes == nil {
		changes = []MessageChange{}
	}
	return changes, nil
}
