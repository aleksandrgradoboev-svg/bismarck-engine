package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/imagemime"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// ReadToolDetails are read's details: how the shown text was cut, the text
// itself being the result's output.
type ReadToolDetails struct {
	Truncation *TruncationResult `json:"truncation,omitempty"`
}

// ReadToolOptions configure the read tool.
type ReadToolOptions struct {
	// Images resizes and converts images; without one, images within
	// DefaultImageLimits pass through as they are.
	Images ImageProcessor
}

const readChunk = 64 * 1024

// maxSafeInteger is JS's Number.MAX_SAFE_INTEGER.
const maxSafeInteger = 1<<53 - 1

// sliceIndex is Array.prototype.slice's conversion of an index: NaN is 0,
// other values truncate toward zero.
func sliceIndex(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	return math.Trunc(value)
}

func isSafeInteger(value float64) bool {
	return value == math.Trunc(value) && math.Abs(value) <= maxSafeInteger
}

// jsNumber is a number as a JS template literal renders it.
func jsNumber(value float64) string { return jstext.NumberToString(value) }

// numberArg is an optional numeric argument; JSON gives float64, Go callers
// may pass ints.
func numberArg(args map[string]any, key string) (float64, bool) {
	switch v := args[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// NewReadTool is the read tool (pi createReadTool): text files and images.
// Remarks about truncation, continuation and image processing are
// diagnostics; the output is only file text, or one image, which programs
// get as its ImageContent.
func NewReadTool(options ReadToolOptions) *harness.ToolRegistration {
	formats := "jpg, png, gif, webp"
	if options.Images != nil {
		formats = "jpg, png, gif, webp, bmp"
	}
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name: "read",
			Description: fmt.Sprintf("Read the contents of a file. Supports text files and images (%s). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
				formats, DefaultMaxLines, DefaultMaxBytes/1024),
			Parameters: ai.Object(
				ai.Prop("path", ai.String("Path to the file to read (relative or absolute)")),
				ai.Opt("offset", ai.Number("Line number to start reading from (1-indexed)")),
				ai.Opt("limit", ai.Number("Maximum number of lines to read")),
			),
		},
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			path, _ := args["path"].(string)
			offset, hasOffset := numberArg(args, "offset")
			limit, hasLimit := numberArg(args, "limit")
			e, err := requireEnv(api)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			absolute, err := resolveReadToolPath(ctx, e, path)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			reader, err := e.OpenBinaryReader(ctx, absolute, env.BinaryReaderOptions{})
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			defer reader.Close()
			// A concurrent writer can change the file between the scan and the
			// reads. Appending (a growing log) leaves the scanned text as it
			// was; an image must be whole. Otherwise the file is read again
			// once.
			for attempt := 0; ; attempt++ {
				before, err := reader.Info(ctx)
				if err != nil {
					return harness.ToolExecutionResult{}, err
				}
				mimeType, err := imagemime.DetectOf(before.Size, func(offset, length int64) ([]byte, error) {
					return reader.Read(ctx, offset, length)
				})
				if err != nil {
					return harness.ToolExecutionResult{}, err
				}
				var result harness.ToolExecutionResult
				if mimeType == "" {
					result, err = readText(ctx, reader, path, offset, hasOffset, limit, hasLimit)
				} else {
					var model imageModel
					if model, err = imageModelOf(ctx, api); err == nil {
						result, err = readImage(ctx, reader, before, path, mimeType, options.Images, model)
					}
				}
				if err != nil {
					return harness.ToolExecutionResult{}, err
				}
				after, err := reader.Info(ctx)
				if err != nil {
					return harness.ToolExecutionResult{}, err
				}
				unchanged := after.Size == before.Size && after.ModTime.Equal(before.ModTime)
				if unchanged || (mimeType == "" && after.Size > before.Size) {
					return result, nil
				}
				if attempt == 1 {
					return harness.ToolExecutionResult{}, fmt.Errorf("%s changed while it was read", path)
				}
			}
		},
	}
}

// imageModel is what the conversation's model takes: whether images, and
// within which limits.
type imageModel struct {
	vision bool
	limits ImageLimits
}

