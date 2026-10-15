package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Upstream 8562bcf66 (compaction-nested-calls.test.ts): files touched by the
// nested calls recorded on a tool result count as file operations; a call
// whose arguments were omitted for size contributes nothing.
func TestCompactionFileOpsIncludeNestedCalls(t *testing.T) {
	result := ai.ToolResultMessage{
		ToolCallID: "codemode-1",
		ToolName:   "codemode",
		Content:    ai.ContentList{},
		NestedCalls: &ai.NestedToolCalls{
			Calls: []ai.NestedToolCallRecord{
				{ID: "codemode-1/1", Name: "read", Arguments: json.RawMessage(`{"path":"a.ts"}`), Status: ai.NestedToolCallOK},
				{ID: "codemode-1/2", Name: "edit", Arguments: json.RawMessage(`{"path":"b.ts","edits":[]}`), Status: ai.NestedToolCallOK},
				// pi records whatever a script passed; a string names no file.
				{ID: "codemode-1/4", Name: "read", Arguments: json.RawMessage(`"c.ts"`), Status: ai.NestedToolCallError},
				{ID: "codemode-1/3", Name: "write", ArgumentsBytes: 40000, Status: ai.NestedToolCallOK},
			},
			Complete: false,
		},
	}
	readFiles, modifiedFiles := computeFileLists([]agent.AgentMessage{result})
	texts := func(items []fileListItem) []string {
		out := []string{}
		for _, it := range items {
			out = append(out, it.text)
		}
		return out
	}
	if got := texts(readFiles); !reflect.DeepEqual(got, []string{"a.ts"}) {
		t.Errorf("readFiles = %q, want [a.ts]", got)
	}
	if got := texts(modifiedFiles); !reflect.DeepEqual(got, []string{"b.ts"}) {
		t.Errorf("modifiedFiles = %q, want [b.ts]", got)
	}
}

// Upstream 8562bcf66: pi records a nested call's arguments before validating
// them, so a session pi wrote can hold a string, an array or a number there —
// codemode's tools.read("a.ts") is recorded as "a.ts" — and pi loads it. The
// port loads it too: the tool result survives (a failed decode would drop it,
// and a resumed conversation would then see "No result provided" in its
// place), and compaction still finds the one call whose arguments name a file.
func TestSessionKeepsToolResultsWithNonObjectNestedArguments(t *testing.T) {
	const session = `{"type":"session","version":3,"id":"0199a000-0000-7000-8000-000000000001","timestamp":"2026-09-29T00:00:00.000Z","cwd":"/proj"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-29T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"run a script"}],"timestamp":1}}
{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-09-29T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"codemode-1","name":"codemode","arguments":{"code":"await tools.read('a.ts')"}}],"api":"openai-completions","provider":"p","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":2}}
{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-09-29T00:00:03.000Z","message":{"role":"toolResult","toolCallId":"codemode-1","toolName":"codemode","content":[{"type":"text","text":"done"}],"isError":false,"timestamp":3,"nestedCalls":{"calls":[{"id":"codemode-1/1","name":"read","status":"error","arguments":"a.ts","durationMs":3,"error":"Validation failed"},{"id":"codemode-1/2","name":"read","status":"ok","arguments":{"path":"b.ts"},"durationMs":0},{"id":"codemode-1/3","name":"read","status":"error","arguments":[1,2],"durationMs":0},{"id":"codemode-1/4","name":"read","status":"error","arguments":7,"durationMs":0}],"complete":true}}}
`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(session), 0o644); err != nil {
		t.Fatal(err)
	}
	messages, err := LoadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	var result *ai.ToolResultMessage
	for _, m := range messages {
		if trm, ok := messageAsToolResult(m); ok {
			result = trm
		}
	}
	if result == nil {
		t.Fatalf("the tool result was dropped; loaded %d messages", len(messages))
	}
	if result.NestedCalls == nil || len(result.NestedCalls.Calls) != 4 || string(result.NestedCalls.Calls[0].Arguments) != `"a.ts"` {
		t.Fatalf("nested calls = %+v, want the four records as written", result.NestedCalls)
	}
	readFiles, _ := computeFileLists(messages)
	if len(readFiles) != 1 || readFiles[0].text != "b.ts" {
		t.Fatalf("readFiles = %+v, want only b.ts", readFiles)
	}
}
