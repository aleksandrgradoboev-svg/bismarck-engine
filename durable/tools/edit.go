package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/diff"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/editmatch"
)

// EditToolDetails are edit's details: a display diff with line numbers, a
// unified patch, and the first changed line of the new file.
type EditToolDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine *int   `json:"firstChangedLine,omitempty"`
}

func isSingleEditInput(value any) bool {
	edit, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, oldOK := edit["oldText"].(string)
	_, newOK := edit["newText"].(string)
	return oldOK && newOK
}

// prepareEditArguments repairs shapes models commonly send: edits as a JSON
// string or as a single edit object, and a top-level oldText/newText pair.
// It works on a copy; the call's arguments stay unchanged.
func prepareEditArguments(input any) (any, error) {
	original, ok := input.(map[string]any)
	if !ok {
		return input, nil
	}
	args := make(map[string]any, len(original))
	for k, v := range original {
		args[k] = v
	}
	if text, ok := args["edits"].(string); ok {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			if list, ok := parsed.([]any); ok {
				args["edits"] = list
			} else if isSingleEditInput(parsed) {
				args["edits"] = []any{parsed}
			}
		}
	} else if isSingleEditInput(args["edits"]) {
		args["edits"] = []any{args["edits"]}
	}

	oldText, oldOK := args["oldText"].(string)
	newText, newOK := args["newText"].(string)
	if !oldOK || !newOK {
		return args, nil
	}
	var edits []any
	if list, ok := args["edits"].([]any); ok {
		edits = append(edits, list...)
	}
	edits = append(edits, map[string]any{"oldText": oldText, "newText": newText})
	delete(args, "oldText")
	delete(args, "newText")
	args["edits"] = edits
	return args, nil
}

// causedError is an error with its own message and a cause it unwraps to,
// as JS's new Error(message, {cause}).
type causedError struct {
	message string
	cause   error
}

func (e *causedError) Error() string { return e.message }
func (e *causedError) Unwrap() error { return e.cause }

func editAccessError(path string, err error) error {
	code := string(env.Code(err))
	if code == "" {
		code = string(env.CodeUnknown)
	}
	return &causedError{message: fmt.Sprintf("Could not edit file: %s. Error code: %s.", path, code), cause: err}
}

// NewEditTool is the edit tool (pi createEditTool): exact-text replacements
// in one file, each matched against the original, serialized with other edits
// and writes of the file. A byte-order mark and CRLF line endings survive.
func NewEditTool() *harness.ToolRegistration {
	edit := ai.Object(
		ai.Prop("oldText", ai.String("Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call.")),
		ai.Prop("newText", ai.String("Replacement text for this targeted edit.")),
	)
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        "edit",
			Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
			Parameters: ai.Object(
				ai.Prop("path", ai.String("Path to the file to edit (relative or absolute)")),
				ai.Prop("edits", ai.ArrayOf(edit, "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.")),
			),
		},
		PrepareArguments: prepareEditArguments,
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			path, _ := args["path"].(string)
			list, _ := args["edits"].([]any)
			if len(list) == 0 {
				return harness.ToolExecutionResult{}, fmt.Errorf("Edit tool input is invalid. edits must contain at least one replacement.")
			}
			edits := make([]editmatch.Edit, len(list))
			for i, item := range list {
				m, _ := item.(map[string]any)
				oldText, _ := m["oldText"].(string)
				newText, _ := m["newText"].(string)
				edits[i] = editmatch.Edit{OldText: oldText, NewText: newText}
			}
			e, err := requireEnv(api)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			absolute, err := resolveToolPath(ctx, e, path)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			return withFileMutationQueue(ctx, e, absolute, func() (harness.ToolExecutionResult, error) {
				return applyEdit(ctx, e, path, absolute, edits)
			})
		},
	}
}

