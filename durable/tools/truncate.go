package tools

import (
	"fmt"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
)

// The default limits of text a tool returns: whichever is hit first wins.
const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024
)

// TruncationLimit is the limit a truncation hit: "lines", "bytes", or ""
// when nothing was cut, which JSON spells null as pi does.
type TruncationLimit string

const (
	TruncatedByLines TruncationLimit = "lines"
	TruncatedByBytes TruncationLimit = "bytes"
)

func (l TruncationLimit) MarshalJSON() ([]byte, error) {
	if l == "" {
		return []byte("null"), nil
	}
	return []byte(`"` + string(l) + `"`), nil
}

// TruncationResult is how a text was cut (pi TruncationResult). Content is
// the kept text; it stays out of JSON, as read's details omit it.
type TruncationResult struct {
	Content     string          `json:"-"`
	Truncated   bool            `json:"truncated"`
	TruncatedBy TruncationLimit `json:"truncatedBy"`
	// TotalLines and TotalBytes describe the whole text.
	TotalLines int `json:"totalLines"`
	TotalBytes int `json:"totalBytes"`
	// OutputLines are the complete lines kept; OutputBytes their bytes.
	OutputLines int `json:"outputLines"`
	OutputBytes int `json:"outputBytes"`
	// LastLinePartial is always false for head truncation.
	LastLinePartial bool `json:"lastLinePartial"`
	// FirstLineExceedsLimit: the first line alone is over the byte limit.
	FirstLineExceedsLimit bool `json:"firstLineExceedsLimit"`
	MaxLines              int  `json:"maxLines"`
	MaxBytes              int  `json:"maxBytes"`
}

// TruncationOptions override the default limits; zero keeps a default.
type TruncationOptions struct {
	MaxLines, MaxBytes int
}

// FormatSize renders a byte count as a human-readable size.
func FormatSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

func splitLinesForCounting(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TruncateHead keeps the first lines of content within the limits, never a
// partial line: when the first line alone exceeds the byte limit, nothing is
// kept and FirstLineExceedsLimit is set.
func TruncateHead(content string, options TruncationOptions) TruncationResult {
	totals := TextTotals{Lines: len(splitLinesForCounting(content)), Bytes: output.UTF8ByteLength(content)}
	return TruncateHeadOf(content, totals, options)
}

// TextTotals are a text's line count, ignoring a trailing newline as
// TruncateHead does, and its UTF-8 byte length.
type TextTotals struct {
	Lines, Bytes int
}

// TruncateHeadOf is TruncateHead of a text known by a prefix and its totals.
// The prefix must be the whole text, or longer than MaxBytes+1 UTF-8 bytes,
// or hold at least MaxLines newlines; then the result equals TruncateHead of
// the whole text.
func TruncateHeadOf(prefix string, totals TextTotals, options TruncationOptions) TruncationResult {
	maxLines := options.MaxLines
	if maxLines == 0 {
		maxLines = DefaultMaxLines
	}
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	result := TruncationResult{TotalLines: totals.Lines, TotalBytes: totals.Bytes, MaxLines: maxLines, MaxBytes: maxBytes}
	lines := splitLinesForCounting(prefix)

	if totals.Lines <= maxLines && totals.Bytes <= maxBytes {
		result.Content = prefix
		result.OutputLines = totals.Lines
		result.OutputBytes = totals.Bytes
		return result
	}
	result.Truncated = true

	// pi measures lines[0] of an empty prefix as 0 bytes.
	if len(lines) > 0 && output.UTF8ByteLength(lines[0]) > maxBytes {
		result.TruncatedBy = TruncatedByBytes
		result.FirstLineExceedsLimit = true
		return result
	}

	var kept []string
	keptBytes := 0
	truncatedBy := TruncatedByLines
	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineBytes := output.UTF8ByteLength(lines[i])
		if i > 0 {
			lineBytes++
		}
		if keptBytes+lineBytes > maxBytes {
			truncatedBy = TruncatedByBytes
			break
		}
		kept = append(kept, lines[i])
		keptBytes += lineBytes
	}
	// Without a byte break, only omitted lines prove the line limit was
	// reached; otherwise a trailing newline exceeded the bytes.
	if truncatedBy != TruncatedByBytes {
		if len(kept) < totals.Lines {
			truncatedBy = TruncatedByLines
		} else {
			truncatedBy = TruncatedByBytes
		}
	}
	result.Content = strings.Join(kept, "\n")
	result.TruncatedBy = truncatedBy
	result.OutputLines = len(kept)
	result.OutputBytes = output.UTF8ByteLength(result.Content)
	return result
}