// imageModelOf is the conversation model's image input: DefaultImageLimits
// overridden by its InputLimits.Images.Resize. Without a resolvable model, it
// counts as one that takes images within the defaults.
func imageModelOf(ctx context.Context, api harness.ToolExecutionAPI) (imageModel, error) {
	agent, err := api.Agent(ctx)
	if err != nil {
		return imageModel{}, err
	}
	var model *ai.Model
	if agent != nil && agent.Model != nil {
		if models := api.Models(); models != nil {
			model = models.GetModel(agent.Model.Provider, agent.Model.ModelID)
		}
	}
	result := imageModel{vision: true, limits: DefaultImageLimits}
	if model == nil {
		return result, nil
	}
	result.vision = false
	for _, input := range model.Input {
		if input == "image" {
			result.vision = true
		}
	}
	if model.InputLimits != nil && model.InputLimits.Images != nil && model.InputLimits.Images.Resize != nil {
		resize := model.InputLimits.Images.Resize
		if resize.MaxWidth != nil {
			result.limits.MaxWidth = *resize.MaxWidth
		}
		if resize.MaxHeight != nil {
			result.limits.MaxHeight = *resize.MaxHeight
		}
		if resize.MaxBytes != nil {
			result.limits.MaxBytes = *resize.MaxBytes
		}
	}
	return result, nil
}

func imageError(message string) harness.ToolExecutionResult {
	return harness.ToolExecutionResult{
		Output:      ai.ContentList{},
		IsError:     true,
		Diagnostics: []harness.ToolDiagnostic{{Severity: "error", Code: "unsupported_image", Message: message}},
	}
}

// readImage is one image block, prepared by processor or, without one, as it
// is when its format is inline and it fits the limits (its size checked
// before reading it, its dimensions from its header); otherwise an error
// result saying why. What happened to the image is an info diagnostic, so
// programs get the bare ImageContent. A model without image input sees a
// placeholder instead; a diagnostic says so.
func readImage(ctx context.Context, reader env.BinaryReader, info env.FileInfo, path, mimeType string, processor ImageProcessor, model imageModel) (harness.ToolExecutionResult, error) {
	var data, imageType string
	var notes []string
	if processor != nil {
		bytes, err := reader.Read(ctx, 0, info.Size)
		if err != nil {
			return harness.ToolExecutionResult{}, err
		}
		prepared, err := processor.Prepare(ctx, bytes, mimeType, model.limits)
		if err != nil {
			return harness.ToolExecutionResult{}, err
		}
		if prepared == nil {
			return imageError(fmt.Sprintf("%s is an image (%s) that cannot be prepared for the model", path, mimeType)), nil
		}
		data, imageType = prepared.Data, prepared.MimeType
		if prepared.ConvertedFrom != "" {
			notes = append(notes, fmt.Sprintf("Converted from %s to %s.", prepared.ConvertedFrom, prepared.MimeType))
		}
		if r := prepared.Resized; r != nil {
			scale := float64(r.From.Width) / float64(r.To.Width)
			notes = append(notes, fmt.Sprintf("Resized from %dx%d to %dx%d. Multiply coordinates by %.2f to map them to the original.",
				r.From.Width, r.From.Height, r.To.Width, r.To.Height, scale))
		}
	} else {
		if !inlineImageTypes[mimeType] {
			return imageError(fmt.Sprintf("%s is an image (%s) that needs converting, and no image processor is configured", path, mimeType)), nil
		}
		const unshrinkable = "and no image processor is configured to shrink it"
		if base64Length(info.Size) > int64(model.limits.MaxBytes) {
			return imageError(fmt.Sprintf("%s is an image (%s) of %s, too large to send, %s", path, mimeType, FormatSize(int(info.Size)), unshrinkable)), nil
		}
		bytes, err := reader.Read(ctx, 0, info.Size)
		if err != nil {
			return harness.ToolExecutionResult{}, err
		}
		width, height, ok := imagemime.Dimensions(bytes, mimeType)
		if !ok {
			return imageError(fmt.Sprintf("%s is an image (%s) whose size cannot be read", path, mimeType)), nil
		}
		if width > model.limits.MaxWidth || height > model.limits.MaxHeight {
			return imageError(fmt.Sprintf("%s is an image (%s) of %dx%d, larger than %dx%d, %s",
				path, mimeType, width, height, model.limits.MaxWidth, model.limits.MaxHeight, unshrinkable)), nil
		}
		data, imageType = toBase64(bytes), mimeType
	}
	if !model.vision {
		notes = append(notes, "The current model does not support images; it sees a placeholder instead.")
	}
	message := strings.Join(append([]string{fmt.Sprintf("Read image file [%s].", imageType)}, notes...), " ")
	return harness.ToolExecutionResult{
		Output:      ai.ContentList{ai.ImageContent{Data: data, MimeType: imageType}},
		Diagnostics: []harness.ToolDiagnostic{{Severity: "info", Code: "image", Message: message}},
	}, nil
}

