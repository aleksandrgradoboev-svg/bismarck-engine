package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// Who may call a tool.
const (
	// CallerModel: the model, through a tool call.
	CallerModel = "model"
	// CallerTools: other tools, through nested calls.
	CallerTools = "tools"
)

// Whether an interrupted execution may rerun on recovery.
const (
	ReplaySafe   = "safe"
	ReplayUnsafe = "unsafe"
)

// ToolExecutionMode is whether the tools of one round run at once or one after
// another in call order.
type ToolExecutionMode string

const (
	ToolExecutionParallel   ToolExecutionMode = "parallel"
	ToolExecutionSequential ToolExecutionMode = "sequential"
)

// OutputLimits bound the output a tool call retains for the model.
type OutputLimits struct {
	MaxBytes int
	MaxLines int
	// Retain is "head" or "tail".
	Retain string
}

// ToolRegistration is an executable tool registered in a registry. Only the
// embedded ai.Tool enters the transcript; the Harness validates arguments
// against its Parameters before Execute.
type ToolRegistration struct {
	ai.Tool
	// Replay is whether an interrupted execution may rerun on recovery:
	// ReplaySafe or ReplayUnsafe (default).
	Replay string
	// ExecutionMode defaults to the settings' ToolExecution. One sequential
	// call makes its whole round sequential.
	ExecutionMode ToolExecutionMode
	// PrepareArguments repairs arguments models commonly get wrong before
	// validation. It must be pure and must not mutate args: it runs again when
	// a call is retried before its intent is recorded.
	PrepareArguments func(args any) (any, error)
	// StructuredOutputSchema is the schema of the structured output programs
	// that call the tool receive.
	StructuredOutputSchema *ai.Schema
	// Callers says who may call the tool: CallerModel, CallerTools, or both
	// (nil).
	Callers []string
	// Deferred tools are not offered to the model until loaded.
	Deferred bool
	// OutputLimits bound the retained output; nil keeps the defaults.
	OutputLimits *OutputLimits
	// Execute runs one call; an error is a thrown one, answered with a
	// tool_error result.
	Execute func(ctx context.Context, args map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error)
}

// callableBy reports whether caller may call the tool.
func (t *ToolRegistration) callableBy(caller string) bool {
	if t.Callers == nil {
		return true
	}
	for _, c := range t.Callers {
		if c == caller {
			return true
		}
	}
	return false
}

// ToolExecutionAPI is the operations available to one tool invocation (pi
// ToolExecutionApi). It is an interface so a tool wrapper can embed the API
// it was given and replace members, as pi's wrappers spread it: one that
// replaces Output and transforms text must also replace OutputWindow with
// nil, so skipped text cannot bypass its transform. Every operation that
// writes fails once the call has settled.
type ToolExecutionAPI interface {
	TaskID() durable.TaskID
	ConversationID() durable.ConversationID
	CallID() string
	// Registry is the tool task's phase snapshot.
	Registry() *RegistrySnapshot
	// Agent is the calling conversation's agent, as the tool task's phase
	// resolved it.
	Agent(ctx context.Context) (*Agent, error)
	// Models is the Harness model access, read at each call.
	Models() ai.Models
	// Env is built by Options.Env for this call; nil without an
	// environment.
	Env() env.ExecutionEnv
	// Now is the Harness clock, for times a rerun after a restart compares
	// against.
	Now() (time.Time, error)
	// Output appends running output; it becomes the result's Output when the
	// result omits it. skipped is output omitted before the chunk, as
	// reported by an environment given OutputWindow.
	Output(chunk []byte, skipped *output.Skip) error
	// OutputWindow is the tail this call's output keeps and the pace of its
	// progress commits, for env.ExecOptions.Window; nil when the tool keeps
	// the head of its output, which cannot accept skips.
	OutputWindow() *env.OutputWindow
	// RetainedOutput is the output retained so far, as the model will see it
	// when the result omits Output, and whether earlier output was dropped.
	RetainedOutput() (text string, truncated bool, err error)
	// Diagnostic records a model-visible remark about this call.
	Diagnostic(diagnostic ToolDiagnostic) error
	// Details replaces the running details and returns once a progress
	// commit holds them; cancelling ctx stops only the wait. The last value
	// becomes the result's details when the result omits them.
	Details(ctx context.Context, value any) error
	// Commit runs change in one commit of the call's task.
	Commit(ctx context.Context, change func(tx *durable.Tx) error) error
	// Memo reads a durable memo of the call's task; MemoOr stores candidate
	// when it is absent.
	Memo(ctx context.Context, name string) (json.RawMessage, error)
	MemoOr(ctx context.Context, name string, candidate any) (json.RawMessage, error)
	// CreateTask creates a task in one commit. A child task of a tool that
	// is not replay-safe defaults to AbandonOnRestart: the tool never
	// resumes after a restart, so nothing awaits the child then.
	CreateTask(ctx context.Context, task Task, input any, options durable.TaskOptions) (durable.TaskID, error)
	GetTask(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error)
	WaitForTask(ctx context.Context, id durable.TaskID) (durable.TaskRecord, error)
	Snapshot(ctx context.Context, ref durable.DocRef) (map[string]any, error)
	SnapshotAsOf(ctx context.Context, ref durable.DocRef, at durable.EntryID) (map[string]any, error)
	WatchDoc(ctx context.Context, ref durable.DocRef) (*durable.DocumentWatch, error)
	// ExecuteTool runs tool name, among the tools this call may call, as a
	// nested call of this one in its own tool task, and waits for its
	// result. A key already made returns that call when it names the same
	// tool and arguments. Cancelling ctx stops only the wait: the call runs
	// on, and its key returns its result.
	ExecuteTool(ctx context.Context, name string, args map[string]any, options NestedCallOptions) (NestedToolExecutionResult, error)
}

