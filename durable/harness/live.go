package harness

import (
	"encoding/json"
	"errors"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// LiveRun is run control in pi.live: the task that settles the run's inputs,
// and those inputs; present exactly while the conversation is busy.
type LiveRun struct {
	TaskID durable.TaskID         `json:"taskId"`
	Inputs []durable.SubmissionID `json:"inputs"`
}

// LiveGeneration is the presentation of the current generation attempt.
type LiveGeneration struct {
	Attempt int `json:"attempt"`
	// Message is the committed throttled partial of the in-flight response.
	Message *ai.AssistantMessage `json:"message,omitempty"`
	// Retry is the durable backoff before the next attempt.
	Retry *LiveRetry `json:"retry,omitempty"`
	// Deferred is a provider-side deferred response being polled.
	Deferred *LiveDeferred `json:"deferred,omitempty"`
}

// LiveRetry is a durable backoff: when the next attempt starts (Unix
// milliseconds on the Harness clock) and the error that caused it.
type LiveRetry struct {
	At    int64  `json:"at"`
	Error string `json:"error"`
}

// LiveDeferred is when a deferred response is polled next (Unix
// milliseconds on the Harness clock).
type LiveDeferred struct {
	PollAt int64 `json:"pollAt"`
}

// Slot statuses.
const (
	slotPending = "pending"
	slotRunning = "running"
	slotDone    = "done"
)

// ToolSlot is the presentation of one tool call of the current round in
// pi.live tools, in call order, from the tool-calling answer until the
// generation's tools phase ends it. Read only: the Harness writes slots
// field by field, so a running output commits as an append or a trim.
type ToolSlot struct {
	CallID string `json:"callId"`
	Name   string `json:"name"`
	// Status is "pending", "running" or "done".
	Status string `json:"status"`
	SlotProgress
	// TaskID is absent for a call not started yet (sequential round) and
	// for a call its request did not offer, which starts done with the
	// Entry the generation wrote.
	TaskID *durable.TaskID `json:"taskId,omitempty"`
	// Entry is the result entry once done; absent when the tool task
	// faulted or was orphaned.
	Entry *durable.EntryID `json:"entry,omitempty"`
}

// SlotProgress is what a running tool call publishes, shared by
// model-issued and nested calls.
type SlotProgress struct {
	// Output is the retained running output, and DroppedBytes and
	// DroppedLines what the bounds dropped.
	Output       *string `json:"output,omitempty"`
	DroppedBytes int     `json:"droppedBytes,omitempty"`
	DroppedLines int     `json:"droppedLines,omitempty"`
	// Details is the last Details value.
	Details     any              `json:"details,omitempty"`
	Diagnostics []ToolDiagnostic `json:"diagnostics,omitempty"`
}

// NestedToolSlot is the presentation of one nested call, a call a running
// tool made through ExecuteTool, in pi.live nestedTools. CallID is
// "<parent call ID>/<key>"; slots are found by task ID.
type NestedToolSlot struct {
	CallID       string         `json:"callId"`
	ParentCallID string         `json:"parentCallId"`
	ParentTaskID durable.TaskID `json:"parentTaskId"`
	Name         string         `json:"name"`
	TaskID       durable.TaskID `json:"taskId"`
	// Arguments are what the call was made with, replaced by the arguments
	// it runs with, after repair, BeforeTool and coercion, once it starts
	// executing.
	Arguments map[string]any `json:"arguments"`
	// Status is "pending", "running" or "done".
	Status string `json:"status"`
	SlotProgress
	// Summary is, once done, how the call ended, for a status line; the
	// result itself is in the caller's NestedCallDoc.
	Summary *NestedToolSummary `json:"summary,omitempty"`
}

// NestedToolSummary is how a nested call ended: an error flag, its
// execution time, its spend, and up to 500 characters of error text.
type NestedToolSummary struct {
	IsError    bool      `json:"isError"`
	DurationMs *int64    `json:"durationMs,omitempty"`
	Usage      *ai.Usage `json:"usage,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// toolKind is the built-in tool task's kind.
const toolKind = "pi.tool"

// toolSlotDraft is the slot of tool task taskID, model-issued or nested, if
// pi.live still lists it. Nested slots are found by binary search: a tool may
// make thousands of nested calls, each of which looks up its slot in several
// commits, and the list holds them in ascending task ID.
func toolSlotDraft(live *delta.Draft, taskID durable.TaskID) *delta.Draft {
	if tools := live.At("tools"); tools != nil {
		for i := range tools.Len() {
			slot := tools.At(i)
			if slot == nil {
				continue
			}
			if id, ok := slot.Get("taskId"); ok && id == float64(taskID) {
				return slot
			}
		}
	}
	nested := live.At("nestedTools")
	if nested == nil {
		return nil
	}
	low, high := 0, nested.Len()-1
	for low <= high {
		middle := (low + high) / 2
		slot := nested.At(middle)
		id, _ := slot.Get("taskId")
		candidate, _ := id.(float64)
		switch {
		case candidate == float64(taskID):
			return slot
		case candidate < float64(taskID):
			low = middle + 1
		default:
			high = middle - 1
		}
	}
	return nil
}

// isNestedSlot reports whether a slot draft is a nested call's.
func isNestedSlot(slot *delta.Draft) bool {
	return slot.Has("parentTaskId")
}

// removeNestedSlots removes the nested slots below tool task taskID,
// transitively; the list holds parents before their children.
func removeNestedSlots(live *delta.Draft, taskID durable.TaskID) error {
	nested := live.At("nestedTools")
	if nested == nil {
		return nil
	}
	removed := map[float64]bool{float64(taskID): true}
	below := make([]bool, nested.Len())
	for i := range nested.Len() {
		slot := nested.At(i)
		parent, _ := slot.Get("parentTaskId")
		id, _ := slot.Get("taskId")
		if p, _ := parent.(float64); removed[p] {
			below[i] = true
			if task, ok := id.(float64); ok {
				removed[task] = true
			}
		}
	}
	for i := len(below) - 1; i >= 0; i-- {
		if below[i] {
			if _, err := nested.Splice(i, 1); err != nil {
				return err
			}
		}
	}
	if nested.Len() == 0 {
		return live.Delete("nestedTools")
	}
	return nil
}

// finishSlot marks a slot done and clears its running output; the caller
// sets a model-issued call's result entry. Neither is set when the task
// faulted or was orphaned.
func finishSlot(slot *delta.Draft) error {
	if err := slot.Set("status", slotDone); err != nil {
		return err
	}
	return clearProgress(slot)
}

// clearProgress removes what a tool published while running; its result
// entry or a rerun replaces it.
func clearProgress(slot *delta.Draft) error {
	for _, key := range []string{"output", "droppedBytes", "droppedLines", "details", "diagnostics"} {
		if err := slot.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// runTaskKinds are the built-in task kinds that can own pi.live run.
var runTaskKinds = map[string]bool{"pi.generation": true}

// liveDraft returns the pi.live draft of a conversation.
func liveDraft(tx *durable.Tx, conversationID durable.ConversationID) (*delta.Draft, error) {
	return tx.Doc(LiveDoc.Conversation(conversationID))
}

// liveRun returns the run of a pi.live draft, or nil while idle.
func liveRun(live *delta.Draft) (*LiveRun, error) {
	var run LiveRun
	found, err := draftField(live, "run", &run)
	if err != nil || !found {
		return nil, err
	}
	return &run, nil
}

// draftField decodes the member key of an object draft into out; false when
// absent.
func draftField(d *delta.Draft, key string, out any) (bool, error) {
	value, ok := d.Get(key)
	if !ok {
		return false, nil
	}
	if child, isDraft := value.(*delta.Draft); isDraft {
		snapshot, err := child.Snapshot()
		if err != nil {
			return false, err
		}
		value = snapshot
	}
	return true, decodeValue(value, out)
}

// setDraftField sets the member key of an object draft to v's JSON value.
func setDraftField(d *delta.Draft, key string, v any) error {
	value, err := toValue(v)
	if err != nil {
		return err
	}
	return d.Set(key, value)
}

// startRun starts a run for inputs, placed input submissions: a new
// generation takes pi.live run.
func startRun(tx *durable.Tx, conversationID durable.ConversationID, live *delta.Draft, inputs []durable.SubmissionID) error {
	// The run continues its first input's admission trace and links the
	// others'.
	taskID, err := createGeneration(tx, conversationID, inputs)
	if err != nil {
		return err
	}
	return setDraftField(live, "run", LiveRun{TaskID: taskID, Inputs: inputs})
}

// createGeneration creates a generation owned by its conversation; with
// inputs, its telemetry comes from their submissions, otherwise from the
// commit's span.
func createGeneration(tx *durable.Tx, conversationID durable.ConversationID, inputs []durable.SubmissionID) (durable.TaskID, error) {
	return CreateTask(tx, GenerationTask, GenerationInput{}, durable.TaskOptions{
		Ownership:        durable.TaskOwnership{Kind: durable.OwnershipConversation},
		ConversationID:   conversationID,
		TraceSubmissions: inputs,
	})
}

// handOver hands run control from one task to another; the run's inputs move
// with it.
func handOver(live *delta.Draft, from, to durable.TaskID) error {
	run, err := liveRun(live)
	if err != nil || run == nil || run.TaskID != from {
		return err
	}
	// One field write, as pi's `live.run.taskId = to`.
	return live.At("run").Set("taskId", float64(to))
}

// endRun ends the run owned by taskID: it settles each of its inputs and
// removes run. It always removes generation, tools, and nestedTools, whose
// presentation belongs to the ending run.
func endRun(tx *durable.Tx, live *delta.Draft, taskID durable.TaskID, settlement durable.SubmissionSettlement) error {
	run, err := liveRun(live)
	if err != nil {
		return err
	}
	if run != nil && run.TaskID == taskID {
		for _, id := range run.Inputs {
			if err := tx.SettleSubmission(id, settlement); err != nil {
				return err
			}
		}
		if err := live.Delete("run"); err != nil {
			return err
		}
	}
	for _, key := range []string{"generation", "tools", "nestedTools"} {
		if err := live.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// convertPartial appends a committed partial left by an interrupted, aborted,
// faulted, or orphaned attempt as an aborted assistant entry; the caller
// replaces or removes generation.
func convertPartial(tx *durable.Tx, live *delta.Draft, conversationID durable.ConversationID) error {
	var generation LiveGeneration
	found, err := draftField(live, "generation", &generation)
	if err != nil || !found || generation.Message == nil {
		return err
	}
	message := *generation.Message.Clone()
	message.StopReason = ai.StopAborted
	_, err = appendAssistant(tx, conversationID, message)
	return err
}

// appendAssistant appends a provider result and adds its usage to pi.usage
// in the same commit.
// REMINDER: every built-in writer of assistant entries goes through here, so
// the usage ledger stays complete.
func appendAssistant(tx *durable.Tx, conversationID durable.ConversationID, message ai.AssistantMessage) (durable.EntryRecord, error) {
	if err := recordUsage(tx, conversationID, "models", message.Provider+"/"+message.Model, message.Usage); err != nil {
		return durable.EntryRecord{}, err
	}
	return tx.AppendEntry(conversationID, durable.EntryDraft{Kind: durable.AssistantEntry.Kind, Model: []ai.Message{message}})
}

// recordUsage adds usage to one bucket ("models" or "tools") of the
// conversation's pi.usage, in the commit that records the response.
func recordUsage(tx *durable.Tx, conversationID durable.ConversationID, bucket, key string, usage ai.Usage) error {
	doc, err := tx.Doc(UsageDoc.Conversation(conversationID))
	if err != nil {
		return err
	}
	totals := doc.At(bucket)
	if totals == nil {
		return errors.New("pi.usage has no " + bucket)
	}
	var total ai.Usage
	found, err := draftField(totals, key, &total)
	if err != nil {
		return err
	}
	if found {
		addUsage(&total, usage)
		usage = total
	}
	return setDraftField(totals, key, usage)
}

// settleSchedulerOutcome is the Harness cleanup for a terminal outcome the
// scheduler writes itself (faulted or orphaned): a run task ends its run; a
// tool task's slot is marked done without an entry, and context derivation
// synthesizes the missing result, and the nested slots below it are removed.
// Other kinds are ignored, so it never
// creates pi.live elsewhere; compaction tasks join with their stage of the
// port.
// REMINDER: a committed generation partial becomes an aborted assistant entry
// here, exactly as in the generation abort handler, so the transcript keeps
// what the model produced and pi.usage counts its spend.
func settleSchedulerOutcome(tx *durable.Tx, record durable.TaskRecord, outcome durable.TaskOutcome) error {
	if record.Kind == toolKind {
		live, err := liveDraft(tx, record.ConversationID)
		if err != nil {
			return err
		}
		if slot := toolSlotDraft(live, record.ID); slot != nil {
			if err := finishSlot(slot); err != nil {
				return err
			}
		}
		// The nested calls it made settle with it.
		return removeNestedSlots(live, record.ID)
	}
	if !runTaskKinds[record.Kind] {
		return nil
	}
	live, err := liveDraft(tx, record.ConversationID)
	if err != nil {
		return err
	}
	run, err := liveRun(live)
	if err != nil || run == nil || run.TaskID != record.ID {
		return err
	}
	if err := convertPartial(tx, live, record.ConversationID); err != nil {
		return err
	}
	settlement := durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: outcome.Reason}
	if outcome.Status == durable.OutcomeFaulted {
		settlement.Reason = "faulted"
		if outcome.Error != nil {
			detail, err := json.Marshal(outcome.Error.Message)
			if err != nil {
				return err
			}
			settlement.Detail = detail
		}
	}
	return endRun(tx, live, record.ID, settlement)
}

// assignJSON assigns value at target[key] leaf by leaf. Chord records a
// container assignment as one full set and only emits an append when a
// string leaf is reassigned with a longer string, so writing a partial whole
// would store and publish the complete message on every flush.
func assignJSON(target *delta.Draft, key any, value any) error {
	current, exists := target.Get(key)
	if child, isDraft := current.(*delta.Draft); isDraft {
		switch v := value.(type) {
		case map[string]any:
			if !child.IsArray() {
				for _, name := range child.Keys() {
					if _, keep := v[name]; !keep {
						if err := child.Delete(name); err != nil {
							return err
						}
					}
				}
				for _, name := range sortedKeys(v) {
					if err := assignJSON(child, name, v[name]); err != nil {
						return err
					}
				}
				return nil
			}
		case []any:
			if child.IsArray() && child.Len() <= len(v) {
				for index, item := range v {
					if index < child.Len() {
						if err := assignJSON(child, index, item); err != nil {
							return err
						}
					} else if _, err := child.Push(item); err != nil {
						return err
					}
				}
				return nil
			}
		}
		return target.Set(key, value)
	}
	if exists && scalarEqual(current, value) {
		return nil
	}
	return target.Set(key, value)
}

// scalarEqual reports whether two document scalars are equal; containers
// never are.
func scalarEqual(a, b any) bool {
	switch a.(type) {
	case nil, bool, float64, string:
	default:
		return false
	}
	switch b.(type) {
	case nil, bool, float64, string:
	default:
		return false
	}
	return a == b
}
