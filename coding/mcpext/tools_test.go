package mcpext

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// Ported from pi v1.2.0 packages/coding-agent/test/mcp-extension.test.ts,
// "MCP tools" (createMcpResultSchema lands with the codemode extension).

func TestToolNames(t *testing.T) {
	if got := ToolName("docs", "search", nil); got != "mcp__docs__search" {
		t.Errorf("got %s", got)
	}
	if got := ToolName("my-server", "get.item/v2", nil); got != "mcp__my_server__get_item_v2" {
		t.Errorf("got %s", got)
	}
	long := ToolName("server", strings.Repeat("x", 100), nil)
	if len(long) != 64 || !regexp.MustCompile(`^mcp__server__x+_[0-9a-f]{8}$`).MatchString(long) {
		t.Errorf("long %s", long)
	}
	if ToolName("server", strings.Repeat("x", 100)+"y", nil) == long {
		t.Error("different long names collide")
	}
	// Names that sanitize to one already taken by another tool get a hash
	// suffix; a short one keeps its whole name before the suffix.
	taken := ToolName("s", "a_b", nil)
	second := ToolName("s", "a-b", func(name string) bool { return name == taken })
	if !regexp.MustCompile(`^mcp__s__a_b_[0-9a-f]{8}$`).MatchString(second) {
		t.Errorf("second %s", second)
	}
	// JavaScript replaces UTF-16 code units: one underscore for "é", two for
	// a character outside the BMP.
	if got := ToolName("s", "é🦅", nil); got != "mcp__s_____" {
		t.Errorf("got %s", got)
	}
}

func block(t *testing.T, text string) []mcp.ContentBlock {
	t.Helper()
	var blocks []mcp.ContentBlock
	if err := json.Unmarshal([]byte(text), &blocks); err != nil {
		t.Fatal(err)
	}
	return blocks
}

func contentJSON(c ai.ContentList) string {
	var parts []string
	for _, item := range c {
		switch v := item.(type) {
		case ai.TextContent:
			parts = append(parts, "text:"+v.Text)
		case ai.ImageContent:
			parts = append(parts, "image:"+v.MimeType+":"+v.Data)
		}
	}
	return strings.Join(parts, "|")
}

func structuredJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConvertResult(t *testing.T) {
	blocks := block(t, `[{"type":"resource_link","uri":"file:///a","name":"a"},{"type":"resource","resource":{"uri":"file:///b","text":"b text"}},{"type":"audio","data":"","mimeType":"audio/wav"}]`)
	r := ConvertResult("docs", "t", &mcp.CallToolResult{Content: blocks, StructuredContent: json.RawMessage(`{"ok":true}`), Meta: json.RawMessage(`{"trace":"x"}`)}, ConvertOptions{})
	if got := contentJSON(r.Content); got != `text:[Resource file:///a "a"]|text:b text|text:[audio audio/wav omitted]` {
		t.Errorf("content %s", got)
	}
	if r.Details != (ToolDetails{Server: "docs", Tool: "t"}) || r.IsError {
		t.Errorf("details %+v error %v", r.Details, r.IsError)
	}
	// Scripts get the server's blocks as sent, without _meta.
	if got := structuredJSON(t, r.StructuredContent); got != `{"content":[{"type":"resource_link","uri":"file:///a","name":"a"},{"type":"resource","resource":{"uri":"file:///b","text":"b text"}},{"type":"audio","mimeType":"audio/wav"}],"structuredContent":{"ok":true}}` {
		t.Errorf("structured %s", got)
	}

	r = ConvertResult("docs", "t", &mcp.CallToolResult{Content: []mcp.ContentBlock{}, StructuredContent: json.RawMessage(`{"n":1}`)}, ConvertOptions{})
	if got := contentJSON(r.Content); got != "text:{\n  \"n\": 1\n}" {
		t.Errorf("content %q", got)
	}
	r = ConvertResult("docs", "t", &mcp.CallToolResult{Content: []mcp.ContentBlock{}, StructuredContent: json.RawMessage(`[{"id":1}]`)}, ConvertOptions{})
	if got := contentJSON(r.Content); got != "text:[\n  {\n    \"id\": 1\n  }\n]" {
		t.Errorf("content %q", got)
	}
	if got := structuredJSON(t, r.StructuredContent); got != `{"content":[],"structuredContent":[{"id":1}]}` {
		t.Errorf("structured %s", got)
	}
	r = ConvertResult("docs", "t", &mcp.CallToolResult{Content: block(t, `[{"type":"text","text":"nope"}]`), IsError: true}, ConvertOptions{})
	if contentJSON(r.Content) != "text:nope" || !r.IsError || structuredJSON(t, r.StructuredContent) != `{"content":[{"type":"text","text":"nope"}],"isError":true}` {
		t.Errorf("error result %+v", r)
	}
	r = ConvertResult("docs", "t", &mcp.CallToolResult{Content: []mcp.ContentBlock{}, IsError: true}, ConvertOptions{})
	if got := contentJSON(r.Content); got != "text:MCP tool docs/t returned an error" {
		t.Errorf("content %q", got)
	}
}