// readHead is the decoded start of bytes [start, end) of the file, decoded as
// part of the whole file: all of it, or enough for TruncateHeadOf (more than
// DefaultMaxBytes+1 bytes, or DefaultMaxLines newlines).
func readHead(ctx context.Context, reader env.BinaryReader, start, end int64, skipBOM bool) (string, error) {
	decoder := jstext.TextDecoder{IgnoreBOM: true}
	var text strings.Builder
	newlines := 0
	position := start
	if skipBOM && start == 0 {
		position = 3
	}
	for position < end {
		bytes, err := reader.Read(ctx, position, min(readChunk, end-position))
		if err != nil {
			return "", err
		}
		if len(bytes) == 0 {
			break
		}
		position += int64(len(bytes))
		decoded := decoder.Decode(bytes)
		text.WriteString(decoded)
		newlines += strings.Count(decoded, "\n")
		if newlines >= DefaultMaxLines || output.UTF8ByteLength(text.String()) > DefaultMaxBytes+1 {
			return text.String(), nil
		}
	}
	return text.String() + decoder.Flush(), nil
}

func startsWithBOM(first []byte) bool {
	return len(first) >= 3 && first[0] == 0xef && first[1] == 0xbb && first[2] == 0xbf
}

// readText is the read result for the opened file. It equals decoding the
// whole file, splitting it on "\n", and bounding the selected lines with
// TruncateHead, while reading only one scan's worth of the file plus the
// head. Offsets and limits follow JS arithmetic, fractions included, as the
// arguments are JSON numbers.
func readText(ctx context.Context, reader env.BinaryReader, path string, offset float64, hasOffset bool, limit float64, hasLimit bool) (harness.ToolExecutionResult, error) {
	startLine := 0.0
	if hasOffset && offset != 0 && !math.IsNaN(offset) {
		startLine = math.Max(0, offset-1)
	}
	startLineDisplay := startLine + 1
	// Lines are selected like allLines.slice(startLine, endLine), which
	// truncates fractional indices.
	sliceStart := sliceIndex(startLine)
	// One pass finds the line count and the selection; a selection past the
	// last line ends with it, as slice does, and an empty one (a zero or
	// negative limit) is scanned as one line and then ignored. A start beyond
	// any file is scanned from 0 only to count lines; the offset check below
	// then fails.
	scanStart := 0.0
	if isSafeInteger(sliceStart) {
		scanStart = sliceStart
	}
	scanEnd := 0.0 // to the end
	if hasLimit {
		requestedEnd := math.Max(scanStart+1, sliceIndex(startLine+limit))
		if isSafeInteger(requestedEnd) {
			scanEnd = requestedEnd
		}
	}
	scanOf := func(endLine float64) (env.LineScan, error) {
		return reader.ScanLines(ctx, env.LineRange{Start: int64(scanStart), End: int64(endLine)})
	}
	scan, err := scanOf(scanEnd)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	totalFileLines := float64(scan.Newlines + 1)
	if startLine >= totalFileLines {
		return harness.ToolExecutionResult{}, fmt.Errorf("Offset %s is beyond end of file (%s lines total)", jsNumber(offset), jsNumber(totalFileLines))
	}

	userLimited := false
	var userLimitedLines float64
	selectedLineCount := totalFileLines - sliceStart
	if hasLimit {
		endLine := math.Min(startLine+limit, totalFileLines)
		userLimited, userLimitedLines = true, endLine-startLine
		// slice counts a negative end from the end of the lines, which only
		// the line count tells; scan again for it.
		relativeEnd := sliceIndex(endLine)
		sliceEnd := relativeEnd
		if relativeEnd < 0 {
			sliceEnd = math.Max(totalFileLines+relativeEnd, 0)
		}
		selectedLineCount = math.Max(0, sliceEnd-sliceStart)
		if selectedLineCount > 0 && relativeEnd < 0 {
			if scan, err = scanOf(sliceEnd); err != nil {
				return harness.ToolExecutionResult{}, err
			}
		}
	}
	empty := selectedLineCount == 0
	// Counted like TruncateHead: a trailing newline adds no line, and empty
	// text has none.
	endsWithNewline := !empty && scan.LastLineStart == scan.End && scan.LastLineStart > scan.Start
	totals := TextTotals{}
	if !empty && scan.SelectedBytes != 0 {
		totals.Lines = int(selectedLineCount)
		if endsWithNewline {
			totals.Lines--
		}
	}
	if !empty {
		totals.Bytes = int(scan.SelectedBytes)
	}
	firstBytes, err := reader.Read(ctx, 0, 3)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	head := ""
	if !empty {
		if head, err = readHead(ctx, reader, scan.Start, scan.End, startsWithBOM(firstBytes)); err != nil {
			return harness.ToolExecutionResult{}, err
		}
	}

	truncation := TruncateHeadOf(head, totals, TruncationOptions{})
	outputText := truncation.Content
	diagnostics := []harness.ToolDiagnostic{}
	var details *ReadToolDetails
	switch {
	case truncation.FirstLineExceedsLimit:
		// Show the start of the line, cut at the byte limit on a character
		// boundary. Like allLines[startLine], a fractional start line names
		// no line.
		var lineBytes []byte
		lineSize := 0
		if startLine == math.Trunc(startLine) {
			first, _, _ := strings.Cut(head, "\n")
			lineBytes, lineSize = []byte(first), int(scan.FirstLineBytes)
		}
		end := output.CharacterEnd(lineBytes, DefaultMaxBytes)
		if end > len(lineBytes) {
			end = len(lineBytes)
		}
		outputText = string(lineBytes[:end])
		diagnostics = append(diagnostics, harness.ToolDiagnostic{
			Severity: "warn",
			Code:     "truncated",
			Message: fmt.Sprintf("Line %s is %s, exceeds the %s limit; showing its first %s. Use bash: sed -n '%sp' %s | tail -c +%d",
				jsNumber(startLineDisplay), FormatSize(lineSize), FormatSize(DefaultMaxBytes), FormatSize(end), jsNumber(startLineDisplay), path, end+1),
		})
		cut := truncation
		cut.OutputBytes, cut.OutputLines = end, 1
		details = &ReadToolDetails{Truncation: &cut}
	case truncation.Truncated:
		endLineDisplay := startLineDisplay + float64(truncation.OutputLines) - 1
		nextOffset := endLineDisplay + 1
		limitText := ""
		if truncation.TruncatedBy != TruncatedByLines {
			limitText = fmt.Sprintf(" (%s limit)", FormatSize(DefaultMaxBytes))
		}
		diagnostics = append(diagnostics, harness.ToolDiagnostic{
			Severity: "info",
			Code:     "truncated",
			Message: fmt.Sprintf("Showing lines %s-%s of %s%s. Use offset=%s to continue.",
				jsNumber(startLineDisplay), jsNumber(endLineDisplay), jsNumber(totalFileLines), limitText, jsNumber(nextOffset)),
		})
		details = &ReadToolDetails{Truncation: &truncation}
	case userLimited && startLine+userLimitedLines < totalFileLines:
		remaining := totalFileLines - (startLine + userLimitedLines)
		nextOffset := startLine + userLimitedLines + 1
		diagnostics = append(diagnostics, harness.ToolDiagnostic{
			Severity: "info",
			Message:  fmt.Sprintf("%s more lines in file. Use offset=%s to continue.", jsNumber(remaining), jsNumber(nextOffset)),
		})
	}

	result := harness.ToolExecutionResult{Output: ai.ContentList{}, Diagnostics: diagnostics}
	if outputText != "" {
		result.Output = ai.ContentList{ai.TextContent{Text: outputText}}
	}
	if details != nil {
		result.Details = details
	}
	return result, nil
}
