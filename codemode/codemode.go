// Package codemode runs model-written JavaScript in a QuickJS sandbox whose
// only capability is calling the tools the host injects. It is the Go port of
// pi's packages/codemode.
//
// The script is an async function body: `return` and top-level `await` work.
// It sees `tools.<name>(args)` for every tool, `ALL_TOOLS`, the output helpers
// `text`, `image`, `exit` and `console.*`, `store`/`load`, and the configured
// globals; nothing else (no timers, fetch, process, require or modules). Each
// execution gets its own VM (package codemode/quickjs) on its own goroutine;
// the host side here owns the tool table, call records, output, deadline and
// cancellation, and the VM side only exchanges messages with it, as pi's
// worker and host do.
//
// pi has three launchers (worker thread, calling thread with an interrupt
// budget, remote isolate). Go needs one: a goroutine plays the worker, and
// canceling its context stops even a script spinning in wasm, so no interrupt
// budget is needed and the fingerprint reports "budget none", as pi's worker
// sandbox does.
package codemode

import (
	"context"
	"encoding/json"
	"time"
)

// Tool is a function the script can call. Tools are called as
// `tools.<id>(args)`, where <id> is Name with characters not valid in an
// identifier replaced by `_` (see Identifier), and as `tools["<name>"](args)`.
// Globals (Options.Globals) are called as `<name>(args)`, and their names must
// be identifiers or `<namespace>.<member>`.
type Tool struct {
	Name string
	// Description is shown in declarations and listed in ALL_TOOLS.
	Description string
	// InputSchema and OutputSchema are JSON Schema documents, used only to
	// render declarations; values are not validated against them.
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	// Spread (globals only): Execute receives all call arguments as an array.
	Spread bool
	// Signature (globals only): TypeScript parameter list and return type for
	// declarations, replacing the rendering from the schemas.
	Signature string
	// Execute runs the call. args is what the script passed, as JSON; nil when
	// it passed undefined. The result must be JSON-serializable; nil is
	// undefined, and a json.RawMessage is sent as is. A returned error surfaces
	// in the script as an Error with the same message. ctx is canceled when the
	// script finishes (including unawaited calls), the execution times out or
	// is aborted, or the sandbox is closed.
	Execute func(ctx context.Context, args json.RawMessage) (any, error)
}

// OutputItem is one item of the script's output, in the order produced:
// text() and console.* give text items (Console set for console.*), image()
// gives image items with base64 Data.
type OutputItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Console  bool   `json:"console,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// CallStatus is how a tool call ended.
type CallStatus string

const (
	CallOK        CallStatus = "ok"
	CallError     CallStatus = "error"
	CallCancelled CallStatus = "cancelled"
)

// Call records one tool call; calls to globals are not recorded.
type Call struct {
	Name     string
	Status   CallStatus
	Duration time.Duration
}

// ErrorKind classifies a failed execution.
type ErrorKind string

const (
	// ErrorScript: the script threw or failed to parse.
	ErrorScript ErrorKind = "script"
	// ErrorTimeout: the deadline expired; the VM was stopped.
	ErrorTimeout ErrorKind = "timeout"
	// ErrorAborted: the caller canceled or the sandbox was closed.
	ErrorAborted ErrorKind = "aborted"
	// ErrorSandbox: the VM or the bridge failed outside the script's control.
	ErrorSandbox ErrorKind = "sandbox"
)

// Error describes a failed execution. Name and Stack come from the script's
// error when Kind is ErrorScript.
type Error struct {
	Kind    ErrorKind
	Name    string
	Message string
	Stack   string
}

func (e *Error) Error() string {
	if e.Name != "" {
		return e.Name + ": " + e.Message
	}
	return e.Message
}

// StoreWrites are the keys the script changed with store(); only successful
// executions report them.
type StoreWrites struct {
	Set map[string]json.RawMessage
	// Delete lists keys stored as undefined.
	Delete []string
}

// During, in a Start run that the script itself ended, is the result the VM
// was processing when it did (0: the script's first run) and the calls issued
// by then.
type During struct {
	Seq    int
	Issued int
}

// Result is the outcome of one execution. Output and Calls are kept for failed
// executions too, up to the failure. exit() completes with an undefined Value.
type Result struct {
	OK bool
	// Value is the returned value as JSON, nil for undefined. OK only.
	Value json.RawMessage
	// Error is set when OK is false.
	Error       *Error
	Output      []OutputItem
	Calls       []Call
	StoreWrites StoreWrites
	During      *During
}

// Output limits, enforced inside the VM by the prelude: characters of text and
// base64 image data, and items, one script may produce. The host keeps all
// output until the script ends.
const (
	MaxOutputChars = 16 * 1024 * 1024
	MaxOutputItems = 100_000
	// Store limits: one value's JSON, and all stored keys and JSON together.
	MaxStoreValueChars = 256 * 1024
	MaxStoreTotalChars = 1024 * 1024
)
