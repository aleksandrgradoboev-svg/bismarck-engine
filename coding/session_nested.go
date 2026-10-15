package coding

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Nested tool calls in the session (pi AgentSession._executeNestedToolCall and
// the message_start handler that records them). A tool that calls other tools,
// codemode's for example, calls ExecuteTool with its own tool call id.

// combineUsage is pi's combineUsage: the sum of two usages, keeping the
// optional token splits when either side reports them (in Go they are zero
// when unreported, so summing keeps them).
func combineUsage(first, second ai.Usage) ai.Usage {
	sum := first
	addUsage(&sum, second)
	sum.CacheWrite1h += second.CacheWrite1h
	sum.Reasoning += second.Reasoning
	return sum
}

// nestedRunner is created on the first nested call.
func (s *Session) nestedRunner() *nestedCallRunner {
	s.nestedMu.Lock()
	defer s.nestedMu.Unlock()
	if s.nested == nil {
		s.nested = newNestedCallRunner(nestedHost{
			tools:      s.CallableTools,
			sequential: func() bool { return s.Agent.ToolExecution == agent.ToolSequential },
			run:        s.runNestedToolCall,
			emit: func(ctx context.Context, event agent.AgentEvent) {
				lock := s.Agent.ToolLock()
				lock.Lock()
				defer lock.Unlock()
				_ = s.Agent.Notify(ctx, event)
			},
		})
	}
	return s.nested
}

// ExecuteTool runs a call that the tool call parentToolCallID makes while it
// runs (pi ctx.executeTool). The call goes through the agent's tool pipeline —
// argument validation, the BeforeToolCall and AfterToolCall hooks, which see
// ParentToolCallID — against CallableTools, emits tool_execution_* events with
// ParentToolCallID, and is recorded on the parent's tool result message along
// with its usage. Its id is "<parentToolCallID>/<n>". Tool failures come back
// as an outcome with IsError set. onUpdate, when set, receives the tool's
// partial results.
func (s *Session) ExecuteTool(ctx context.Context, parentToolCallID, name string, args map[string]any, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome {
	return s.nestedRunner().execute(ctx, parentToolCallID, name, args, onUpdate)
}

func lastAssistantMessage(messages []agent.AgentMessage) *ai.AssistantMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if am, ok := messageAsAssistant(messages[i]); ok {
			return am
		}
	}
	return nil
}

// runNestedToolCall runs one nested call with the session's hooks. The hooks
// run under the agent's ToolLock, as the loop runs its own, so they never
// interleave with the loop's; the tool's Execute runs outside it.
func (s *Session) runNestedToolCall(ctx context.Context, call ai.ToolCall, parentID string, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome {
	st := s.Agent.State()
	assistant := lastAssistantMessage(st.Messages)
	if assistant == nil {
		return agent.AgentToolCallOutcome{
			ToolCall: call,
			Result:   agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "No assistant message issued this call"}}, Details: map[string]any{}},
			IsError:  true,
		}
	}
	lock := s.Agent.ToolLock()
	hooks := agent.ToolCallHooks{ParentToolCallID: parentID}
	if before := s.Agent.BeforeToolCall; before != nil {
		hooks.BeforeToolCall = func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			lock.Lock()
			defer lock.Unlock()
			return before(ctx, c)
		}
	}
	if after := s.Agent.AfterToolCall; after != nil {
		hooks.AfterToolCall = func(ctx context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult {
			lock.Lock()
			defer lock.Unlock()
			return after(ctx, c)
		}
	}
	return agent.RunToolCall(ctx, call, agent.RunToolCallOptions{
		ToolCallHooks:    hooks,
		Tools:            s.CallableTools(),
		AssistantMessage: assistant,
		Context:          &agent.AgentContext{Messages: st.Messages, Tools: st.Tools},
		OnUpdate:         onUpdate,
	})
}

// attachNestedCalls records the calls a tool made through ExecuteTool, and
// their usage, on its result message (pi does this at message_start).
func (s *Session) attachNestedCalls(m *ai.ToolResultMessage) {
	s.nestedMu.Lock()
	runner := s.nested
	s.nestedMu.Unlock()
	if runner == nil {
		return
	}
	summary := runner.takeRecord(m.ToolCallID)
	if summary == nil {
		return
	}
	if summary.Calls != nil {
		m.NestedCalls = summary.Calls
	}
	if summary.Usage != nil {
		if m.Usage != nil {
			u := combineUsage(*m.Usage, *summary.Usage)
			m.Usage = &u
		} else {
			u := *summary.Usage
			m.Usage = &u
		}
	}
}

// clearNestedCalls drops the records of a finished run (pi clears them at
// agent_end).
func (s *Session) clearNestedCalls() {
	s.nestedMu.Lock()
	runner := s.nested
	s.nestedMu.Unlock()
	if runner != nil {
		runner.clear()
	}
}