func applyEdit(ctx context.Context, e env.ExecutionEnv, path, absolute string, edits []editmatch.Edit) (harness.ToolExecutionResult, error) {
	fail := func(err error) (harness.ToolExecutionResult, error) { return harness.ToolExecutionResult{}, err }
	if ctx.Err() != nil {
		return fail(errAborted)
	}
	info, err := e.Stat(ctx, absolute)
	if err != nil {
		return fail(editAccessError(path, err))
	}
	if info.Kind != env.KindFile && info.Kind != env.KindSymlink {
		return fail(fmt.Errorf("Could not edit file: %s. Path is not a file.", path))
	}
	raw, err := e.ReadTextFile(ctx, absolute)
	if err != nil {
		return fail(editAccessError(path, err))
	}
	if ctx.Err() != nil {
		return fail(errAborted)
	}

	bom, content := stripBOM(raw)
	ending := editmatch.DetectLineEnding(content)
	base, updated, err := editmatch.ApplyEditsToNormalizedContent(editmatch.NormalizeToLF(content), edits, path)
	if err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return fail(errAborted)
	}

	final := bom + editmatch.RestoreLineEndings(updated, ending)
	if err := e.WriteFile(ctx, absolute, []byte(final)); err != nil {
		return fail(editAccessError(path, err))
	}
	if ctx.Err() != nil {
		return fail(errAborted)
	}

	display, firstChangedLine := generateDiffString(base, updated, 4)
	return harness.ToolExecutionResult{
		Output: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path)}},
		Details: &EditToolDetails{
			Diff:             display,
			Patch:            diff.CreateTwoFilesPatch(path, path, base, updated, 4),
			FirstChangedLine: firstChangedLine,
		},
	}, nil
}

// stripBOM splits a leading U+FEFF off content.
func stripBOM(content string) (bom, text string) {
	const bomText = "\xef\xbb\xbf"
	if strings.HasPrefix(content, bomText) {
		return bomText, content[len(bomText):]
	}
	return "", content
}

// generateDiffString is a display diff with line numbers and contextLines of
// context around each change, and the first changed line in the new file
// (nil without changes).
func generateDiffString(oldContent, newContent string, contextLines int) (string, *int) {
	parts := diff.DiffLines(oldContent, newContent)
	var out []string

	maxLineNum := max(strings.Count(oldContent, "\n"), strings.Count(newContent, "\n")) + 1
	width := len(strconv.Itoa(maxLineNum))
	pad := func(n int) string { return fmt.Sprintf("%*d", width, n) }
	ellipsis := " " + strings.Repeat(" ", width) + " ..."

	oldLineNum, newLineNum := 1, 1
	lastWasChange := false
	var firstChangedLine *int
	for i, part := range parts {
		raw := strings.Split(part.Value, "\n")
		if raw[len(raw)-1] == "" {
			raw = raw[:len(raw)-1]
		}
		if part.Added || part.Removed {
			if firstChangedLine == nil {
				line := newLineNum
				firstChangedLine = &line
			}
			for _, line := range raw {
				if part.Added {
					out = append(out, "+"+pad(newLineNum)+" "+line)
					newLineNum++
				} else {
					out = append(out, "-"+pad(oldLineNum)+" "+line)
					oldLineNum++
				}
			}
			lastWasChange = true
			continue
		}
		context := func(lines []string) {
			for _, line := range lines {
				out = append(out, " "+pad(oldLineNum)+" "+line)
				oldLineNum++
				newLineNum++
			}
		}
		skip := func(n int) {
			oldLineNum += n
			newLineNum += n
		}
		nextIsChange := i < len(parts)-1 && (parts[i+1].Added || parts[i+1].Removed)
		switch {
		case lastWasChange && nextIsChange:
			if len(raw) <= contextLines*2 {
				context(raw)
			} else {
				context(raw[:contextLines])
				out = append(out, ellipsis)
				skip(len(raw) - 2*contextLines)
				context(raw[len(raw)-contextLines:])
			}
		case lastWasChange:
			shown := raw[:min(contextLines, len(raw))]
			context(shown)
			if skipped := len(raw) - len(shown); skipped > 0 {
				out = append(out, ellipsis)
				skip(skipped)
			}
		case nextIsChange:
			skipped := max(0, len(raw)-contextLines)
			if skipped > 0 {
				out = append(out, ellipsis)
				skip(skipped)
			}
			context(raw[skipped:])
		default:
			skip(len(raw))
		}
		lastWasChange = false
	}
	return strings.Join(out, "\n"), firstChangedLine
}
