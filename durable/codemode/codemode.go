// Package codemode is durable code mode (pi durable/src/codemode): a tool
// that runs model-written JavaScript in the QuickJS sandbox of the codemode
// package, whose script survives a restart.
//
// A script is in-memory VM state, so a restart while it waits for the last of
// 200 reads would lose it and every read it made. The sandbox replays
// deterministically (codemode.Sandbox.Start): the VM runs only when the host
// hands it a result, and given the same start data, clock, seed and results
// in the same order, it makes the same calls with the same arguments. The
// tool is replay-safe: after a restart its execution runs again, starts the
// script again from the stored start data, and hands it the results it got
// before, in the order it got them, from a journal written before each
// delivery. Nested tool calls reattach to their tasks by key ("c<VM call
// id>"), so they never run twice unless their own replay policy reruns them;
// their results come from harness.NestedCallDoc. Then the script continues
// live.
//
// Documents, scoped to the code mode call's task and gone when it settles:
//   - pi.codemode.run: the start data, written once before the VM starts, and
//     the outcome, written once when the call decides it (with the store
//     writes, in one transaction), so a rerun after that returns it without a
//     VM.
//   - pi.codemode.journal: one entry per delivered result, in delivery order:
//     the VM call ID, the clock value, the calls the VM had issued after
//     processing it (once acknowledged), and a global's outcome, which
//     nothing else stores.
//
// A call is launched only once the count of calls issued in the drain that
// made it is committed, so every key an earlier attempt admitted is at most
// its last recorded count; a replay checks every recorded count, so when it
// releases its calls it has made every call an earlier attempt could have
// admitted, and later calls are new. A rerun refuses to replay when this
// package's version or the VM's fingerprint changed, when the deadline passed,
// or when the script diverges: it makes another call, or calls in another
// order, than the journal says. Each ends the call with an error result.
//
// The models.* globals of pi are not ported: the engine has no classifier or
// image generation in its model access. The tool behaves as pi's with
// models: false.
package codemode

import (
	"context"
	"encoding/json"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// ModuleVersion is the version of everything in this package that turns
// stored results into what a script sees: result mapping, globals, start
// data. Bump it when that changes, so a call started before an upgrade is not
// replayed by different code.
const ModuleVersion = 2

// ToolName is the code mode tool's name.
const ToolName = "codemode"

// internalPrefix is the reserved prefix of code mode's internal tools, which
// are never listed to scripts or the model (pi's models.* tools use it).
const internalPrefix = "pi.codemode."

// RunDoc holds a call's start data, output directory and decided outcome.
var RunDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:           "pi.codemode.run",
	Version:        1,
	Scope:          durable.ScopeTask,
	Initial:        func(any) map[string]any { return map[string]any{} },
	CheckpointWhen: durable.NeverCheckpoint,
})

// JournalDoc holds a call's delivered results in delivery order.
var JournalDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:           "pi.codemode.journal",
	Version:        1,
	Scope:          durable.ScopeTask,
	Initial:        func(any) map[string]any { return map[string]any{"entries": []any{}} },
	CheckpointWhen: durable.NeverCheckpoint,
})

// StoreDoc is what scripts keep with store() and read with load(), per
// conversation. Rewindable and copied as of the fork point, so a fork sees
// the values as they were when its entry was committed.
var StoreDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:           "pi.codemode.store",
	Version:        1,
	Scope:          durable.ScopeConversation,
	History:        durable.HistoryRewindable,
	Fork:           durable.ForkAsOf,
	Initial:        func(any) map[string]any { return map[string]any{"values": map[string]any{}} },
	CheckpointWhen: durable.NeverCheckpoint,
})

