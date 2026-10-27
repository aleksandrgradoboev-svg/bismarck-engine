package codemodetool

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Notices: a nested call's result can carry fields meant for whoever reads
// it — a warning that the answer merges two objects of the same name, that the
// index is stale, that rows were cut — and a script that prints only the
// fields it wanted, or prints so much that the output is truncated, drops
// them before the model sees them. The host names such fields
// (Options.Notices); the tool appends their values to the script's output
// after truncation, unless the output already shows them. Not in pi: an
// engine extension, off when no field is named.

const (
	// noticePreviewChars bounds one notice's text.
	noticePreviewChars = 500
	// maxNotices bounds the notices one script's output carries.
	maxNotices = 20
)

// notice is one named field of one nested call's result: full is its whole
// text, checked against the output; text is what the block shows, cut.
type notice struct {
	tool, field, text, full string
}

// noticesOf is the named fields of a nested call's result, top-level in its
// structured content: the content itself, the structuredContent object of an
// MCP result, or a text block of an MCP result that is a JSON object (a server
// answering with JSON as text). A result's plain text is not read — a file
// read as JSON is data, not a notice. Empty values (null, "", false) are not
// notices.
func noticesOf(fields []string, tool string, result agent.AgentToolResult) []notice {
	if len(fields) == 0 {
		return nil
	}
	var objects []map[string]any
	add := func(data []byte) {
		var object map[string]any
		if json.Unmarshal(data, &object) == nil && object != nil {
			objects = append(objects, object)
		}
	}
	if result.StructuredContent != nil {
		if data, err := json.Marshal(result.StructuredContent); err == nil {
			add(data)
		}
	}
	if len(objects) > 0 {
		top := objects[0]
		if inner, ok := top["structuredContent"].(map[string]any); ok {
			objects = append(objects, inner)
		}
		if blocks, ok := top["content"].([]any); ok {
			for _, b := range blocks {
				if block, ok := b.(map[string]any); ok && block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						add([]byte(text))
					}
				}
			}
		}
	}
	var out []notice
	for _, object := range objects {
		for _, field := range fields {
			if text, ok := noticeText(object[field]); ok {
				out = append(out, notice{tool: tool, field: field, text: utf16Truncate(text, noticePreviewChars), full: text})
			}
		}
	}
	return out
}

// noticeText is a field's value as a notice: a string as it is, anything else
// as JSON.
func noticeText(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, strings.TrimSpace(v) != ""
	case bool:
		return "true", v
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// formatNotices is the block appended to a script's output: the notices the
// output does not already show, each once, at most maxNotices. Empty when
// there is nothing to add.
func formatNotices(notices []notice, output ai.ContentList) string {
	if len(notices) == 0 {
		return ""
	}
	shown := textOf(agent.AgentToolResult{Content: output})
	var lines []string
	seen := map[notice]bool{}
	omitted := 0
	for _, n := range notices {
		if seen[n] || strings.Contains(shown, n.full) {
			continue
		}
		seen[n] = true
		if len(lines) == maxNotices {
			omitted++
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s.%s: %s", n.tool, n.field, n.text))
	}
	if len(lines) == 0 {
		return ""
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("- …%d more", omitted))
	}
	return "Tool notices (fields of nested call results the output above does not show; read them before relying on the results):\n" +
		strings.Join(lines, "\n")
}

// noticeFields is fields without empty names and repeats.
func noticeFields(fields []string) []string {
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