func TestResourceLinksAndBinaryResources(t *testing.T) {
	type savedFile struct {
		data      string
		extension string
	}
	var saved []savedFile
	save := func(data []byte, extension string) (string, error) {
		saved = append(saved, savedFile{string(data), extension})
		return "/tmp/saved" + extension, nil
	}
	r := ConvertResult("docs", "t", &mcp.CallToolResult{Content: block(t, `[
		{"type":"resource_link","uri":"docs://guide","name":"guide","title":"The Guide","mimeType":"text/markdown","size":2048,"description":"How to use it"},
		{"type":"resource","resource":{"uri":"file:///r/report.pdf","mimeType":"application/pdf","blob":"JVBERg=="}},
		{"type":"resource","resource":{"uri":"docs://logo","mimeType":"image/png","blob":"AAAA"}}]`)},
		ConvertOptions{Save: save, ReadableResources: true})
	want := `text:[Resource docs://guide "The Guide" (text/markdown, 2.0KB): How to use it. Read it with read_mcp_resource (server "docs")]` +
		`|text:[Binary resource file:///r/report.pdf (application/pdf, 4B) saved to /tmp/saved.pdf]|image:image/png:AAAA`
	if got := contentJSON(r.Content); got != want {
		t.Errorf("content\n%s\nwant\n%s", got, want)
	}
	if len(saved) != 1 || saved[0] != (savedFile{"%PDF", ".pdf"}) {
		t.Errorf("saved %+v", saved)
	}
}

func TestLongOutputIsCutInTheMiddle(t *testing.T) {
	var saved []string
	save := func(data []byte, _ string) (string, error) {
		saved = append(saved, string(data))
		return "/tmp/full.txt", nil
	}
	lines := make([]string, 3000)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i+1)
	}
	full := strings.Join(lines, "\n")
	result := &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: full}, {Type: "image", Data: "AAAA", MimeType: "image/png"}}}
	r := ConvertResult("docs", "snapshot", result, ConvertOptions{Save: save})
	if len(r.Content) != 2 {
		t.Fatalf("content %d", len(r.Content))
	}
	text := r.Content[0].(ai.TextContent).Text
	head := "Warning: truncated output (original token count: " + strconv.Itoa(int(math.Ceil(float64(len(full))/4))) + ")\nTotal output lines: 3000\n\nline 1\nline 2\n"
	if !strings.HasPrefix(text, head) || !regexp.MustCompile(`…\d+ chars truncated…`).MatchString(text) ||
		!strings.HasSuffix(text, "line 3000\n\n[Full output: /tmp/full.txt (read it with offset/limit)]") || len(text) >= 21*1024 {
		t.Fatalf("text head %q tail %q (%d bytes)", text[:120], text[len(text)-120:], len(text))
	}
	if contentJSON(r.Content[1:]) != "image:image/png:AAAA" || r.Details != (ToolDetails{Server: "docs", Tool: "snapshot", FullOutputPath: "/tmp/full.txt"}) {
		t.Fatalf("image %v details %+v", r.Content[1], r.Details)
	}
	if len(saved) != 1 || saved[0] != full {
		t.Fatalf("saved %d", len(saved))
	}
	// Text within the limit is not saved.
	ConvertResult("docs", "small", &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}, ConvertOptions{Save: save})
	if len(saved) != 1 {
		t.Fatal("small output saved")
	}
}

func TestTruncateMiddleAtCharacterBoundaries(t *testing.T) {
	text := strings.Repeat("é", 20_000) + "end"
	out, truncated, _, _ := truncateMiddle(text, 1001)
	if !truncated || strings.Contains(out, "�") || !utf8.ValidString(out) || !strings.HasSuffix(out, "end") {
		t.Fatalf("truncated %v valid %v", truncated, utf8.ValidString(out))
	}
	parts := regexp.MustCompile(`…(\d+) chars truncated…`).FindStringSubmatchIndex(out)
	headPart, tailPart := out[:parts[0]], out[parts[1]:]
	removed := 0
	for _, c := range out[parts[2]:parts[3]] {
		removed = removed*10 + int(c-'0')
	}
	if len(headPart) > 500 || len(tailPart) > 501 || utf8.RuneCountInString(headPart)+utf8.RuneCountInString(tailPart)+removed != utf8.RuneCountInString(text) {
		t.Fatalf("head %d tail %d removed %d", len(headPart), len(tailPart), removed)
	}
}