// declaredTool is one tool as the script sees it: tools.<JSName>.
// Structured: it declares a structured output schema, so an error result
// with a value resolves with it. Hidden: an internal tool, reached only
// through a global; never in tools, ALL_TOOLS, or tool search.
type declaredTool struct {
	Name        string `json:"name"`
	JSName      string `json:"jsName"`
	Description string `json:"description"`
	Structured  bool   `json:"structured,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
}

type startNamespace struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Tools        []string `json:"tools"`
}

type startGlobal struct {
	Name   string `json:"name"`
	Spread bool   `json:"spread"`
}

// startData is what a call starts with; a rerun starts the VM from exactly
// this.
type startData struct {
	Version   int     `json:"version"`
	StartedAt float64 `json:"startedAt"`
	Seed      uint32  `json:"seed"`
	// Fingerprint is the VM's, recorded by the first run before any call is
	// admitted.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Deadline is StartedAt + timeout_ms when the script set one.
	Deadline        *float64         `json:"deadline,omitempty"`
	Tools           []declaredTool   `json:"tools"`
	Namespaces      []startNamespace `json:"namespaces"`
	Globals         []startGlobal    `json:"globals"`
	MaxOutputTokens int              `json:"maxOutputTokens"`
	Store           map[string]any   `json:"store"`
}

// storedResult is a decided outcome as pi.codemode.run keeps it.
type storedResult struct {
	Output  ai.ContentList  `json:"output"`
	IsError bool            `json:"isError,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// journalOutcome is a delivered outcome as the journal keeps it; Value nil is
// undefined.
type journalOutcome struct {
	OK         bool            `json:"ok"`
	Value      json.RawMessage `json:"value,omitempty"`
	Message    string          `json:"message,omitempty"`
	Properties map[string]any  `json:"properties,omitempty"`
}

type journalGlobal struct {
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args,omitempty"`
	Outcome journalOutcome  `json:"outcome"`
}

// journalEntry is one delivered result: the VM's call ID (its result is the
// entry's position + 1), the VM clock while it processes it, the calls the
// VM had issued after processing it once acknowledged, and a global's call
// and outcome (a tool's result is its nested call's).
type journalEntry struct {
	ID          int            `json:"id"`
	At          float64        `json:"at"`
	IssuedAfter *int           `json:"issuedAfter,omitempty"`
	Global      *journalGlobal `json:"global,omitempty"`
}

// NestedCall is one tool call a script made, as the code mode call's details
// show it.
type NestedCall struct {
	// ID is the VM's number for the call; its nested call ID is
	// "<callId>/c<id>".
	ID   int    `json:"id"`
	Name string `json:"name"`
	// Args is compact JSON of the arguments, cut for display.
	Args string `json:"args"`
	// Status is "running", "ok", "error" or "cancelled".
	Status     string `json:"status"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	// Error is the error text, cut for display.
	Error string `json:"error,omitempty"`
}

// ToolDetails are the code mode call's details: its tool calls in the order
// it made them, with their status (calls to globals are not listed), and the
// file with the complete text output when it was cut to its token budget.
type ToolDetails struct {
	Calls          []NestedCall `json:"calls"`
	FullOutputPath string       `json:"fullOutputPath,omitempty"`
}

// Global is a function scripts call as a top-level global; its result is
// journaled. Execute receives the script's arguments as JSON (nil for
// undefined; with Spread, the argument list) and returns a JSON-serializable
// value, nil for undefined.
type Global struct {
	Name    string
	Spread  bool
	Execute func(ctx context.Context, args json.RawMessage, api harness.ToolExecutionAPI) (any, error)
}

// Sandbox runs scripts deterministically; *codemode.Sandbox is one.
type Sandbox interface {
	Start(ctx context.Context, code string, options sandbox.RunOptions) (*sandbox.Run, error)
}

// Options configure the code mode tool and extension.
type Options struct {
	// Sandbox runs the scripts.
	Sandbox Sandbox
	// Namespaces group tools by name prefix, such as MCP servers, for the
	// codemode section and tool search.
	Namespaces []Namespace
	// InlineBudget is the estimated tokens the tool declarations in the
	// codemode section may use; 0 is DefaultInlineBudget.
	InlineBudget int
	// Globals are host functions scripts call as globals.
	Globals []Global
}

// outputLimitsOf is the tool's output limits: outputs are bounded by the
// script's own token budget, and the Harness's limits only stop a runaway.
var toolOutputLimits = outputLimits{maxBytes: 4 * 1024 * 1024, maxLines: 100_000}

// NewTool is the code mode tool. Scripts call the conversation's callable
// tools as tools.<name>(args).
func NewTool(options Options) *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        ToolName,
			Description: description(),
			Parameters:  ai.Object(ai.Prop("code", ai.String("Raw JavaScript source."))),
			// Capable models write the script as raw text instead of a
			// JSON-escaped string; ai applies it only for them.
			ConstrainedSampling: &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingGrammar, Variants: ai.GrammarVariants{OpenAILark: sandbox.SourceGrammar}},
		},
		OutputLimits: &harness.OutputLimits{MaxBytes: toolOutputLimits.maxBytes, MaxLines: toolOutputLimits.maxLines},
		Callers:      []string{harness.CallerModel},
		Replay:       harness.ReplaySafe,
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			code, _ := args["code"].(string)
			return newCall(options, api).execute(ctx, code)
		},
	}
}

// NewExtension is the codemode extension: the code mode tool, and the
// codemode prompt section listing the tools scripts can call. Each call
// builds a fresh extension.
func NewExtension(options Options) *harness.Extension {
	budget := options.InlineBudget
	if budget == 0 {
		budget = DefaultInlineBudget
	}
	return &harness.Extension{
		Name:  "codemode",
		Tools: []*harness.ToolRegistration{NewTool(options)},
		Sections: []*harness.PromptSection{harness.Section("codemode", func(_ context.Context, input harness.PromptInput) (string, bool, error) {
			// Deferred tools are found with searchTools(), as the coding
			// agent leaves them out too.
			var tools []*harness.ToolRegistration
			for _, tool := range input.Agent.Callable {
				if !hasInternalPrefix(tool.Name) && !tool.Deferred {
					tools = append(tools, tool)
				}
			}
			text, ok := section(tools, options.Namespaces, budget)
			return text, ok, nil
		})},
	}
}

func hasInternalPrefix(name string) bool {
	return len(name) >= len(internalPrefix) && name[:len(internalPrefix)] == internalPrefix
}
