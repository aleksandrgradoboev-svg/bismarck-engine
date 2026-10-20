package codemode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
)

// A script's result as the model reads it (pi durable/src/codemode/output.ts):
// a "Script completed/failed" header with the wall time, the script's text
// items ("==> text N/M <==" when there are several), its console lines in one
// <console_output> block, the returned value, and on failure the error with
// the calls already made. Text past the token budget is cut to its start and
// end, with the complete text in a file; images are saved to files too, with
// their path next to them, because a script has no other way to hand their
// bytes on.

// outputFile writes a file of the result under a fixed name and returns its
// path.
type outputFile func(ctx context.Context, name string, data []byte) (string, error)

// outputLimits are the tool's output limits, which the Harness enforces by
// cutting without a spill file: results stay inside them.
type outputLimits struct{ maxBytes, maxLines int }

// lineMargin is the lines of the result besides the script's text: the
// header, markers, notes.
const lineMargin = 100

// DefaultMaxOutputTokens is the default token budget for script output.
const DefaultMaxOutputTokens = 10_000

var imageExtensions = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// valueText is the returned value like the script's text(): a string as is,
// other values as compact JSON.
func valueText(value json.RawMessage) string {
	var s string
	if json.Unmarshal(value, &s) == nil {
		return s
	}
	return string(value)
}

// formatItems lays out the script's output so the model can tell items
// apart: with more than one text item, each starts with a "==> text N/M <=="
// line; console lines follow all other output in one <console_output> block.
func formatItems(output []sandbox.OutputItem) ai.ContentList {
	total := 0
	for _, item := range output {
		if item.Type == "text" && !item.Console {
			total++
		}
	}
	items := ai.ContentList{}
	var consoleLines []string
	index := 0
	for _, item := range output {
		switch {
		case item.Type == "image":
			items = append(items, ai.ImageContent{Data: item.Data, MimeType: item.MimeType})
		case item.Console:
			consoleLines = append(consoleLines, item.Text)
		default:
			index++
			text := item.Text
			if total > 1 {
				text = fmt.Sprintf("==> text %d/%d <==\n%s", index, total, item.Text)
			}
			items = append(items, ai.TextContent{Text: text})
		}
	}
	if len(consoleLines) > 0 {
		items = append(items, ai.TextContent{Text: "<console_output>\n" + strings.Join(consoleLines, "\n") + "\n</console_output>"})
	}
	return items
}

// joinAdjacentText joins adjacent text items into one, each part starting on
// its own line.
func joinAdjacentText(items ai.ContentList) ai.ContentList {
	joined := ai.ContentList{}
	var parts strings.Builder
	open, lastPart := false, ""
	flush := func() {
		if open {
			joined = append(joined, ai.TextContent{Text: parts.String()})
		}
		parts.Reset()
		open, lastPart = false, ""
	}
	for _, item := range items {
		text, ok := item.(ai.TextContent)
		if !ok {
			flush()
			joined = append(joined, item)
			continue
		}
		if lastPart != "" && !strings.HasSuffix(lastPart, "\n") {
			parts.WriteString("\n")
			lastPart = "\n"
		}
		parts.WriteString(text.Text)
		open = true
		if text.Text != "" {
			lastPart = text.Text
		}
	}
	flush()
	return joined
}

// jsSlice is String.prototype.slice over UTF-16 code units; a cut surrogate
// pair becomes U+FFFD.
func jsSlice(units []uint16, from, to int) string {
	from, to = max(0, min(from, len(units))), max(0, min(to, len(units)))
	if to < from {
		return ""
	}
	return string(utf16.Decode(units[from:to]))
}

