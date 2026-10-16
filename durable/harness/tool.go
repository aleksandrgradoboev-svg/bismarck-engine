package harness

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
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
	// Execute runs one call.
	Execute func(ctx context.Context, args map[string]any, api *ToolExecutionAPI) (ToolExecutionResult, error)
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

// ToolExecutionAPI is the operations available to one tool invocation.
//
// It is filled in by the tool task, which arrives with a later stage of the
// harness port; until then it only names the call.
type ToolExecutionAPI struct {
	TaskID         durable.TaskID
	ConversationID durable.ConversationID
	CallID         string
}

// ToolControl is the post-tools controls a tool result requests.
type ToolControl struct {
	AddTools  []string
	Terminate bool
	Handoff   *string
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
