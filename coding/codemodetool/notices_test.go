package codemodetool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// ambiguousNote is the warning of a navigation server whose answer merges
// two objects of one name (a register and a catalog), the case notices were
// made for: the script printed the fields and missed the warning.
const ambiguousNote = "name is in two categories: sections below are merged; pass category for one"

// lookupTool answers as an MCP tool converted by mcpext does: structured
// content wraps the server's content blocks, and the server's JSON is the text
// of one of them. rows is how many filler rows the answer carries.
func lookupTool(t *testing.T, rows int) coding.ToolDefinition {
	answer := map[string]any{"fields": []string{"Date", "Amount"}, "note": ambiguousNote, "stale": "", "truncated": false}
	filler := make([]string, rows)
	for i := range filler {
		filler[i] = fmt.Sprintf("row %d", i)
	}
	// Sorted keys put the note between the halves: truncation, which keeps
	// the head and the tail, cuts it out.
	answer["a_rows"], answer["z_rows"] = filler[:rows/2], filler[rows/2:]
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	tool := agent.AgentTool{Name: "lookup", Label: "Lookup", Description: "Look an object up",
		Parameters:   ai.Object(),
		OutputSchema: schema(t, `{"type":"object","properties":{"content":{"type":"array"}}}`),
		Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: string(data)}},
				StructuredContent: map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}}}, nil
		}}
	return coding.ToolDefinition{Tool: tool}
}

const printFields = "const r = await tools.lookup({});\ntext(JSON.parse(r.content[0].text).fields.join(','));"

// TestANoticeTheScriptDidNotPrintReachesTheModel: the script prints the field
// it wanted; the warning in the same answer is appended, naming its tool and
// field, and empty fields (stale "", truncated false) are not.
func TestANoticeTheScriptDidNotPrintReachesTheModel(t *testing.T) {
	h := setupNotices(t, []string{"note", "stale", "truncated", "note"}, coding.SessionOptions{}, lookupTool(t, 0))
	text := resultText(t, h.run(printFields))
	if !strings.HasPrefix(text, "Date,Amount\n") || !strings.Contains(text, "- lookup.note: "+ambiguousNote) {
		t.Fatalf("text:\n%s", text)
	}
	if strings.Contains(text, "lookup.stale") || strings.Contains(text, "lookup.truncated") || strings.Count(text, ambiguousNote) != 1 {
		t.Fatalf("empty or repeated notices:\n%s", text)
	}
}

// TestNoticesAreOffUnlessNamed: without Options.Notices the output is the
// script's alone.
func TestNoticesAreOffUnlessNamed(t *testing.T) {
	h := setupNotices(t, nil, coding.SessionOptions{}, lookupTool(t, 0))
	if text := resultText(t, h.run(printFields)); text != "Date,Amount" {
		t.Fatalf("text:\n%s", text)
	}
}

// TestANoticeTheOutputShowsIsNotRepeated, and one truncation cut is: the
// script printing the whole answer shows the warning itself; printing it past
// the output budget cuts it out of the middle (the benchmark's case), and then
// it is appended after the truncated output.
func TestANoticeTheOutputShowsIsNotRepeated(t *testing.T) {
	h := setupNotices(t, []string{"note"}, coding.SessionOptions{}, lookupTool(t, 0))
	if text := resultText(t, h.run("const r = await tools.lookup({});\ntext(r.content[0].text);")); strings.Contains(text, "Tool notices") ||
		!strings.Contains(text, ambiguousNote) {
		t.Fatalf("whole answer:\n%s", text)
	}
	h = setupNotices(t, []string{"note"}, coding.SessionOptions{}, lookupTool(t, 2000))
	result := h.run("// @options: {\"max_output_tokens\": 200}\nconst r = await tools.lookup({});\ntext(r.content[0].text);")
	defer removeSpill(t, result)
	text := resultText(t, result)
	if !strings.HasPrefix(text, "Warning: truncated output") || !strings.HasSuffix(text, "- lookup.note: "+ambiguousNote) {
		t.Fatalf("truncated answer:\n%s", text)
	}
}

func removeSpill(t *testing.T, m *ai.ToolResultMessage) {
	t.Helper()
	if path := details(t, m).FullOutputPath; path != "" {
		_ = os.Remove(path)
	}
}

// TestNoticesOf: the fields are read top-level in the structured content, in
// an MCP result's structuredContent and in its JSON text blocks; a result's
// plain text, JSON or not, is not read (a file read is data); non-string
// values are JSON.
func TestNoticesOf(t *testing.T) {
	fields := []string{"note", "truncated"}
	cases := []struct {
		name   string
		result agent.AgentToolResult
		want   []string
	}{
		{"structured", agent.AgentToolResult{StructuredContent: map[string]any{"note": "a", "truncated": 3}}, []string{"note=a", "truncated=3"}},
		{"mcp structuredContent", agent.AgentToolResult{StructuredContent: map[string]any{
			"content": []any{}, "structuredContent": json.RawMessage(`{"note":"b"}`)}}, []string{"note=b"}},
		{"mcp text block", agent.AgentToolResult{StructuredContent: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": `{"truncated":"40 rows cut"}`}}}}, []string{"truncated=40 rows cut"}},
		{"bool", agent.AgentToolResult{StructuredContent: map[string]any{"truncated": true}}, []string{"truncated=true"}},
		{"plain JSON text", agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: `{"note":"c","truncated":true}`}}}, nil},
		{"plain text", agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: `note: d`}}}, nil},
		{"empty values", agent.AgentToolResult{StructuredContent: map[string]any{"note": " ", "truncated": nil}}, nil},
	}
	for _, c := range cases {
		var got []string
		for _, n := range noticesOf(fields, "x", c.result) {
			got = append(got, n.field+"="+n.text)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if noticesOf(nil, "x", agent.AgentToolResult{StructuredContent: map[string]any{"note": "a"}}) != nil {
		t.Error("no fields named, yet notices")
	}
}

// TestFormatNoticesBoundsTheBlock: one line per distinct notice, long texts
// cut, at most maxNotices lines and a count of the rest.
func TestFormatNoticesBoundsTheBlock(t *testing.T) {
	var ns []notice
	for i := range maxNotices + 3 {
		w := fmt.Sprintf("warning %d", i)
		n := notice{tool: "t", field: "note", text: w, full: w}
		ns = append(ns, n, n)
	}
	block := formatNotices(ns, ai.ContentList{ai.TextContent{Text: "warning 0 is already shown"}})
	lines := strings.Split(block, "\n")
	if len(lines) != 1+maxNotices+1 || strings.Contains(block, "t.note: warning 0\n") || lines[len(lines)-1] != "- …2 more" {
		t.Fatalf("block:\n%s", block)
	}
	long := noticesOf([]string{"note"}, "t", agent.AgentToolResult{StructuredContent: map[string]any{"note": strings.Repeat("я", 2*noticePreviewChars)}})
	if len(long) != 1 || len([]rune(long[0].text)) > noticePreviewChars+1 {
		t.Fatalf("long notice: %d runes", len([]rune(long[0].text)))
	}
	// A long notice the output shows whole is not repeated: the cut text is
	// not what the script printed.
	if block := formatNotices(long, ai.ContentList{ai.TextContent{Text: strings.Repeat("я", 2*noticePreviewChars)}}); block != "" {
		t.Fatalf("a long notice shown whole, repeated:\n%s", block)
	}
	if formatNotices(nil, nil) != "" {
		t.Fatal("a block without notices")
	}
}
