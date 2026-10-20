package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// The built-in tool task (pi packages/durable/src/harness/tool.ts). This
// stage of the port runs model-issued calls, recovery after intent included;
// nested calls come with their stage, and reaching them faults ("not ported
// yet").

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

// MarshalJSON writes a nested call's call as pi stores it: a tool call
// block, with its "type".
func (in ToolTaskInput) MarshalJSON() ([]byte, error) {
	type plain ToolTaskInput
	if in.Call == nil {
		return json.Marshal(plain(in))
	}
	block, err := json.Marshal(ai.ContentList{*in.Call})
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		plain
		Call json.RawMessage `json:"call"`
	}{plain(in), block[1 : len(block)-1]})
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

// beforeNestedAdmission, set only by tests, runs before a nested call's
// admission commits: goroutines start admissions in no fixed order, so a test
// holds one there to make it commit after its caller returned, as pi's
// synchronous admissions can.
var beforeNestedAdmission atomic.Pointer[func(key string)]

// madeNestedCalls is the memo of a tool task that made a nested call, in any
// attempt, committed before the first admission of each attempt: only then
// can pi.live hold slots below it, which settling removes. A crash between
// the two only costs a scan.
const madeNestedCalls = "pi.tool.madeNestedCalls"

// NestedCallState is one nested call of a running tool: its task, and once
// it ended, its result.
type NestedCallState struct {
	// TaskID is absent only inside the commit that creates the call; a
	// creation that fails leaves no member.
	TaskID *durable.TaskID `json:"taskId,omitempty"`
	// Result is exactly what ExecuteTool returns; absent while running, and
	// for a call the scheduler faulted or orphaned.
	Result *NestedToolExecutionResult `json:"result,omitempty"`
}

// NestedCallDoc is the nested calls of a tool call: a task-scoped family of
// the calling task keyed by the nested call's key, so a rerun finds the calls
// it already made, and the caller reads each result. Created with the nested
// task; the nested call's terminal commit adds its result. One document per
// call, so no commit rewrites the others. Retires with the caller.
var NestedCallDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "pi.tool.nested-call",
	Version: 1,
	Scope:   durable.ScopeTask,
	Family:  true,
	Initial: func(any) map[string]any { return map[string]any{} },
	// The document retires with its task.
	CheckpointWhen: durable.NeverCheckpoint,
})

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
			toolPhaseExecute: executeTool,
		},
		Abort: abortTool,
	})
	builtinTasks = append(builtinTasks, ToolTask)
}

// callTool is the call phase.
func callTool(ctx context.Context, task *toolTask, rt *TaskRuntime) error {
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
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*SlotProgress) ToolExecutionResult { return unavailable }, nil)
	}
	invalidResult := func(message string) error {
		result := harnessError("invalid_arguments", message)
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*SlotProgress) ToolExecutionResult { return result }, nil)
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
		return settleTool(ctx, rt, task.Input, call, endingCompleted, func(*SlotProgress) ToolExecutionResult { return blocked }, nil)
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
			// A nested slot shows the arguments the call runs with, which
			// repair, hooks, and coercion may have changed. Compared with the
			// input, which the slot got at admission.
			if isNestedSlot(slot) && task.Input.Call != nil && !jsonAlike(task.Input.Call.Arguments, final) {
				if err := setDraftField(slot, "arguments", final); err != nil {
					return nil, err
				}
			}
		}
		return Next(ToolTaskCheckpoint{Phase: toolPhaseExecute, Arguments: final, Replay: replay})
	}); err != nil {
		return err
	}
	return runTool(ctx, rt, task.Input, call, tool, final)
}

// executeTool is recovery after intent: it reruns only when the stored and
// the current policy both say safe; otherwise the call fails as interrupted,
// answered with the slot's durable output.
func executeTool(ctx context.Context, task *toolTask, rt *TaskRuntime) error {
	call, err := readToolCall(ctx, rt, task.Input)
	if err != nil {
		return err
	}
	tool, err := resolveTool(ctx, rt, task.Input, call.Name)
	if err != nil {
		return err
	}
	if task.Checkpoint.Replay == ReplaySafe && tool != nil && tool.Replay == ReplaySafe {
		// The rerun reports from scratch; clear what the interrupted attempt
		// published.
		if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			if slot := toolSlotDraft(live, rt.TaskID); slot != nil {
				return nil, clearProgress(slot)
			}
			return nil, nil
		}); err != nil {
			return err
		}
		args := task.Checkpoint.Arguments
		if args == nil {
			// Empty arguments are omitted from the stored intent.
			args = map[string]any{}
		}
		return runTool(ctx, rt, task.Input, call, tool, args)
	}
	message := fmt.Sprintf("Tool %s was interrupted and may have partially run", call.Name)
	// Failed records cancellation intent, so the call's owned conversations,
	// left unsupervised, are aborted.
	ending := toolEnding{status: durable.OutcomeFailed, message: message}
	return settleTool(ctx, rt, task.Input, call, ending, func(slot *SlotProgress) ToolExecutionResult { return fromSlot(slot, "interrupted", message) }, nil)
}