// truncate applies the token budget: when the combined text exceeds it, the
// text items become one item that keeps the start and end of the text, and
// images follow it. The complete text is written to a file.
func truncate(ctx context.Context, items ai.ContentList, maxTokens int, limits outputLimits, write outputFile) (ai.ContentList, string) {
	var texts []string
	var images ai.ContentList
	for _, item := range items {
		if text, ok := item.(ai.TextContent); ok {
			texts = append(texts, text.Text)
		} else {
			images = append(images, item)
		}
	}
	combined := strings.Join(texts, "\n")
	units := utf16.Encode([]rune(combined))
	// Within the limits too: a UTF-16 unit is at most 3 UTF-8 bytes, and the
	// rest of the result is a few short lines.
	budget := min(maxTokens*charsPerToken, limits.maxBytes/4)
	lineBudget := (limits.maxLines - lineMargin) / 2
	lines := strings.Count(combined, "\n") + 1
	if len(texts) == 0 || (len(units) <= budget && lines <= 2*lineBudget) {
		return append(ai.ContentList{}, items...), ""
	}
	headChars := budget / 2
	tailChars := budget - headChars
	headLines := strings.Split(jsSlice(units, 0, headChars), "\n")
	head := strings.Join(headLines[:min(len(headLines), lineBudget)], "\n")
	tail := ""
	if tailChars > 0 {
		tailLines := strings.Split(jsSlice(units, len(units)-tailChars, len(units)), "\n")
		tail = strings.Join(tailLines[max(0, len(tailLines)-lineBudget):], "\n")
	}
	removed := len(units) - utf16Len(head) - utf16Len(tail)
	text := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s…%d tokens truncated…%s",
		int(math.Ceil(float64(len(units))/charsPerToken)), lines, head, int(math.Ceil(float64(removed)/charsPerToken)), tail)
	path, err := write(ctx, "output.txt", []byte(combined))
	if err != nil {
		text += "\n\n[Could not save the full output: " + err.Error() + "]"
	} else {
		text += "\n\n[Full output: " + path + " (read with offset/limit)]"
	}
	return append(ai.ContentList{ai.TextContent{Text: text}}, images...), path
}

// saveImages saves each image to a file and puts a text item with its path
// before it; an image shown twice is saved once.
func saveImages(ctx context.Context, items ai.ContentList, write outputFile) ai.ContentList {
	labels := map[string]string{}
	out := ai.ContentList{}
	for _, item := range items {
		image, ok := item.(ai.ImageContent)
		if !ok {
			out = append(out, item)
			continue
		}
		label, seen := labels[image.Data]
		if !seen {
			bytes, _ := base64.StdEncoding.DecodeString(image.Data)
			kind := fmt.Sprintf("%s, %d bytes", image.MimeType, len(bytes))
			extension, known := imageExtensions[image.MimeType]
			if !known {
				extension = ".bin"
			}
			// A failed write must not discard the result of a script whose
			// calls already ran: it becomes part of the label.
			if path, err := write(ctx, fmt.Sprintf("image-%d%s", len(labels)+1, extension), bytes); err != nil {
				label = fmt.Sprintf("[Image (%s) could not be saved: %s]", kind, err.Error())
			} else {
				label = fmt.Sprintf("[Image saved to %s (%s)]", path, kind)
			}
			labels[image.Data] = label
		}
		out = append(out, ai.TextContent{Text: label}, item)
	}
	return out
}

// callSummary is one call for the failure summary: its name and status.
type callSummary struct{ name, status string }

func formatError(err *sandbox.Error, calls []callSummary) string {
	var head string
	switch err.Kind {
	case sandbox.ErrorScript:
		head = err.Stack
		if head == "" {
			name := err.Name
			if name == "" {
				name = "Error"
			}
			head = name + ": " + err.Message
		}
	case sandbox.ErrorTimeout:
		head = "Script timed out: " + err.Message
	case sandbox.ErrorAborted:
		head = "Script aborted: " + err.Message
	default:
		head = "Script sandbox failed: " + err.Message
	}
	summary := "No tool calls were made."
	if len(calls) > 0 {
		made := make([]string, len(calls))
		for i, call := range calls {
			made[i] = call.name + " (" + call.status + ")"
		}
		summary = "Tool calls made before the failure (they are not undone): " + strings.Join(made, ", ")
	}
	return head + "\n\n" + summary
}

// formatResult is the tool result's content for a script's result, and the
// path of the complete text when it was cut.
func formatResult(ctx context.Context, result sandbox.Result, wallMs float64, maxOutputTokens int, calls []callSummary, limits outputLimits, write outputFile) (ai.ContentList, string) {
	output := append([]sandbox.OutputItem{}, result.Output...)
	if result.OK && result.Value != nil {
		output = append(output, sandbox.OutputItem{Type: "text", Text: valueText(result.Value)})
	}
	items := formatItems(output)
	if !result.OK && result.Error != nil {
		items = append(items, ai.TextContent{Text: "Script error:\n" + formatError(result.Error, calls)})
	}
	truncated, fullOutputPath := truncate(ctx, joinAdjacentText(items), maxOutputTokens, limits, write)
	// After truncation, which joins the text items and moves images after
	// them, so each path stays next to its image.
	content := joinAdjacentText(saveImages(ctx, truncated, write))
	status := "Script completed"
	if !result.OK {
		status = "Script failed"
	}
	header := status + "\nWall time " + strconv.FormatFloat(wallMs/1000, 'f', 1, 64) + " seconds\nOutput:\n"
	return append(ai.ContentList{ai.TextContent{Text: header}}, content...), fullOutputPath
}
