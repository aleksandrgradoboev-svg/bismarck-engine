package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi agent-loop.test.ts "records how long execute() took on the tool result,
// excluding hooks" (#10549, upstream 36a686ee).
func TestToolResultRecordsExecuteDuration(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ToolParallel, ToolSequential} {
		t.Run(string(mode), func(t *testing.T) { testToolResultRecordsExecuteDuration(t, mode) })
	}
}

func testToolResultRecordsExecuteDuration(t *testing.T, mode ToolExecutionMode) {
	tool := AgentTool{
		Name:       "echo",
		Parameters: ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			time.Sleep(30 * time.Millisecond)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: params["value"].(string)}}}, nil
		},
	}
	var mu sync.Mutex
	var ends []AgentEvent
	a := NewAgent(AgentOptions{
		InitialState:  &AgentState{Model: testModel, Tools: []AgentTool{tool}},
		ToolExecution: mode,
		StreamFn: scriptedStream(
			&ai.AssistantMessage{Content: ai.ContentList{
				ai.ToolCall{ID: "ran", Name: "echo", Arguments: map[string]any{"value": "a"}},
				ai.ToolCall{ID: "blocked", Name: "echo", Arguments: map[string]any{"value": "b"}},
			}, StopReason: ai.StopToolUse, Model: "faux"},
			&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
		),
		BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
			time.Sleep(100 * time.Millisecond)
			if c.ToolCall.ID == "blocked" {
				return &BeforeToolCallResult{Block: true, Reason: "no"}
			}
			return nil
		},
	})
	a.Subscribe(func(_ context.Context, e AgentEvent) error {
		if e.Type == EvToolExecutionEnd {
			mu.Lock()
			ends = append(ends, e)
			mu.Unlock()
		}
		return nil
	})
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	results := map[string]ai.ToolResultMessage{}
	for _, m := range a.State().Messages {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			results[tr.ToolCallID] = tr
		}
	}
	ran, blocked := results["ran"], results["blocked"]
	if ran.DurationMs == nil || *ran.DurationMs < 25 || *ran.DurationMs >= 100 {
		t.Fatalf("ran: durationMs = %v, want execute's ~30ms without the 100ms hook", ran.DurationMs)
	}
	if !blocked.IsError || blocked.DurationMs != nil {
		t.Fatalf("blocked: isError %v, durationMs %v; want an error with no duration", blocked.IsError, blocked.DurationMs)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range ends {
		want := results[e.ToolCallID].DurationMs
		if (e.DurationMs == nil) != (want == nil) || (want != nil && *e.DurationMs != *want) {
			t.Fatalf("tool_execution_end %s: durationMs %v, message %v", e.ToolCallID, e.DurationMs, want)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("%d tool_execution_end events", len(ends))
	}
}
