package coding

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Tool calls that a tool makes while it runs (pi core/nested-tool-calls.ts,
// pi's ctx.executeTool()), for example from codemode scripts. The agent loop
// does not know about them: the session runs each one through the agent's
// tool pipeline (agent.RunToolCall) with its own hooks, emits
// tool_execution_* events with ParentToolCallID, and records the calls and
// their usage on the model-issued call's tool result message.

// Limits of the nested-call record on a tool result: arguments over the
// per-call or total size are omitted, calls beyond the count are dropped, and
// the record is marked incomplete when any of that happens.
const (
	NestedMaxCalls                = 256
	NestedMaxArgumentBytesPerCall = 8 * 1024
	NestedMaxArgumentBytesTotal   = 32 * 1024
	NestedMaxErrorChars           = 500
)

// nestedCallRecorder collects the nested calls of one model-issued tool call,
// including calls made by nested tools. It is safe for concurrent use.
type nestedCallRecorder struct {
	mu            sync.Mutex
	calls         []*ai.NestedToolCallRecord
	startedAt     map[*ai.NestedToolCallRecord]time.Time
	complete      bool
	argumentBytes int
	usage         *ai.Usage
}

func newNestedCallRecorder() *nestedCallRecorder {
	return &nestedCallRecorder{startedAt: map[*ai.NestedToolCallRecord]time.Time{}, complete: true}
}

// start records a call as it starts; nil when the call is dropped.
func (r *nestedCallRecorder) start(call ai.ToolCall) *ai.NestedToolCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) >= NestedMaxCalls {
		r.complete = false
		return nil
	}
	record := &ai.NestedToolCallRecord{ID: call.ID, Name: call.Name, Status: ai.NestedToolCallUnfinished}
	args := call.Arguments
	if args == nil {
		args = map[string]any{}
	}
	// JSON.stringify(arguments): pi's key order, its escaping.
	text, err := jstext.Stringify(ai.ToolCall{Arguments: args, ArgumentsOrder: call.ArgumentsOrder}.OrderedArguments())
	if err != nil {
		text = "{}"
	}
	data := json.RawMessage(text)
	if bytes := len(data); bytes > NestedMaxArgumentBytesPerCall || r.argumentBytes+bytes > NestedMaxArgumentBytesTotal {
		record.ArgumentsBytes = bytes
		r.complete = false
	} else {
		record.Arguments = data
		r.argumentBytes += bytes
	}
	r.calls = append(r.calls, record)
	r.startedAt[record] = time.Now()
	return record
}

func (r *nestedCallRecorder) finish(record *ai.NestedToolCallRecord, isError bool, errorText string) {
	if record == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record.Status = ai.NestedToolCallOK
	if isError {
		record.Status = ai.NestedToolCallError
	}
	started, ok := r.startedAt[record]
	if !ok {
		started = time.Now()
	}
	duration := time.Since(started).Round(time.Millisecond).Milliseconds()
	record.DurationMs = &duration
	delete(r.startedAt, record)
	if isError && errorText != "" {
		record.Error = truncateUTF16(errorText, NestedMaxErrorChars)
	}
}

func (r *nestedCallRecorder) addUsage(u ai.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// pi: this.usage = this.usage ? combineUsage(this.usage, usage) : usage —
	// the first usage as it is, then sums with its flag and token splits.
	if r.usage == nil {
		r.usage = &u
		return
	}
	*r.usage = combineUsage(*r.usage, u)
}

// snapshot copies the record so far; nil when no nested call was made.
func (r *nestedCallRecorder) snapshot() *ai.NestedToolCalls {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 && r.complete {
		return nil
	}
	out := &ai.NestedToolCalls{Calls: make([]ai.NestedToolCallRecord, len(r.calls)), Complete: r.complete}
	for i, c := range r.calls {
		out.Calls[i] = *c
		if c.Status == ai.NestedToolCallUnfinished {
			out.Complete = false
		}
	}
	return out
}

// NestedCallSummary is what the nested calls of one model-issued tool call
// leave on its tool result message.
type NestedCallSummary struct {
	// Calls becomes NestedCalls; nil when no nested call was made.
	Calls *ai.NestedToolCalls
	// Usage is the summed usage of the nested results, added to the message's.
	Usage *ai.Usage
}

