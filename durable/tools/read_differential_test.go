package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/imagemime"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Ported from pi packages/durable/test/tools-read-differential.test.ts. The
// offsets and limits drop NaN, which JSON arguments cannot carry.

// readOutcome is a read result as the comparison sees it, or its error.
type readOutcome struct {
	Output      []string
	Details     string
	Diagnostics []harness.ToolDiagnostic
	IsError     bool
	Error       string
}

func outcomeOf(result harness.ToolExecutionResult, err error) readOutcome {
	if err != nil {
		return readOutcome{Error: err.Error()}
	}
	o := readOutcome{IsError: result.IsError, Diagnostics: result.Diagnostics}
	for _, part := range result.Output {
		switch p := part.(type) {
		case ai.TextContent:
			o.Output = append(o.Output, "text:"+p.Text)
		case ai.ImageContent:
			o.Output = append(o.Output, "image:"+p.MimeType+":"+p.Data)
		}
	}
	if result.Details != nil {
		b, _ := json.Marshal(result.Details)
		o.Details = string(b)
	}
	if o.Diagnostics == nil {
		o.Diagnostics = []harness.ToolDiagnostic{}
	}
	return o
}

// jsSlice is lines.slice(start, end) of JS, end omitted when !hasEnd.
func jsSlice(lines []string, start float64, end float64, hasEnd bool) []string {
	n := float64(len(lines))
	clamp := func(v float64) int {
		v = math.Trunc(v)
		if v < 0 {
			v = math.Max(n+v, 0)
		}
		return int(math.Min(v, n))
	}
	from, to := clamp(start), len(lines)
	if hasEnd {
		to = clamp(end)
	}
	if to < from {
		return nil
	}
	return lines[from:to]
}

// referenceRead is the read tool before bounded reads: it decodes the whole
// file, splits it into lines, and truncates the selection.
func referenceRead(bytes []byte, path string, offset float64, hasOffset bool, limit float64, hasLimit bool) readOutcome {
	if mimeType := imagemime.Detect(bytes); mimeType != "" {
		return outcomeOf(harness.ToolExecutionResult{
			Output:      ai.ContentList{ai.ImageContent{Data: toBase64(bytes), MimeType: mimeType}},
			Diagnostics: []harness.ToolDiagnostic{{Severity: "info", Code: "image", Message: "Read image file [" + mimeType + "]."}},
		}, nil)
	}
	allLines := strings.Split(jstext.DecodeText(bytes), "\n")
	total := float64(len(allLines))
	startLine := 0.0
	if hasOffset && offset != 0 {
		startLine = math.Max(0, offset-1)
	}
	startLineDisplay := startLine + 1
	if startLine >= total {
		return readOutcome{Error: fmt.Sprintf("Offset %s is beyond end of file (%s lines total)", jstext.NumberToString(offset), jstext.NumberToString(total))}
	}
	var selected []string
	userLimited := false
	var userLimitedLines float64
	if hasLimit {
		endLine := math.Min(startLine+limit, total)
		selected = jsSlice(allLines, startLine, endLine, true)
		userLimited, userLimitedLines = true, endLine-startLine
	} else {
		selected = jsSlice(allLines, startLine, 0, false)
	}
	truncation := TruncateHead(strings.Join(selected, "\n"), TruncationOptions{})
	diagnostics := []harness.ToolDiagnostic{}
	outputText := truncation.Content
	var details *ReadToolDetails
	switch {
	case truncation.FirstLineExceedsLimit:
		var lineBytes []byte
		if startLine == math.Trunc(startLine) {
			lineBytes = []byte(allLines[int(startLine)])
		}
		end := min(output.CharacterEnd(lineBytes, DefaultMaxBytes), len(lineBytes))
		outputText = string(lineBytes[:end])
		diagnostics = append(diagnostics, harness.ToolDiagnostic{Severity: "warn", Code: "truncated",
			Message: fmt.Sprintf("Line %s is %s, exceeds the %s limit; showing its first %s. Use bash: sed -n '%sp' %s | tail -c +%d",
				jstext.NumberToString(startLineDisplay), FormatSize(len(lineBytes)), FormatSize(DefaultMaxBytes), FormatSize(end), jstext.NumberToString(startLineDisplay), path, end+1)})
		cut := truncation
		cut.OutputBytes, cut.OutputLines = end, 1
		details = &ReadToolDetails{Truncation: &cut}
	case truncation.Truncated:
		endLineDisplay := startLineDisplay + float64(truncation.OutputLines) - 1
		limitText := ""
		if truncation.TruncatedBy != TruncatedByLines {
			limitText = fmt.Sprintf(" (%s limit)", FormatSize(DefaultMaxBytes))
		}
		diagnostics = append(diagnostics, harness.ToolDiagnostic{Severity: "info", Code: "truncated",
			Message: fmt.Sprintf("Showing lines %s-%s of %s%s. Use offset=%s to continue.",
				jstext.NumberToString(startLineDisplay), jstext.NumberToString(endLineDisplay), jstext.NumberToString(total), limitText, jstext.NumberToString(endLineDisplay+1))})
		details = &ReadToolDetails{Truncation: &truncation}
	case userLimited && startLine+userLimitedLines < total:
		diagnostics = append(diagnostics, harness.ToolDiagnostic{Severity: "info",
			Message: fmt.Sprintf("%s more lines in file. Use offset=%s to continue.",
				jstext.NumberToString(total-(startLine+userLimitedLines)), jstext.NumberToString(startLine+userLimitedLines+1))})
	}
	result := harness.ToolExecutionResult{Output: ai.ContentList{}, Diagnostics: diagnostics}
	if outputText != "" {
		result.Output = ai.ContentList{ai.TextContent{Text: outputText}}
	}
	if details != nil {
		result.Details = details
	}
	return outcomeOf(result, nil)
}