// NestedCallOptions are the options of ExecuteTool.
type NestedCallOptions struct {
	// Key names the nested call within this call; empty takes its position
	// among this attempt's calls. An explicit key is non-empty, has no "/",
	// is not "__proto__", and is not a positive integer.
	Key string
	// NoProgress commits no running output, details or diagnostics to the
	// nested call's slot.
	NoProgress bool
	// OnAdmitted, when set, is called once the nested call is admitted (its
	// task exists), before ExecuteTool waits for it; not when admission
	// fails. A caller that starts calls in order waits for it before
	// starting the next.
	OnAdmitted func(id durable.TaskID)
}

// NestedToolExecutionResult is what ExecuteTool returns, exactly as the
// nested call stored it: without the output, which only the model reads, and
// without controls, which only model-issued calls apply. StructuredOutput is
// the tool's value for programs (a tool without a schema gives its output:
// one text as a string, one image as itself, nothing as "", else the content
// list) and Details are for UIs; both are JSON to decode, nil when absent.
type NestedToolExecutionResult struct {
	TaskID           durable.TaskID   `json:"taskId"`
	StructuredOutput json.RawMessage  `json:"structuredOutput,omitempty"`
	IsError          bool             `json:"isError"`
	Details          json.RawMessage  `json:"details,omitempty"`
	Diagnostics      []ToolDiagnostic `json:"diagnostics"`
	Usage            *ai.Usage        `json:"usage,omitempty"`
	DurationMs       *int64           `json:"durationMs,omitempty"`
}

// ToolHookCall is a tool call as hooks see it; Parent is set for a nested
// call.
type ToolHookCall struct {
	ai.ToolCall
	Parent *ToolCallParent
}

// ToolCallParent is the call that made a nested call.
type ToolCallParent struct {
	TaskID durable.TaskID
	CallID string
}

// BeforeToolDecision is a BeforeTool hook's decision: Block blocks the call
// with its reason; otherwise non-nil Arguments replace the call's arguments.
type BeforeToolDecision struct {
	Arguments map[string]any
	Block     *string
}

// ToolHooks are the hooks of the built-in tool task (pi ToolHooks); register
// a *ToolHooks with Hook(ToolTask, …). A nil field is no hook.
type ToolHooks struct {
	// BeforeTool runs before intent; the first Block wins, otherwise
	// Arguments replace the call's arguments. An error blocks with its text.
	BeforeTool func(ctx context.Context, call ToolHookCall, rt *TaskRuntime) (*BeforeToolDecision, error)
	// AfterTool runs after execution, before the result is committed; a
	// non-nil result replaces it.
	AfterTool func(ctx context.Context, call ToolHookCall, result ToolExecutionResult, rt *TaskRuntime) (*ToolExecutionResult, error)
}

// ToolControl is the post-tools controls a tool result requests.
type ToolControl struct {
	AddTools  []string `json:"addTools,omitempty"`
	Terminate bool     `json:"terminate,omitempty"`
	Handoff   *string  `json:"handoff,omitempty"`
}

// ToolDiagnostic is a remark about a call for the model and the UI, such as
// truncation or a spill path; never part of the tool's data.
type ToolDiagnostic struct {
	// Severity is "info", "warn" or "error".
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
}

// ToolExecutionResult is what a tool returns, on three channels: Output for
// the model, StructuredOutput for programs that call the tool, Details for
// UIs.
type ToolExecutionResult struct {
	// Output is for the model; nil keeps the retained running output.
	Output ai.ContentList
	// StructuredOutput is for programs; required unless IsError when the tool
	// declares StructuredOutputSchema, and not allowed without one.
	StructuredOutput any
	IsError          bool
	// Details are for UIs; nil keeps the last running details.
	Details any
	// Diagnostics are added after those recorded through the API.
	Diagnostics []ToolDiagnostic
	// Usage is the spend of the execution itself, such as a model call.
	Usage   *ai.Usage
	Control *ToolControl
}