// nestedHost is what the runner needs of the session.
type nestedHost struct {
	// tools are the tools nested calls resolve against.
	tools func() []agent.AgentTool
	// sequential: every nested call runs exclusively, as when the agent
	// executes tool calls sequentially.
	sequential func() bool
	// run runs the call through the tool pipeline, with hooks that know the
	// parent.
	run func(ctx context.Context, call ai.ToolCall, parentID string, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome
	// emit sends a nested call's tool_execution_* event.
	emit func(ctx context.Context, event agent.AgentEvent)
}

// callScope: calls below one model-issued call share its recorder.
type callScope struct {
	recorder *nestedCallRecorder
	nextID   int
	// holdsQueue is set inside a call that holds the exclusive queue, so its
	// own nested calls do not wait on it.
	holdsQueue bool
}

// nestedCallRunner runs nested calls (pi NestedToolCallRunner).
type nestedCallRunner struct {
	host nestedHost

	mu     sync.Mutex
	scopes map[string]*callScope
	// exclusive serializes nested calls that must not run concurrently.
	exclusive sync.Mutex
}

func newNestedCallRunner(host nestedHost) *nestedCallRunner {
	return &nestedCallRunner{host: host, scopes: map[string]*callScope{}}
}

// truncateUTF16 keeps the first max UTF-16 code units of s, as String.slice
// does, without splitting a surrogate pair into invalid UTF-8.
func truncateUTF16(s string, max int) string {
	units := 0
	for i, r := range s {
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if units+n > max {
			return s[:i]
		}
		units += n
	}
	return s
}

func textOfResult(result agent.AgentToolResult) string {
	var parts []string
	for _, c := range result.Content {
		if t, ok := c.(ai.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// execute runs name on behalf of the call callerID; the nested call gets the
// id <callerID>/<n>. Tool failures come back as IsError outcomes.
func (r *nestedCallRunner) execute(ctx context.Context, callerID, name string, args map[string]any, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome {
	r.mu.Lock()
	scope := r.scopes[callerID]
	if scope == nil {
		scope = &callScope{recorder: newNestedCallRecorder(), nextID: 1}
		r.scopes[callerID] = scope
	}
	id := callerID + "/" + strconv.Itoa(scope.nextID)
	scope.nextID++
	holdsQueue := scope.holdsQueue
	r.mu.Unlock()
	if args == nil {
		args = map[string]any{}
	}
	call := ai.ToolCall{ID: id, Name: name, Arguments: args}
	record := scope.recorder.start(call)
	r.host.emit(ctx, agent.AgentEvent{Type: agent.EvToolExecutionStart, ToolCallID: id, ToolName: name, Args: args, ParentToolCallID: callerID})

	exclusive := !holdsQueue && (r.host.sequential() || toolIsSequential(r.host.tools(), name))
	if exclusive {
		r.exclusive.Lock()
	}
	r.mu.Lock()
	r.scopes[id] = &callScope{recorder: scope.recorder, nextID: 1, holdsQueue: holdsQueue || exclusive}
	r.mu.Unlock()
	outcome := func() agent.AgentToolCallOutcome {
		defer func() {
			r.mu.Lock()
			delete(r.scopes, id)
			r.mu.Unlock()
			if exclusive {
				r.exclusive.Unlock()
			}
		}()
		return r.host.run(ctx, call, callerID, func(partial agent.AgentToolResult) {
			if onUpdate != nil {
				onUpdate(partial)
			}
			r.host.emit(ctx, agent.AgentEvent{Type: agent.EvToolExecutionUpdate, ToolCallID: id, ToolName: name, Args: args, PartialResult: partial, ParentToolCallID: callerID})
		})
	}()

	scope.recorder.finish(record, outcome.IsError, textOfResult(outcome.Result))
	// Nested results are not persisted, so their usage is only counted
	// through the recorder.
	if outcome.Result.Usage != nil {
		scope.recorder.addUsage(*outcome.Result.Usage)
	}
	r.host.emit(ctx, agent.AgentEvent{Type: agent.EvToolExecutionEnd, ToolCallID: id, ToolName: name, Result: outcome.Result, IsError: outcome.IsError, DurationMs: outcome.DurationMs, ParentToolCallID: callerID})
	return outcome
}

func toolIsSequential(tools []agent.AgentTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return t.ExecutionMode == agent.ToolSequential
		}
	}
	return false
}

// takeRecord removes and returns the record of the nested calls a
// model-issued call made; nil when it made none.
func (r *nestedCallRunner) takeRecord(toolCallID string) *NestedCallSummary {
	r.mu.Lock()
	scope := r.scopes[toolCallID]
	delete(r.scopes, toolCallID)
	r.mu.Unlock()
	if scope == nil {
		return nil
	}
	return &NestedCallSummary{Calls: scope.recorder.snapshot(), Usage: scope.recorder.usageTotal()}
}

func (r *nestedCallRecorder) usageTotal() *ai.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usage == nil {
		return nil
	}
	u := *r.usage
	return &u
}

func (r *nestedCallRunner) clear() {
	r.mu.Lock()
	clear(r.scopes)
	r.mu.Unlock()
}
