package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The generation's tool round (pi generation.ts startToolRound, the tools
// phase and finishToolRound, spec §8.3, §8.5): the tool-calling answer starts
// one tool task per offered call, owned by the generation, which waits for
// them in its tools phase and then ends the run or hands it to the next
// generation.

// createToolTask creates the tool task of call callID, owned by the
// generation.
func createToolTask(tx *durable.Tx, rt *TaskRuntime, assistant durable.EntryID, callID string) (durable.TaskID, error) {
	return CreateTask(tx, ToolTask, ToolTaskInput{Kind: toolCallModel, Assistant: &assistant, CallID: callID}, durable.TaskOptions{
		Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: rt.TaskID},
	})
}

// startToolRound appends the tool-calling answer and starts its tool round
// in one commit. A call to a tool the request did not offer gets its
// tool_unavailable result here; every other call gets a tool task owned by
// the generation, only the first one now when the round is sequential. The
// generation then waits for them in its tools phase, keeping the run.
func startToolRound(ctx context.Context, rt *TaskRuntime, request GenerationCheckpoint, message ai.AssistantMessage, calls []ai.ToolCall) error {
	if request.Cutoff == nil {
		return fmt.Errorf("Generation request has no cutoff")
	}
	view, err := rt.Context(ctx, rt.ConversationID, request.Cutoff)
	if err != nil {
		return err
	}
	offered := map[string]bool{}
	for _, tool := range ai.GetCurrentTools(view.Messages) {
		offered[tool.Name] = true
	}
	// Read as the round starts; a tool is resolved as its tool task
	// resolves it.
	agent, err := rt.Agent(ctx)
	if err != nil {
		return err
	}
	sequential := rt.Settings().ToolExecution == ToolExecutionSequential
	for _, call := range calls {
		if !offered[call.Name] {
			continue
		}
		for _, tool := range agent.Tools {
			if tool.Name == call.Name && tool.ExecutionMode == ToolExecutionSequential {
				sequential = true
			}
		}
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		entry, err := appendAssistant(tx, rt.ConversationID, message)
		if err != nil {
			return nil, err
		}
		var slots []ToolSlot
		tools := []durable.TaskID{}
		pending := []string{}
		for _, call := range calls {
			if !offered[call.Name] {
				unavailable := harnessError("tool_unavailable", fmt.Sprintf("Tool %s is not available", call.Name))
				result, err := appendToolResult(tx, rt.ConversationID, call, unavailable, now.UnixMilli(), nil)
				if err != nil {
					return nil, err
				}
				id := result.ID
				slots = append(slots, ToolSlot{CallID: call.ID, Name: call.Name, Status: slotDone, Entry: &id})
				continue
			}
			if sequential && len(tools) > 0 {
				pending = append(pending, call.ID)
				slots = append(slots, ToolSlot{CallID: call.ID, Name: call.Name, Status: slotPending})
				continue
			}
			taskID, err := createToolTask(tx, rt, entry.ID, call.ID)
			if err != nil {
				return nil, err
			}
			tools = append(tools, taskID)
			id := taskID
			slots = append(slots, ToolSlot{CallID: call.ID, Name: call.Name, TaskID: &id, Status: slotPending})
		}
		if err := live.Delete("generation"); err != nil {
			return nil, err
		}
		if err := setDraftField(live, "tools", slots); err != nil {
			return nil, err
		}
		assistant := entry.ID
		return Wait(tools, durable.JoinAllSettled, GenerationCheckpoint{Phase: phaseTools, Assistant: &assistant, Tools: tools, Pending: pending})
	})
}

// toolsGeneration is the tools phase: a sequential round starts its next
// call and waits for it; once none is pending, the round ends.
func toolsGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	if checkpoint.Assistant == nil {
		return fmt.Errorf("Generation tools checkpoint lacks its assistant entry")
	}
	if len(checkpoint.Pending) == 0 {
		return finishToolRound(ctx, rt, *checkpoint.Assistant, checkpoint.Tools)
	}
	next, rest := checkpoint.Pending[0], checkpoint.Pending[1:]
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		taskID, err := createToolTask(tx, rt, *checkpoint.Assistant, next)
		if err != nil {
			return nil, err
		}
		if tools := live.At("tools"); tools != nil {
			for i := range tools.Len() {
				slot := tools.At(i)
				callID, _ := slot.Get("callId")
				if _, started := slot.Get("taskId"); callID == next && !started {
					if err := slot.Set("taskId", float64(taskID)); err != nil {
						return nil, err
					}
					break
				}
			}
		}
		return Wait([]durable.TaskID{taskID}, durable.JoinAllSettled, GenerationCheckpoint{
			Phase:     phaseTools,
			Assistant: checkpoint.Assistant,
			Tools:     append(slices.Clone(checkpoint.Tools), taskID),
			Pending:   slices.Clone(rest),
		})
	})
}