// mulberry32 is the test's pseudo-random source, as pi's random(seed).
func mulberry32(seed uint32) func() float64 {
	state := seed
	return func() float64 {
		state += 0x6d2b79f5
		t := state
		t = (t ^ t>>15) * (t | 1)
		t ^= t + (t^t>>7)*(t|61)
		return float64(t^t>>14) / 4294967296
	}
}

var readPieces = [][]byte{
	{0x0a}, {0x0a}, {0x61}, {0x62, 0x63, 0x64, 0x65}, {0xef, 0xbb, 0xbf}, {0xc3, 0xa9},
	{0xe2, 0x82, 0xac}, {0xf0, 0x9f, 0x98, 0x80}, {0xe2, 0x82}, {0xff}, {0x0d, 0x0a},
}

// randomFile is mostly small files, some over the line limit, some over the
// byte limit, some with one huge line.
func randomFile(next func() float64) []byte {
	kind := next()
	switch {
	case kind < 0.1:
		return append([]byte{0xef, 0xbb, 0xbf}, strings.Repeat("x\n", 2100)...)
	case kind < 0.2:
		line := []byte(strings.Repeat("a", DefaultMaxBytes+200+int(next()*400)))
		line[DefaultMaxBytes-1+int(next()*3)] = 0xc3
		return append(line, 0x0a, 0x62)
	case kind < 0.3:
		return []byte(strings.Repeat(strings.Repeat("0123456789", 30)+"\n", 200))
	}
	var b []byte
	pieces := int(next() * 80)
	for range pieces {
		b = append(b, readPieces[int(next()*float64(len(readPieces)))]...)
	}
	return b
}

// readArgs are the offsets or limits of the comparison; nil is omitted.
type readArg *float64

func num(v float64) readArg { return &v }

var (
	readOffsets = []readArg{nil, num(0), num(1), num(2), num(3), num(2.5), num(-4), num(50), num(2001), num(3000), num(1e20)}
	readLimits  = []readArg{nil, num(0), num(1), num(2), num(-3), num(-1e20), num(1.5), num(7), num(2000), num(2500), num(1e20)}
)

func TestReadReturnsExactlyWhatReadingTheWholeFileReturned(t *testing.T) {
	cwd := t.TempDir()
	e := env.NewLocal(cwd)
	tool := NewReadTool(ReadToolOptions{})
	for seed := uint32(1); seed <= 400; seed++ {
		next := mulberry32(seed)
		file := randomFile(next)
		if err := os.WriteFile(filepath.Join(cwd, "f.txt"), file, 0o644); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			offset := readOffsets[int(next()*float64(len(readOffsets)))]
			limit := readLimits[int(next()*float64(len(readLimits)))]
			args := map[string]any{"path": "f.txt"}
			var o, l float64
			if offset != nil {
				o = *offset
				args["offset"] = o
			}
			if limit != nil {
				l = *limit
				args["limit"] = l
			}
			actual := outcomeOf(run(context.Background(), tool, args, e))
			expected := referenceRead(file, "f.txt", o, offset != nil, l, limit != nil)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("seed %d offset %v limit %v:\n got  %+v\n want %+v", seed, args["offset"], args["limit"], actual, expected)
			}
		}
	}
}

func TestReadDetectsAnimatedPNGsWhoseAcTLChunkLiesFarBeyondTheHeader(t *testing.T) {
	cwd := t.TempDir()
	chunk := func(kind string, length int) []byte {
		b := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
		b = append(b, kind...)
		return append(b, make([]byte, length+4)...)
	}
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	for _, c := range [][]byte{chunk("IHDR", 13), chunk("iCCP", 200_000), chunk("acTL", 8), chunk("IDAT", 10)} {
		png = append(png, c...)
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	actual := outcomeOf(run(context.Background(), NewReadTool(ReadToolOptions{}), map[string]any{"path": "a.png"}, env.NewLocal(cwd)))
	if expected := referenceRead(png, "a.png", 0, false, 0, false); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("got %+v\nwant %+v", actual, expected)
	}
}
