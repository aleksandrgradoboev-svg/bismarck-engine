package coding

import (
	"context"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi agent-session.ts afterToolCall passes the tool_result hook's usage on
// (`usage: hookResult?.usage`, upstream 2fd386840), so a hook can patch what a
// tool reported. The SDK's AfterToolCall is the Go counterpart of that hook.
func TestToolResultHookPassesUsageThrough(t *testing.T) {
	patched := &ai.Usage{Input: 5, Output: 6, TotalTokens: 11}
	wrapped := withToolResultImageNormalization(func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult {
		return &agent.AfterToolCallResult{Usage: patched}
	}, nil)
	out := wrapped(context.Background(), agent.AfterToolCallContext{
		Result: agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	})
	if out == nil || out.Usage != patched {
		t.Fatalf("hook usage dropped: got %+v", out)
	}
}

// pi agent-session.ts afterToolCall returns content, so it also returns the
// structured content to keep (upstream 8562bcf66): the tool's own, unless the
// tool_result hook replaced the content without supplying its own.
func TestToolResultHookKeepsStructuredContent(t *testing.T) {
	original := map[string]any{"value": "original"}
	replaced := map[string]any{"value": "replaced"}
	redacted := ai.ContentList{ai.TextContent{Text: "redacted"}}
	cases := []struct {
		name string
		hook *agent.AfterToolCallResult
		want any
	}{
		{"hook changes details only", &agent.AfterToolCallResult{Details: map[string]any{"note": 1}, HasDetails: true}, original},
		{"hook replaces content", &agent.AfterToolCallResult{Content: redacted, HasContent: true}, nil},
		{"hook replaces structured content", &agent.AfterToolCallResult{StructuredContent: replaced}, replaced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := withToolResultImageNormalization(func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult {
				return tc.hook
			}, nil)
			out := wrapped(context.Background(), agent.AfterToolCallContext{Result: agent.AgentToolResult{
				Content:           ai.ContentList{ai.TextContent{Text: "ok"}},
				StructuredContent: original,
			}})
			if out == nil || !reflect.DeepEqual(out.StructuredContent, tc.want) {
				t.Fatalf("structured content = %+v, want %#v", out, tc.want)
			}
		})
	}
}
