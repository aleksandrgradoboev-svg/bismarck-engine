package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// The built-in tool task (pi packages/durable/src/harness/tool.ts). This
// stage of the port runs model-issued calls; the execute phase (recovery
// after intent) and nested calls come with their stages, and reaching them
// faults ("not ported yet").

// The output limits a tool without its own keep (pi truncate.ts).
const (
	defaultToolMaxLines = 2000
	defaultToolMaxBytes = 50 * 1024
)

// Tool call input kinds.
const (
	toolCallModel  = "model"
	toolCallNested = "nested"
)

// ToolTaskInput is a model-issued call, read from its assistant entry, or a
// nested call a running tool made, whose call the input carries.
type ToolTaskInput struct {
	Kind         string           `json:"kind"`
	Assistant    *durable.EntryID `json:"assistant,omitempty"`
	CallID       string           `json:"callId,omitempty"`
	Parent       *durable.TaskID  `json:"parent,omitempty"`
	ParentCallID string           `json:"parentCallId,omitempty"`
	Key          string           `json:"key,omitempty"`
	Call         *ai.ToolCall     `json:"call,omitempty"`
	// Progress false commits no running output, details or diagnostics to
	// the slot; the result still gets them.
	Progress *bool `json:"progress,omitempty"`
}

// Tool task phases.
const (
	toolPhaseCall    = "call"
	toolPhaseExecute = "execute"
)