// finishToolRound ends a round whose tools are terminal: it applies their
// controls and either ends the run at the final boundary (terminate,
// handoff, or a queued reset) or hands it to the next generation at the
// postTools boundary (spec §8.5).
func finishToolRound(ctx context.Context, rt *TaskRuntime, assistant durable.EntryID, tools []durable.TaskID) error {
	outcomes, err := rt.Outcomes(ctx, tools)
	if err != nil {
		return err
	}
	controls := map[durable.TaskID]*ToolControl{}
	for i, id := range tools {
		controls[id] = nil
		outcome := outcomes[i]
		if outcome.Status != durable.OutcomeCompleted || len(outcome.Result) == 0 {
			continue
		}
		var result ToolTaskResult
		if err := json.Unmarshal(outcome.Result, &result); err != nil {
			return err
		}
		// A result without a kind is a version 1 one, a model-issued
		// call's.
		if result.Kind != toolCallNested {
			controls[id] = result.Control
		}
	}
	value, err := rt.Snapshot(ctx, LiveDoc.Conversation(rt.ConversationID))
	if err != nil {
		return err
	}
	var live struct {
		Tools []ToolSlot `json:"tools"`
	}
	if err := fromValue(value, &live); err != nil {
		return err
	}
	var results []durable.EntryID
	for _, slot := range live.Tools {
		if slot.Entry != nil {
			results = append(results, *slot.Entry)
		}
	}
	if err := EachHook(ctx, rt, func(hooks *GenerationHooks) error {
		if hooks.AfterTools == nil {
			return nil
		}
		return hooks.AfterTools(ctx, assistant, slices.Clone(results), rt)
	}); err != nil {
		return err
	}
	// Every call of the round, including those answered without a task,
	// must ask to terminate.
	terminate := len(live.Tools) > 0
	for _, slot := range live.Tools {
		if slot.TaskID == nil || controls[*slot.TaskID] == nil || !controls[*slot.TaskID].Terminate {
			terminate = false
		}
	}
	var added []string
	var handoff *string
	// In call order; the last handoff wins.
	for _, id := range tools {
		if control := controls[id]; control != nil {
			added = append(added, control.AddTools...)
			if control.Handoff != nil {
				handoff = control.Handoff
			}
		}
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		b, err := prepareBoundary(tx, rt.ConversationID, rt.Settings())
		if err != nil {
			return nil, err
		}
		if len(added) > 0 {
			if err := addTools(tx, rt.ConversationID, added); err != nil {
				return nil, err
			}
		}
		liveDoc, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		now := rt.s.now().UnixMilli()
		result, err := Complete(GenerationResult{EntryID: assistant})
		if err != nil {
			return nil, err
		}
		if terminate || handoff != nil {
			if handoff != nil {
				entry, err := tx.AppendEntry(rt.ConversationID, durable.EntryDraft{
					Kind:     durable.ResetEntry.Kind,
					HeadSelf: true,
					Model:    []ai.Message{TextInput(*handoff).userMessage(now)},
				})
				if err != nil {
					return nil, err
				}
				head := entry.ID
				b.head = &head
			}
			placed, err := applyBoundary(tx, b, true, now)
			if err != nil {
				return nil, err
			}
			if err := endRun(tx, liveDoc, rt.TaskID, durable.SubmissionSettlement{Status: durable.SubmissionDone, Answer: assistant}); err != nil {
				return nil, err
			}
			if len(placed.users) > 0 {
				if err := startRun(tx, rt.ConversationID, liveDoc, placed.users); err != nil {
					return nil, err
				}
			}
			return result, nil
		}
		placed, err := applyBoundary(tx, b, false, now)
		if err != nil {
			return nil, err
		}
		if placed.reset {
			// The queued reset cut the run's context before an answer.
			if err := endRun(tx, liveDoc, rt.TaskID, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "reset"}); err != nil {
				return nil, err
			}
			if len(placed.users) > 0 {
				if err := startRun(tx, rt.ConversationID, liveDoc, placed.users); err != nil {
					return nil, err
				}
			}
			return result, nil
		}
		for _, key := range []string{"tools", "nestedTools"} {
			if err := liveDoc.Delete(key); err != nil {
				return nil, err
			}
		}
		if run, err := liveRun(liveDoc); err != nil {
			return nil, err
		} else if run != nil && run.TaskID == rt.TaskID && len(placed.users) > 0 {
			run.Inputs = append(run.Inputs, placed.users...)
			if err := setDraftField(liveDoc, "run", run); err != nil {
				return nil, err
			}
		}
		next, err := createGeneration(tx, rt.ConversationID, nil)
		if err != nil {
			return nil, err
		}
		if err := handOver(liveDoc, rt.TaskID, next); err != nil {
			return nil, err
		}
		return result, nil
	})
}

// unstartedCalls are the calls callIDs of the assistant entry, in the given
// order.
func unstartedCalls(ctx context.Context, rt *TaskRuntime, assistant durable.EntryID, callIDs []string) ([]ai.ToolCall, error) {
	entry, err := rt.Entry(ctx, assistant)
	if err != nil || entry == nil || len(entry.Model) == 0 {
		return nil, err
	}
	message, ok := entry.Model[0].(ai.AssistantMessage)
	if !ok {
		return nil, nil
	}
	var calls []ai.ToolCall
	for _, id := range callIDs {
		for _, content := range message.Content {
			if call, ok := toolCallOf(content); ok && call.ID == id {
				calls = append(calls, call)
				break
			}
		}
	}
	return calls, nil
}