// abortTool settles an abort-marked call; one abandoned after a restart says
// whether it may have run.
func abortTool(ctx context.Context, task *toolTask, rt *TaskRuntime) error {
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
	return settleTool(ctx, rt, task.Input, call, endingAborted, func(slot *SlotProgress) ToolExecutionResult { return fromSlot(slot, code, message) }, nil)
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
	// No admission starts after the end; let those underway commit, so
	// settlement aborts and lists their calls.
	api.admissions.Wait()
	rep.mu.Lock()
	rep.output.End()
	rep.mu.Unlock()
	// Details still waiting for a progress commit settle with the terminal
	// commit, the final flush.
	pending := progress.Stop()
	settled, err := finalToolResult(ctx, rt, input, call, tool, result, rep)
	if err == nil {
		err = settleTool(ctx, rt, input, call, ending, func(*SlotProgress) ToolExecutionResult { return settled }, durationMs)
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
	nested := input.Kind == toolCallNested
	if broken := structuredOutputError(tool, final); broken != "" {
		final.StructuredOutput = nil
		if nested {
			final.IsError = true
			harness = append(harness, toolDiagnostic("invalid_structured_output", broken))
		} else {
			// The model never sees structured output, so its call stands;
			// the host learns of the broken tool.
			extension := ""
			if agent, err := rt.Agent(ctx); err == nil {
				extension = toolExtension(agent, tool.Name)
			}
			if err := rt.Report(errors.New(broken), ReportFrom{Extension: extension}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
	}
	// Programs get the tool's own value: without a schema, its output. The
	// model reads only the output, so model-issued calls skip both.
	if nested && tool.StructuredOutputSchema == nil {
		final.StructuredOutput = outputValue(bounded)
	}
	// A schema tool's error result without structured output has its text
	// only in the output, which programs never get: it becomes an error
	// diagnostic, as a thrown error's message does, unless one says what went
	// wrong already.
	if nested && tool.StructuredOutputSchema != nil && final.IsError && final.StructuredOutput == nil && !hasError(final.Diagnostics, harness) {
		var texts []string
		for _, item := range bounded {
			if text, ok := item.(ai.TextContent); ok {
				texts = append(texts, text.Text)
			}
		}
		if text := strings.Join(texts, "\n"); text != "" {
			harness = append(harness, toolDiagnostic("tool_error", text))
		}
	}
	final.Output = bounded
	final.Diagnostics = append(final.Diagnostics, harness...)
	return final, nil
}

// hasError reports whether any of the lists holds an error diagnostic.
func hasError(lists ...[]ToolDiagnostic) bool {
	for _, list := range lists {
		for _, diagnostic := range list {
			if diagnostic.Severity == "error" {
				return true
			}
		}
	}
	return false
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

// settleTool commits the tool's terminal state and marks its slot done. A
// model-issued call appends its result entry and ends with the entry ID; a
// nested call writes its result to the caller's NestedCallDoc, records its
// usage, and ends with a small receipt. Nested calls this call left running
// are aborted first, so they report into their slots before the slots below
// the call leave pi.live. build receives the slot, so interruption and abort
// can report the durable partial output.
func settleTool(ctx context.Context, rt *TaskRuntime, input ToolTaskInput, call ToolHookCall, ending toolEnding, build func(slot *SlotProgress) ToolExecutionResult, durationMs *int64) error {
	// A crash in between leaves the call in execute: a safe rerun reattaches
	// to the calls, an interruption ends here. Every live nested call the
	// task owns, also those an earlier attempt made and this one never
	// called again.
	owned, err := rt.OwnedTasks(ctx)
	if err != nil {
		return err
	}
	var nestedIDs []durable.TaskID
	for _, record := range owned {
		var owner ToolTaskInput
		if record.Kind == toolKind && json.Unmarshal(record.Input, &owner) == nil && owner.Kind == toolCallNested {
			nestedIDs = append(nestedIDs, record.ID)
		}
	}
	// Read before the settling commit: no admission starts after the call
	// ended, and those underway were awaited.
	memo, err := rt.Memo(ctx, madeNestedCalls)
	if err != nil {
		return err
	}
	madeNested := string(memo) == "true"
	// Mark them all before waiting for any: one may run until a sibling is
	// cancelled.
	if err := abortAll(ctx, rt, nestedIDs); err != nil {
		return err
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		slotDraft := toolSlotDraft(live, rt.TaskID)
		var slot *SlotProgress
		if slotDraft != nil {
			snapshot, err := slotDraft.Snapshot()
			if err != nil {
				return nil, err
			}
			var decoded SlotProgress
			if err := decodeValue(snapshot, &decoded); err != nil {
				return nil, err
			}
			slot = &decoded
		}
		result := build(slot)
		var settled ToolTaskResult
		if input.Kind == toolCallNested {
			nested, err := nestedResult(rt.TaskID, result, durationMs)
			if err != nil {
				return nil, err
			}
			if nested.Usage != nil {
				if err := recordUsage(tx, rt.ConversationID, "tools", call.Name, *nested.Usage); err != nil {
					return nil, err
				}
			}
			if input.Parent == nil {
				return nil, fmt.Errorf("Nested call %s names no caller", call.ID)
			}
			// The caller is never terminal before its owned nested calls, so
			// its document is open; another task in it is a bug, not a race.
			state, err := tx.Doc(NestedCallDoc.Task(*input.Parent).Member(input.Key, nil))
			if err != nil {
				return nil, err
			}
			if id, _ := state.Get("taskId"); id != float64(rt.TaskID) {
				return nil, fmt.Errorf("Nested call %s is not in its caller's index", call.ID)
			}
			if err := setDraftField(state, "result", nested); err != nil {
				return nil, err
			}
			if slotDraft != nil && isNestedSlot(slotDraft) {
				if err := finishSlot(slotDraft); err != nil {
					return nil, err
				}
				if err := setDraftField(slotDraft, "summary", summaryOf(nested, result.Output)); err != nil {
					return nil, err
				}
			}
			settled = ToolTaskResult{Kind: toolCallNested}
		} else {
			now, err := rt.Now()
			if err != nil {
				return nil, err
			}
			entry, err := appendToolResult(tx, rt.ConversationID, call.ToolCall, result, now.UnixMilli(), durationMs)
			if err != nil {
				return nil, err
			}
			if slotDraft != nil && !isNestedSlot(slotDraft) {
				if err := finishSlot(slotDraft); err != nil {
					return nil, err
				}
				if err := slotDraft.Set("entry", float64(entry.ID)); err != nil {
					return nil, err
				}
			}
			id := entry.ID
			settled = ToolTaskResult{Kind: toolCallModel, EntryID: &id}
			if result.Control != nil && ending.status == durable.OutcomeCompleted {
				control := *result.Control
				settled.Control = &control
			}
		}
		// A call that made no nested calls, in any attempt, has no slots
		// below it; skipping the scan keeps many leaves linear.
		if madeNested {
			if err := removeNestedSlots(live, rt.TaskID); err != nil {
				return nil, err
			}
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
func fromSlot(slot *SlotProgress, code, message string) ToolExecutionResult {
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

// abortAll marks every task of ids before waiting for any, as one may run
// until a sibling is cancelled, then waits for them all; the first error
// wins.
func abortAll(ctx context.Context, rt *TaskRuntime, ids []durable.TaskID) error {
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func() { errs <- rt.AbortOwned(ctx, id) }()
	}
	var first error
	for range ids {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// nestedResult is a nested call's result as stored and returned: strict
// JSON, without the output, which only the model reads, and without
// controls, which only model-issued calls apply.
func nestedResult(taskID durable.TaskID, result ToolExecutionResult, durationMs *int64) (NestedToolExecutionResult, error) {
	nested := NestedToolExecutionResult{TaskID: taskID, IsError: result.IsError, Diagnostics: slices.Clone(result.Diagnostics), DurationMs: durationMs}
	if nested.Diagnostics == nil {
		nested.Diagnostics = []ToolDiagnostic{}
	}
	if result.Usage != nil {
		usage := *result.Usage
		nested.Usage = &usage
	}
	for _, field := range []struct {
		value any
		out   *json.RawMessage
	}{{result.StructuredOutput, &nested.StructuredOutput}, {result.Details, &nested.Details}} {
		if field.value == nil {
			continue
		}
		strict, err := toValue(field.value)
		if err != nil {
			return NestedToolExecutionResult{}, err
		}
		raw, err := json.Marshal(strict)
		if err != nil {
			return NestedToolExecutionResult{}, err
		}
		*field.out = raw
	}
	return nested, nil
}

// maxSummaryErrorChars is the error text a nested call's summary keeps.
const maxSummaryErrorChars = 500

// summaryOf is how a nested call ended, for its slot: error text, from its
// diagnostics or else its output, bounded.
func summaryOf(result NestedToolExecutionResult, out ai.ContentList) NestedToolSummary {
	summary := NestedToolSummary{IsError: result.IsError, DurationMs: result.DurationMs, Usage: result.Usage}
	if result.IsError {
		summary.Error = sliceUTF16(errorTextOf(result, out), maxSummaryErrorChars)
	}
	return summary
}

// sliceUTF16 is the first n UTF-16 code units of s, as JS's slice(0, n); a
// surrogate pair the cut would split is left out whole, as Go strings hold
// no lone surrogate.
func sliceUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r >= 0x10000 {
			width = 2
		}
		if units+width > n {
			return s[:i]
		}
		units += width
	}
	return s
}

// errorTextOf is the error messages of a nested result, else the text items
// of its output, joined with newlines.
func errorTextOf(result NestedToolExecutionResult, out ai.ContentList) string {
	var lines []string
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == "error" {
			lines = append(lines, diagnostic.Message)
		}
	}
	if len(lines) > 0 {
		return strings.Join(lines, "\n")
	}
	for _, item := range out {
		if text, ok := item.(ai.TextContent); ok {
			lines = append(lines, text.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// outputValue is what programs get of a tool's output when it declares no
// schema: one text item as its string, one image as itself, nothing as "",
// and anything else as the content list.
func outputValue(out ai.ContentList) any {
	switch {
	case len(out) == 0:
		return ""
	case len(out) > 1:
		return out
	}
	switch only := out[0].(type) {
	case ai.TextContent:
		return only.Text
	case ai.ImageContent:
		// The block alone, with its type, as a content list encodes it.
		return map[string]any{"type": "image", "data": only.Data, "mimeType": only.MimeType}
	}
	return out
}

// fallbackResult is what the caller gets for a nested call without a stored
// result: one the scheduler faulted or orphaned.
func fallbackResult(taskID durable.TaskID, name string, outcome *durable.TaskOutcome) NestedToolExecutionResult {
	status, message := "", fmt.Sprintf("Tool %s ended without a result", name)
	if outcome != nil {
		status = outcome.Status
		switch outcome.Status {
		case durable.OutcomeFaulted:
			reason := ""
			if outcome.Error != nil {
				reason = outcome.Error.Message
			}
			message = fmt.Sprintf("Tool %s failed: %s", name, reason)
		case durable.OutcomeOrphaned:
			message = fmt.Sprintf("Tool %s could not resume: %s", name, outcome.Reason)
		}
	}
	return NestedToolExecutionResult{TaskID: taskID, IsError: true, Diagnostics: harnessError(status, message).Diagnostics}
}

// checkKey rejects an explicit nested call key that could collide: keys are
// path segments of the call ID, so they hold no "/", and are never plain
// positive integers, which default keys use. "__proto__" stays reserved, as
// it was when keys indexed a JSON object.
func checkKey(key string) error {
	positive := key != "" && key[0] >= '1' && key[0] <= '9' && strings.Trim(key, "0123456789") == ""
	if key == "" || strings.Contains(key, "/") || key == "__proto__" || positive {
		raw, _ := json.Marshal(key)
		return fmt.Errorf(`Nested call key %s must be non-empty, without "/", not "__proto__", and not a positive integer`, raw)
	}
	return nil
}

// startNestedCall creates nested call key of the call parentCallID in one
// commit: its tool task, owned by the calling task, its slot, and its
// NestedCallDoc. A key already made returns that call's task when it names
// the same tool and arguments.
func startNestedCall(ctx context.Context, rt *TaskRuntime, parentCallID, name string, args map[string]any, key string, noProgress, abandonOnRestart bool) (durable.TaskID, error) {
	strict, err := strictObject(args)
	if err != nil {
		return 0, err
	}
	call := ai.ToolCall{ID: parentCallID + "/" + key, Name: name, Arguments: strict}
	var id durable.TaskID
	replayed := false
	err = rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		// Opened, or created empty for a key not made yet.
		state, err := tx.Doc(NestedCallDoc.Task(rt.TaskID).Member(key, nil))
		if err != nil {
			return nil, err
		}
		if existing, ok := state.Get("taskId"); ok {
			existingID := durable.TaskID(existing.(float64))
			record, err := tx.Task(existingID)
			if err != nil {
				return nil, err
			}
			var input ToolTaskInput
			same := record != nil && json.Unmarshal(record.Input, &input) == nil && input.Kind == toolCallNested &&
				input.Call != nil && input.Call.Name == name && jsonAlike(input.Call.Arguments, call.Arguments)
			if !same {
				return nil, fmt.Errorf("Nested call %s was already made with another tool or other arguments", call.ID)
			}
			id = existingID
			replayed = state.Has("result")
			return nil, nil
		}
		parent := rt.TaskID
		input := ToolTaskInput{Kind: toolCallNested, Parent: &parent, ParentCallID: parentCallID, Key: key, Call: &call}
		if noProgress {
			progress := false
			input.Progress = &progress
		}
		created, err := CreateTask(tx, ToolTask, input, durable.TaskOptions{
			Ownership:        durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: rt.TaskID},
			AbandonOnRestart: abandonOnRestart,
		})
		if err != nil {
			return nil, err
		}
		if err := state.Set("taskId", float64(created)); err != nil {
			return nil, err
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if live.At("nestedTools") == nil {
			if err := live.Set("nestedTools", []any{}); err != nil {
				return nil, err
			}
		}
		slot, err := toValue(NestedToolSlot{CallID: call.ID, ParentCallID: parentCallID, ParentTaskID: rt.TaskID, Name: name, TaskID: created, Arguments: strict, Status: slotPending})
		if err != nil {
			return nil, err
		}
		if _, err := live.At("nestedTools").Push(slot); err != nil {
			return nil, err
		}
		id = created
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	if replayed {
		telemetry.SpanFrom(ctx).AddEvent("pi.durable.replay", telemetry.SpanAttributes{"tool.name": name})
	}
	return id, nil
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
	// sequence numbers this invocation's calls without a key, so a rerun
	// that calls in the same order reattaches.
	sequence int
	// admissions are the nested call admissions underway; cleanup waits for
	// them, so it sees every nested call.
	admissions sync.WaitGroup
	// memoMu guards memoDone, this attempt's commit of madeNestedCalls.
	memoMu   sync.Mutex
	memoDone bool
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

func (a *toolAPI) ExecuteTool(ctx context.Context, name string, args map[string]any, options NestedCallOptions) (NestedToolExecutionResult, error) {
	a.mu.Lock()
	if a.ended {
		a.mu.Unlock()
		return NestedToolExecutionResult{}, fmt.Errorf("Tool call %s has settled", a.call.ID)
	}
	key := options.Key
	if key == "" {
		a.sequence++
		key = strconv.Itoa(a.sequence)
	} else if err := checkKey(key); err != nil {
		a.mu.Unlock()
		return NestedToolExecutionResult{}, err
	}
	// Registered now, so cleanup waits for it.
	a.admissions.Add(1)
	a.mu.Unlock()
	id, err := a.admit(ctx, name, args, key, options.NoProgress)
	if err != nil {
		return NestedToolExecutionResult{}, err
	}
	settled, err := a.rt.WaitForTask(ctx, id)
	if err != nil {
		return NestedToolExecutionResult{}, err
	}
	snapshot, err := a.rt.Snapshot(ctx, NestedCallDoc.Task(a.rt.TaskID).Member(key, nil))
	if err != nil {
		return NestedToolExecutionResult{}, err
	}
	var state NestedCallState
	if err := decodeValue(snapshot, &state); err != nil {
		return NestedToolExecutionResult{}, err
	}
	// A nested call the scheduler faulted or orphaned stored none.
	if state.Result == nil {
		return fallbackResult(id, name, settled.State.Outcome), nil
	}
	return *state.Result, nil
}

// admit commits the madeNestedCalls memo once per attempt, then starts the
// nested call; it ends the admission either way.
func (a *toolAPI) admit(ctx context.Context, name string, args map[string]any, key string, noProgress bool) (durable.TaskID, error) {
	defer a.admissions.Done()
	// The memo first, once per attempt; a failed one is tried again by the
	// next call.
	a.memoMu.Lock()
	if !a.memoDone {
		if _, err := a.rt.MemoOr(ctx, madeNestedCalls, true); err != nil {
			a.memoMu.Unlock()
			return 0, err
		}
		a.memoDone = true
	}
	a.memoMu.Unlock()
	if hook := beforeNestedAdmission.Load(); hook != nil {
		(*hook)(key)
	}
	// A caller that can rerun reattaches to its nested calls after a restart;
	// any other caller never resumes, so they are abandoned.
	return startNestedCall(ctx, a.rt, a.call.ID, name, args, key, noProgress, a.tool.Replay != ReplaySafe)
}