// ToolTaskCheckpoint is the call phase, or the durable intent: the final
// arguments and the replay policy recorded before execution.
type ToolTaskCheckpoint struct {
	Phase     string         `json:"phase"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Replay    string         `json:"replay,omitempty"`
}

// ToolTaskResult is a model-issued call's result entry, with the controls it
// requests, or a nested call's receipt. A version 1 result has no Kind; it is
// a model-issued call's.
type ToolTaskResult struct {
	Kind    string           `json:"kind,omitempty"`
	EntryID *durable.EntryID `json:"entryId,omitempty"`
	Control *ToolControl     `json:"control,omitempty"`
}

type toolTask = Running[ToolTaskInput, ToolTaskCheckpoint]

// ToolTask is the built-in tool task: it resolves the called tool among its
// phase agent's tools, validates, runs BeforeTool, records intent, executes,
// runs AfterTool, and settles the result, all in one call handler, so nothing
// separates resolution from settlement.
var ToolTask Task

func init() {
	ToolTask = MustDefineTask(TaskSpec[ToolTaskInput, ToolTaskCheckpoint, ToolTaskResult]{
		Name:    toolKind,
		Version: 2,
		Initial: func(ToolTaskInput) ToolTaskCheckpoint { return ToolTaskCheckpoint{Phase: toolPhaseCall} },
		// Version 1 had only model-issued calls.
		Migrate: func(input, checkpoint json.RawMessage, _ int) (ToolTaskInput, ToolTaskCheckpoint, error) {
			var in ToolTaskInput
			var cp ToolTaskCheckpoint
			if err := json.Unmarshal(input, &in); err != nil {
				return in, cp, err
			}
			in.Kind = toolCallModel
			return in, cp, json.Unmarshal(checkpoint, &cp)
		},
		Phases: map[string]func(ctx context.Context, task *toolTask, rt *TaskRuntime) error{
			toolPhaseCall:    callTool,
			toolPhaseExecute: func(context.Context, *toolTask, *TaskRuntime) error { return notPorted("the tool execute phase") },
		},
		Abort: abortTool,
	})
	builtinTasks = append(builtinTasks, ToolTask)
}

// callTool is the call phase.
func callTool(ctx context.Context, task *toolTask, rt *TaskRuntime) error {
	if task.Input.Kind == toolCallNested {
		return notPorted("nested tool calls")
	}
	call, err := readToolCall(ctx, rt, task.Input)
	if err != nil {
		return err
	}
	tool, err := resolveTool(ctx, rt, task.Input, call.Name)
	if err != nil {
		return err
	}
	if tool == nil {
		unavailable := harnessError("tool_unavailable", fmt.Sprintf("Tool %s is not available", call.Name))
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*ToolSlot) ToolExecutionResult { return unavailable }, nil)
	}
	invalidResult := func(message string) error {
		result := harnessError("invalid_arguments", message)
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*ToolSlot) ToolExecutionResult { return result }, nil)
	}
	args, problem := prepareArguments(tool, call.Arguments)
	if problem == "" {
		args, problem = validateArguments(tool, call.ToolCall, args)
	}
	if problem != "" {
		return invalidResult(problem)
	}
	var block *string
	if err := EachHook(ctx, rt, func(hooks *ToolHooks) error {
		if block != nil || hooks.BeforeTool == nil {
			return nil
		}
		hookCall := call
		hookCall.Arguments = args
		decision, err := hooks.BeforeTool(ctx, hookCall, rt)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			message := err.Error()
			block = &message
			return nil
		}
		if decision != nil {
			if decision.Block != nil {
				block = decision.Block
			} else if decision.Arguments != nil {
				args = decision.Arguments
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if block != nil {
		blocked := harnessError("blocked", "Tool call blocked: "+*block)
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*ToolSlot) ToolExecutionResult { return blocked }, nil)
	}
	validated, problem := validateArguments(tool, call.ToolCall, args)
	if problem != "" {
		return invalidResult(problem)
	}
	// Strict JSON: what the slot and the checkpoint can hold.
	final, err := strictObject(validated)
	if err != nil {
		return err
	}
	replay := tool.Replay
	if replay == "" {
		replay = ReplayUnsafe
	}
	if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if slot := toolSlotDraft(live, rt.TaskID); slot != nil {
			if err := slot.Set("status", slotRunning); err != nil {
				return nil, err
			}
		}
		return Next(ToolTaskCheckpoint{Phase: toolPhaseExecute, Arguments: final, Replay: replay})
	}); err != nil {
		return err
	}
	return runTool(ctx, rt, task.Input, call, tool, final)
}

// abortTool settles an abort-marked call; one abandoned after a restart says
// whether it may have run.
func abortTool(ctx context.Context, task *toolTask, rt *TaskRuntime) error {
	if task.Input.Kind == toolCallNested {
		return notPorted("nested tool calls")
	}
	call, err := readToolCall(ctx, rt, task.Input)
	if err != nil {
		return err
	}
	code, message := "aborted", fmt.Sprintf("Tool %s was aborted", call.Name)
	if task.Record.AbortReason == durable.AbortReasonRestart {
		if task.Checkpoint.Phase == toolPhaseExecute {
			code, message = "interrupted", fmt.Sprintf("Tool %s was interrupted by a restart and may have partially run", call.Name)
		} else {
			code, message = "abandoned", fmt.Sprintf("Tool %s was not started: its caller ended with a restart", call.Name)
		}
	}
	return settleTool(ctx, rt, task.Input, call, endingAborted, func(slot *ToolSlot) ToolExecutionResult { return fromSlot(slot, code, message) }, nil)
}

// resolveTool is the called tool among the tools the model is offered for a
// model-issued call, else among those tools may call; nil when absent.
func resolveTool(ctx context.Context, rt *TaskRuntime, input ToolTaskInput, name string) (*ToolRegistration, error) {
	agent, err := rt.Agent(ctx)
	if err != nil {
		return nil, err
	}
	tools := agent.Callable
	if input.Kind != toolCallNested {
		tools = agent.Tools
	}
	for _, tool := range tools {
		if tool.Name == name {
			return tool, nil
		}
	}
	return nil, nil
}

// readToolCall is the call: from the input for a nested call, else from the
// assistant entry.
func readToolCall(ctx context.Context, rt *TaskRuntime, input ToolTaskInput) (ToolHookCall, error) {
	if input.Kind == toolCallNested && input.Call != nil {
		var parent ToolCallParent
		if input.Parent != nil {
			parent.TaskID = *input.Parent
		}
		parent.CallID = input.ParentCallID
		return ToolHookCall{ToolCall: *input.Call, Parent: &parent}, nil
	}
	if input.Assistant == nil {
		return ToolHookCall{}, errors.New("Tool task input names no assistant entry")
	}
	entry, err := rt.Entry(ctx, *input.Assistant)
	if err != nil {
		return ToolHookCall{}, err
	}
	if entry != nil && len(entry.Model) > 0 {
		if message, ok := entry.Model[0].(ai.AssistantMessage); ok {
			for _, content := range message.Content {
				if call, ok := toolCallOf(content); ok && call.ID == input.CallID {
					return ToolHookCall{ToolCall: call}, nil
				}
			}
		}
	}
	return ToolHookCall{}, fmt.Errorf("Entry %d has no tool call %s", *input.Assistant, input.CallID)
}

// prepareArguments is the call's arguments as repaired by the tool; a
// failing repair makes them invalid.
func prepareArguments(tool *ToolRegistration, args map[string]any) (map[string]any, string) {
	if tool.PrepareArguments == nil {
		return args, ""
	}
	prepared, err := tool.PrepareArguments(args)
	if err != nil {
		return nil, err.Error()
	}
	object, ok := prepared.(map[string]any)
	if !ok {
		return nil, fmt.Sprintf("Tool %s repaired its arguments into a non-object", tool.Name)
	}
	return object, ""
}

// validateArguments is the arguments validated and coerced against the
// implementation's schema, or why they are invalid.
func validateArguments(tool *ToolRegistration, call ai.ToolCall, args map[string]any) (map[string]any, string) {
	call.Arguments = args
	validated, err := ai.ValidateToolArguments(tool.Tool, call)
	if err != nil {
		return nil, err.Error()
	}
	return validated, ""
}

// strictObject is value as a strict JSON object.
func strictObject(value map[string]any) (map[string]any, error) {
	converted, err := toValue(value)
	if err != nil {
		return nil, err
	}
	object, _ := converted.(map[string]any)
	if object == nil {
		object = map[string]any{}
	}
	return object, nil
}

// reported is what a running tool reported through its API: output, the
// last details, and diagnostics.
type reported struct {
	mu          sync.Mutex
	output      *output.OutputBuffer
	limits      output.Limits
	diagnostics []ToolDiagnostic
	details     any
	hasDetails  bool
}

// toolEnding is how a tool task ends; the result entry is appended either
// way. Failed (execution threw or was interrupted) records cancellation
// intent for the conversations the call owns; a result with IsError still
// completes.
type toolEnding struct {
	status  string
	message string
}

var (
	endingCompleted = toolEnding{status: durable.OutcomeCompleted}
	endingAborted   = toolEnding{status: durable.OutcomeAborted}
)

// runTool executes with the resolved implementation, then settles its
// result.
func runTool(ctx context.Context, rt *TaskRuntime, input ToolTaskInput, call ToolHookCall, tool *ToolRegistration, args map[string]any) error {
	limits := output.Limits{MaxBytes: defaultToolMaxBytes, MaxLines: defaultToolMaxLines, Retain: output.RetainHead}
	if tool.OutputLimits != nil {
		if tool.OutputLimits.MaxBytes != 0 {
			limits.MaxBytes = tool.OutputLimits.MaxBytes
		}
		if tool.OutputLimits.MaxLines != 0 {
			limits.MaxLines = tool.OutputLimits.MaxLines
		}
		if tool.OutputLimits.Retain != "" {
			limits.Retain = output.Retain(tool.OutputLimits.Retain)
		}
	}
	rep := &reported{output: output.NewOutputBuffer(limits), limits: limits}
	streams := input.Kind == toolCallModel || input.Progress == nil || *input.Progress
	progress := publishToolProgress(ctx, rt, rep, streams)
	api := &toolAPI{rt: rt, call: call, tool: tool, reported: rep, progress: progress, limits: limits}

	var result ToolExecutionResult
	ending := endingCompleted
	var durationMs *int64
	environment, err := rt.Env(ctx)
	if err == nil {
		api.environment = environment
		started := time.Now()
		result, err = tool.Execute(ctx, args, api)
		elapsed := time.Since(started).Round(time.Millisecond).Milliseconds()
		durationMs = &elapsed
	}
	if err != nil {
		if ctx.Err() != nil {
			api.end()
			for _, waiter := range progress.Stop() {
				waiter.Reject(err)
			}
			return err
		}
		result = ToolExecutionResult{IsError: true, Diagnostics: []ToolDiagnostic{toolDiagnostic("tool_error", err.Error())}}
		// A throw, from Execute or from building the environment, ends the
		// task failed, which cancels what the call owned.
		ending = toolEnding{status: durable.OutcomeFailed, message: fmt.Sprintf("Tool %s threw", call.Name)}
	}
	api.end()
	rep.mu.Lock()
	rep.output.End()
	rep.mu.Unlock()
	// Details still waiting for a progress commit settle with the terminal
	// commit, the final flush.
	pending := progress.Stop()
	settled, err := finalToolResult(ctx, rt, input, call, tool, result, rep)
	if err == nil {
		err = settleTool(ctx, rt, input, call, ending, func(*ToolSlot) ToolExecutionResult { return settled }, durationMs)
	}
	for _, waiter := range pending {
		if err != nil {
			waiter.Reject(err)
		} else {
			waiter.Resolve()
		}
	}
	return err
}

// harnessClock paces progress by the Harness clock and timers; after the
// invocation ends nothing more commits, and the clock answers with the last
// time it read.
type harnessClock struct {
	rt   *TaskRuntime
	mu   sync.Mutex
	last time.Time
}

func (c *harnessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now, err := c.rt.Now(); err == nil {
		c.last = now
	}
	return c.last
}

func (c *harnessClock) AfterFunc(d time.Duration, f func()) func() bool {
	disarm := c.rt.SetTimer(f, d)
	return func() bool {
		disarm()
		return true
	}
}

// publishToolProgress is the throttled commits of what the tool reported
// into its slot, each writing only what changed since the last one; none
// when enabled is false.
func publishToolProgress(ctx context.Context, rt *TaskRuntime, rep *reported, enabled bool) *output.Progress {
	var written struct {
		text        string
		details     any
		hasDetails  bool
		diagnostics int
	}
	return output.NewProgressWithClock(func() (int, error) {
		if !enabled {
			return 0, nil
		}
		// Capture everything at once: the tool keeps reporting while the
		// commit is in flight.
		rep.mu.Lock()
		snapshot := rep.output.Snapshot()
		details, hasDetails := rep.details, rep.hasDetails
		diagnostics := slices.Clone(rep.diagnostics)
		rep.mu.Unlock()
		added := diagnostics[written.diagnostics:]
		detailsChanged := hasDetails != written.hasDetails || !jsonAlike(details, written.details)
		// What the commit writes, as chord diffs the string.
		bytes := 0
		if snapshot.Text != written.text {
			shared := 0
			if strings.HasPrefix(snapshot.Text, written.text) {
				shared = len(written.text)
			} else {
				shared = delta.Overlap(written.text, snapshot.Text, 65_536)
			}
			bytes += output.UTF8ByteLength(snapshot.Text[shared:])
		}
		if detailsChanged {
			raw, _ := json.Marshal(details)
			bytes += len(raw)
		}
		if len(added) > 0 {
			raw, _ := json.Marshal(added)
			bytes += len(raw)
		}
		err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			slot := toolSlotDraft(live, rt.TaskID)
			if slot == nil {
				return nil, nil
			}
			// REMINDER: assign output as one string field. Chord then diffs
			// it into an append, or a trim plus an append for a sliding
			// tail; replacing the slot would record the whole window.
			// An absent output is "", as pi's `slot.output ?? ""`.
			current, _ := slot.Get("output")
			if current == nil {
				current = ""
			}
			if current != snapshot.Text {
				if err := slot.Set("output", snapshot.Text); err != nil {
					return nil, err
				}
			}
			if snapshot.DroppedBytes > 0 {
				if err := slot.Set("droppedBytes", float64(snapshot.DroppedBytes)); err != nil {
					return nil, err
				}
			}
			if snapshot.DroppedLines > 0 {
				if err := slot.Set("droppedLines", float64(snapshot.DroppedLines)); err != nil {
					return nil, err
				}
			}
			// Diff details leaf by leaf and append new diagnostics, so each
			// commit writes only what changed.
			if detailsChanged && hasDetails {
				value, err := toValue(details)
				if err != nil {
					return nil, err
				}
				if err := assignJSON(slot, "details", value); err != nil {
					return nil, err
				}
			}
			if len(added) > 0 {
				if slot.At("diagnostics") == nil {
					if err := slot.Set("diagnostics", []any{}); err != nil {
						return nil, err
					}
				}
				list := slot.At("diagnostics")
				for _, diagnostic := range added {
					value, err := toValue(diagnostic)
					if err != nil {
						return nil, err
					}
					if _, err := list.Push(value); err != nil {
						return nil, err
					}
				}
			}
			return nil, nil
		})
		if err != nil {
			return 0, err
		}
		written.text, written.details, written.hasDetails, written.diagnostics = snapshot.Text, details, hasDetails, len(diagnostics)
		return bytes, nil
	}, func(err error) {
		// Failures after an abort mark or close are expected; the committed
		// state stays consistent.
		if ctx.Err() == nil {
			_ = rt.Report(err)
		}
	}, rt.Settings().Progress.OutputInterval, &harnessClock{rt: rt})
}

// jsonAlike reports whether two JSON values encode alike.
func jsonAlike(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// finalToolResult is the settled result: the tool's result with the
// retained output and last details as fallbacks, its diagnostics after those
// reported through the API, AfterTool applied, explicit text bounded, and
// StructuredOutput checked against the tool's schema, with the Harness's
// diagnostics last. A model-issued call whose StructuredOutput breaks the
// contract only loses it, and the break is reported to the host.
func finalToolResult(ctx context.Context, rt *TaskRuntime, input ToolTaskInput, call ToolHookCall, tool *ToolRegistration, result ToolExecutionResult, rep *reported) (ToolExecutionResult, error) {
	var harness []ToolDiagnostic
	rep.mu.Lock()
	var retained *output.Bounded
	if result.Output == nil {
		snapshot := rep.output.Snapshot()
		retained = &snapshot
	}
	details, hasDetails := rep.details, rep.hasDetails
	diagnostics := slices.Clone(rep.diagnostics)
	rep.mu.Unlock()
	out := result.Output
	if retained != nil {
		out = ai.ContentList{}
		if retained.Text != "" {
			out = ai.ContentList{ai.TextContent{Text: retained.Text}}
		}
	}
	final := result
	final.Output = out
	if result.Details == nil && hasDetails {
		final.Details = details
	}
	final.Diagnostics = append(diagnostics, result.Diagnostics...)
	keptOutput := final.Output
	if err := EachHook(ctx, rt, func(hooks *ToolHooks) error {
		if hooks.AfterTool == nil {
			return nil
		}
		replaced, err := hooks.AfterTool(ctx, call, final, rt)
		if err == nil && replaced != nil {
			final = *replaced
		}
		return err
	}); err != nil {
		return ToolExecutionResult{}, err
	}
	// The retained output's truncation applies only while AfterTool kept
	// that output.
	if retained != nil && retained.DroppedBytes > 0 && sameContent(final.Output, keptOutput) {
		harness = append(harness, truncatedDiagnostic(retained.DroppedBytes, retained.DroppedLines, string(rep.limits.Retain)))
	}
	bounded, droppedBytes, droppedLines := boundContent(final.Output, rep.limits)
	if droppedBytes > 0 {
		harness = append(harness, truncatedDiagnostic(droppedBytes, droppedLines, string(rep.limits.Retain)))
	}
	if broken := structuredOutputError(tool, final); broken != "" {
		final.StructuredOutput = nil
		// The model never sees structured output, so its call stands; the
		// host learns of the broken tool.
		extension := ""
		if agent, err := rt.Agent(ctx); err == nil {
			extension = toolExtension(agent, tool.Name)
		}
		if err := rt.Report(errors.New(broken), ReportFrom{Extension: extension}); err != nil {
			return ToolExecutionResult{}, err
		}
	}
	final.Output = bounded
	final.Diagnostics = append(final.Diagnostics, harness...)
	return final, nil
}

// sameContent reports whether two content lists are the same list.
func sameContent(a, b ai.ContentList) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return (a == nil) == (b == nil)
	}
	return &a[0] == &b[0]
}

// structuredOutputError is why a result's StructuredOutput breaks the tool's
// contract; empty when it keeps it.
func structuredOutputError(tool *ToolRegistration, result ToolExecutionResult) string {
	schema := tool.StructuredOutputSchema
	value := result.StructuredOutput
	if schema == nil {
		if value == nil {
			return ""
		}
		return fmt.Sprintf("Tool %s returned structuredOutput but declares no structuredOutputSchema", tool.Name)
	}
	if value == nil {
		if result.IsError {
			return ""
		}
		return fmt.Sprintf("Tool %s returned no structuredOutput", tool.Name)
	}
	normalized, err := toValue(value)
	if err != nil {
		return fmt.Sprintf("Tool %s returned structuredOutput that is not JSON: %v", tool.Name, err)
	}
	errs := schema.ValidationErrors(normalized)
	if len(errs) == 0 {
		return ""
	}
	return fmt.Sprintf("Tool %s returned structuredOutput that does not match its schema: %s: %s", tool.Name, errs[0].Path, errs[0].Message)
}

// settleTool commits the tool's terminal state and marks its slot done: a
// model-issued call appends its result entry and ends with the entry ID.
// build receives the slot, so interruption and abort can report the durable
// partial output.
func settleTool(ctx context.Context, rt *TaskRuntime, input ToolTaskInput, call ToolHookCall, ending toolEnding, build func(slot *ToolSlot) ToolExecutionResult, durationMs *int64) error {
	if input.Kind == toolCallNested {
		return notPorted("nested tool calls")
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		slotDraft := toolSlotDraft(live, rt.TaskID)
		var slot *ToolSlot
		if slotDraft != nil {
			snapshot, err := slotDraft.Snapshot()
			if err != nil {
				return nil, err
			}
			var decoded ToolSlot
			if err := decodeValue(snapshot, &decoded); err != nil {
				return nil, err
			}
			slot = &decoded
		}
		result := build(slot)
		now, err := rt.Now()
		if err != nil {
			return nil, err
		}
		entry, err := appendToolResult(tx, rt.ConversationID, call.ToolCall, result, now.UnixMilli(), durationMs)
		if err != nil {
			return nil, err
		}
		if slotDraft != nil {
			if err := finishSlot(slotDraft); err != nil {
				return nil, err
			}
			if err := slotDraft.Set("entry", float64(entry.ID)); err != nil {
				return nil, err
			}
		}
		id := entry.ID
		settled := ToolTaskResult{Kind: toolCallModel, EntryID: &id}
		if result.Control != nil && ending.status == durable.OutcomeCompleted {
			control := *result.Control
			settled.Control = &control
		}
		raw, err := json.Marshal(settled)
		if err != nil {
			return nil, err
		}
		outcome := &durable.TaskOutcome{Status: ending.status, Result: raw}
		if ending.status == durable.OutcomeFailed {
			outcome.Error = &durable.TaskOutcomeError{Message: ending.message}
		}
		return &durable.TaskState{Status: durable.TaskTerminal, Outcome: outcome}, nil
	})
}

// fromSlot is an error result from the slot's durable partial output,
// details and diagnostics.
func fromSlot(slot *ToolSlot, code, message string) ToolExecutionResult {
	result := ToolExecutionResult{Output: ai.ContentList{}, IsError: true}
	if slot != nil {
		result.Diagnostics = slices.Clone(slot.Diagnostics)
		if slot.DroppedBytes > 0 {
			result.Diagnostics = append(result.Diagnostics, truncatedDiagnostic(slot.DroppedBytes, slot.DroppedLines, ""))
		}
		if slot.Output != nil && *slot.Output != "" {
			result.Output = ai.ContentList{ai.TextContent{Text: *slot.Output}}
		}
		result.Details = slot.Details
	}
	result.Diagnostics = append(result.Diagnostics, toolDiagnostic(code, message))
	return result
}

// harnessError is an error result the Harness writes itself: no content and
// one error diagnostic with code.
func harnessError(code, message string) ToolExecutionResult {
	return ToolExecutionResult{Output: ai.ContentList{}, IsError: true, Diagnostics: []ToolDiagnostic{toolDiagnostic(code, message)}}
}

func toolDiagnostic(code, message string) ToolDiagnostic {
	return ToolDiagnostic{Severity: "error", Code: code, Message: message}
}

// truncatedDiagnostic is the Harness's truncation diagnostic; retain is
// unknown ("") when rebuilt from a slot after recovery.
func truncatedDiagnostic(droppedBytes, droppedLines int, retain string) ToolDiagnostic {
	kept := ""
	switch retain {
	case string(output.RetainHead):
		kept = " to its beginning"
	case string(output.RetainTail):
		kept = " to its end"
	}
	return ToolDiagnostic{Severity: "warn", Code: "truncated", Message: fmt.Sprintf("Output truncated%s: %d lines, %d bytes dropped", kept, droppedLines, droppedBytes)}
}

// appendToolResult appends a pi.tool-result entry. The content ends with the
// rendered diagnostics, so the stored message is exactly what the model
// sees; data keeps the structured list. A result's usage is added to
// pi.usage in the same commit.
func appendToolResult(tx *durable.Tx, conversationID durable.ConversationID, call ai.ToolCall, result ToolExecutionResult, timestamp int64, durationMs *int64) (durable.EntryRecord, error) {
	diagnostics := slices.Clone(result.Diagnostics)
	if diagnostics == nil {
		diagnostics = []ToolDiagnostic{}
	}
	content := append(ai.ContentList{}, result.Output...)
	if len(diagnostics) > 0 {
		content = append(content, ai.TextContent{Text: renderDiagnostics(diagnostics)})
	}
	message := ai.ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    content,
		IsError:    result.IsError,
		DurationMs: durationMs,
		Timestamp:  timestamp,
	}
	if result.Details != nil {
		details, err := toValue(result.Details)
		if err != nil {
			return durable.EntryRecord{}, err
		}
		message.Details = details
	}
	if result.Usage != nil {
		usage := *result.Usage
		message.Usage = &usage
		if err := recordUsage(tx, conversationID, "tools", call.Name, usage); err != nil {
			return durable.EntryRecord{}, err
		}
	}
	data, err := json.Marshal(map[string]any{"diagnostics": diagnostics})
	if err != nil {
		return durable.EntryRecord{}, err
	}
	return tx.AppendEntry(conversationID, durable.EntryDraft{Kind: durable.ToolResultEntry.Kind, Model: []ai.Message{message}, Data: data})
}

func renderDiagnostics(diagnostics []ToolDiagnostic) string {
	lines := make([]string, len(diagnostics))
	for i, diagnostic := range diagnostics {
		lines[i] = fmt.Sprintf("[%s] %s", diagnostic.Severity, diagnostic.Message)
	}
	return "<harness>\n" + strings.Join(lines, "\n") + "\n</harness>"
}

// boundContent bounds the text of result content. When the joined text
// exceeds the limits, the text items are replaced by one bounded item at the
// position of the first (head) or last (tail) text item; other content is
// kept.
func boundContent(content ai.ContentList, limits output.Limits) (ai.ContentList, int, int) {
	var texts []int
	var joined strings.Builder
	for i, item := range content {
		if text, ok := item.(ai.TextContent); ok {
			texts = append(texts, i)
			joined.WriteString(text.Text)
		}
	}
	bounded := output.BoundOutput(joined.String(), limits)
	if bounded.DroppedBytes == 0 {
		return content, 0, 0
	}
	keep := texts[0]
	if limits.Retain != output.RetainHead {
		keep = texts[len(texts)-1]
	}
	result := ai.ContentList{}
	for i, item := range content {
		text, isText := item.(ai.TextContent)
		switch {
		case !isText:
			result = append(result, item)
		case i == keep:
			text.Text = bounded.Text
			result = append(result, text)
		}
	}
	return result, bounded.DroppedBytes, bounded.DroppedLines
}

// toolAPI is the ToolExecutionAPI of one invocation of a tool task.
type toolAPI struct {
	rt          *TaskRuntime
	call        ToolHookCall
	tool        *ToolRegistration
	reported    *reported
	progress    *output.Progress
	limits      output.Limits
	environment env.ExecutionEnv

	mu    sync.Mutex
	ended bool
}

func (a *toolAPI) end() {
	a.mu.Lock()
	a.ended = true
	a.mu.Unlock()
}

func (a *toolAPI) live() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return fmt.Errorf("Tool call %s has settled", a.call.ID)
	}
	return nil
}

func (a *toolAPI) TaskID() durable.TaskID                 { return a.rt.TaskID }
func (a *toolAPI) ConversationID() durable.ConversationID { return a.rt.ConversationID }
func (a *toolAPI) CallID() string                         { return a.call.ID }
func (a *toolAPI) Registry() *RegistrySnapshot            { return a.rt.Registry() }
func (a *toolAPI) Agent(ctx context.Context) (*Agent, error) {
	return a.rt.Agent(ctx)
}
func (a *toolAPI) Models() ai.Models       { return a.rt.Models() }
func (a *toolAPI) Env() env.ExecutionEnv   { return a.environment }
func (a *toolAPI) Now() (time.Time, error) { return a.rt.Now() }
func (a *toolAPI) OutputWindow() *env.OutputWindow {
	if a.limits.Retain != output.RetainTail {
		return nil
	}
	return &env.OutputWindow{
		MaxBytes:       int64(a.limits.MaxBytes),
		MaxLines:       int64(a.limits.MaxLines),
		MinInterval:    a.rt.Settings().Progress.OutputInterval,
		BytesPerSecond: output.ProgressBytesPerSecond,
	}
}

func (a *toolAPI) Output(chunk []byte, skipped *output.Skip) error {
	if err := a.live(); err != nil {
		return err
	}
	a.reported.mu.Lock()
	changed, err := a.reported.output.Push(chunk, skipped)
	a.reported.mu.Unlock()
	if err != nil {
		return err
	}
	if changed {
		a.progress.Mark()
	}
	return nil
}

func (a *toolAPI) RetainedOutput() (string, bool, error) {
	if err := a.live(); err != nil {
		return "", false, err
	}
	a.reported.mu.Lock()
	defer a.reported.mu.Unlock()
	retained := a.reported.output.Snapshot()
	return retained.Text, retained.DroppedBytes > 0, nil
}

func (a *toolAPI) Diagnostic(diagnostic ToolDiagnostic) error {
	if err := a.live(); err != nil {
		return err
	}
	a.reported.mu.Lock()
	a.reported.diagnostics = append(a.reported.diagnostics, diagnostic)
	a.reported.mu.Unlock()
	a.progress.Mark()
	return nil
}

func (a *toolAPI) Details(ctx context.Context, value any) error {
	if err := a.live(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	copied, err := toValue(value)
	if err != nil {
		return err
	}
	a.reported.mu.Lock()
	a.reported.details, a.reported.hasDetails = copied, true
	a.reported.mu.Unlock()
	// Cancelling the wait leaves the update in place.
	return a.progress.MarkAndWait().Wait(ctx)
}

func (a *toolAPI) Commit(ctx context.Context, change func(tx *durable.Tx) error) error {
	return a.rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		return nil, change(tx)
	})
}

func (a *toolAPI) Memo(ctx context.Context, name string) (json.RawMessage, error) {
	return a.rt.Memo(ctx, name)
}

func (a *toolAPI) MemoOr(ctx context.Context, name string, candidate any) (json.RawMessage, error) {
	return a.rt.MemoOr(ctx, name, candidate)
}

func (a *toolAPI) CreateTask(ctx context.Context, task Task, input any, options durable.TaskOptions) (durable.TaskID, error) {
	// A caller that can rerun reattaches to its child tasks after a restart;
	// any other caller never resumes, so they are abandoned.
	if options.Ownership.Kind == durable.OwnershipTask && a.tool.Replay != ReplaySafe {
		options.AbandonOnRestart = true
	}
	var id durable.TaskID
	err := a.rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		var err error
		id, err = CreateTask(tx, task, input, options)
		return nil, err
	})
	return id, err
}

func (a *toolAPI) GetTask(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	return a.rt.GetTask(ctx, id)
}

func (a *toolAPI) WaitForTask(ctx context.Context, id durable.TaskID) (durable.TaskRecord, error) {
	return a.rt.WaitForTask(ctx, id)
}

func (a *toolAPI) Snapshot(ctx context.Context, ref durable.DocRef) (map[string]any, error) {
	return a.rt.Snapshot(ctx, ref)
}

func (a *toolAPI) SnapshotAsOf(ctx context.Context, ref durable.DocRef, at durable.EntryID) (map[string]any, error) {
	return a.rt.SnapshotAsOf(ctx, ref, at)
}

func (a *toolAPI) WatchDoc(ctx context.Context, ref durable.DocRef) (*durable.DocumentWatch, error) {
	return a.rt.WatchDoc(ctx, ref)
}

func (a *toolAPI) ExecuteTool(context.Context, string, map[string]any, NestedCallOptions) (NestedToolExecutionResult, error) {
	return NestedToolExecutionResult{}, notPorted("nested tool calls")
}
