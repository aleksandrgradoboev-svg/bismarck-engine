// Package output is pi's chord/output (packages/chord/src/output.ts): bounded
// output streams and adaptive progress commits. [OutputBuffer] keeps a head or
// tail window of a stream at a cost proportional to each chunk, and
// [Progress] paces commits of a changing value by their size.
//
// Text is UTF-8. Where pi's input is a JS string, which TextEncoder encodes
// with every lone surrogate as U+FFFD, a Go string's malformed bytes are read
// as the WHATWG decoder reads them, so counts and slices agree with pi's.
package output

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
)

// Retain says which end of a stream a window keeps.
type Retain string

const (
	RetainHead Retain = "head"
	RetainTail Retain = "tail"
)

// Limits are the retention limits of one output stream (pi OutputLimits).
type Limits struct {
	MaxBytes int
	MaxLines int
	Retain   Retain
}

// Bounded is retained output and what the limits dropped (pi BoundedOutput).
type Bounded struct {
	Text         string
	DroppedBytes int
	DroppedLines int
}

// Slice is an exact slice of the input within the limits, and what it left
// out (pi OutputSlice).
type Slice struct {
	Text         string
	Bytes        int
	DroppedBytes int
	DroppedLines int
}

// Skip is output a producer omitted before a chunk, measured on the decoded
// text: every U+FFFD counts as three bytes, and no sanitizing is applied (pi
// OutputSkip).
type Skip struct {
	// Bytes is the UTF-8 byte length of the omitted text.
	Bytes int
	// Newlines is the number of newlines (U+000A) in the omitted text.
	Newlines int
	// EndsWithNewline reports whether the omitted text ends with a newline.
	EndsWithNewline bool
}

const newline = '\n'

// invalidOutput is pi's /[\x00-\x08\x0b-\x1f\ufff9-\ufffb]/g.
var invalidOutput = regexp.MustCompile("[\x00-\x08\x0b-\x1f\ufff9-\ufffb]")

// ErrSkipNeedsTail is the refusal of a skip with head retention.
var ErrSkipNeedsTail = errors.New("Skipped output requires tail retention")

// SanitizeOutput removes control characters that break display and
// transcripts; tabs and newlines stay.
func SanitizeOutput(text string) string {
	return invalidOutput.ReplaceAllString(text, "")
}

// BoundOutput bounds text to whole lines within the limits: the first lines
// for head, the last lines for tail. The result is an exact slice, trailing
// newline included. A single line longer than MaxBytes is cut at the byte
// limit on a character boundary.
func BoundOutput(text string, limits Limits) Slice {
	text = wellFormed(text)
	b := []byte(text)
	var from, to int
	if limits.Retain == RetainHead {
		from, to = headRange(b, limits)
	} else {
		from, to = tailRange(b, limits)
	}
	kept := text[from:to]
	return Slice{
		Text:         kept,
		Bytes:        len(kept),
		DroppedBytes: len(b) - len(kept),
		DroppedLines: lineCount(b) - lineCount(b[from:to]),
	}
}

// lastIndexAtOrBefore is JS lastIndexOf(byte, at): the last index at or before
// at, -1 when at is negative.
func lastIndexAtOrBefore(b []byte, c byte, at int) int {
	if at < 0 {
		return -1
	}
	if at >= len(b) {
		at = len(b) - 1
	}
	return bytes.LastIndexByte(b[:at+1], c)
}

// indexFrom is JS indexOf(byte, from), with a negative from read as 0.
func indexFrom(b []byte, c byte, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(b) {
		return -1
	}
	i := bytes.IndexByte(b[from:], c)
	if i < 0 {
		return -1
	}
	return from + i
}

func headRange(b []byte, limits Limits) (int, int) {
	if limits.MaxLines == 0 || limits.MaxBytes == 0 {
		return 0, 0
	}
	end := len(b)
	lines := 0
	for i := indexFrom(b, newline, 0); i != -1; i = indexFrom(b, newline, i+1) {
		lines++
		if lines == limits.MaxLines {
			end = i + 1
			break
		}
	}
	if end > limits.MaxBytes {
		nl := lastIndexAtOrBefore(b, newline, limits.MaxBytes-1)
		if nl == -1 {
			end = CharacterEnd(b, limits.MaxBytes)
		} else {
			end = nl + 1
		}
	}
	return 0, end
}

func tailRange(b []byte, limits Limits) (int, int) {
	if limits.MaxLines == 0 || limits.MaxBytes == 0 {
		return len(b), len(b)
	}
	// A trailing newline ends the last line rather than starting another.
	last := len(b) - 1
	if len(b) > 0 && b[len(b)-1] == newline {
		last = len(b) - 2
	}
	start := 0
	lines := 1
	for i := lastIndexAtOrBefore(b, newline, last); i != -1; {
		if lines == limits.MaxLines {
			start = i + 1
			break
		}
		lines++
		if i == 0 {
			i = -1
		} else {
			i = lastIndexAtOrBefore(b, newline, i-1)
		}
	}
	if len(b)-start > limits.MaxBytes {
		from := len(b) - limits.MaxBytes
		nl := indexFrom(b, newline, from-1)
		// The first line starting inside the byte window, or a cut of the
		// last line when it alone is too long.
		if nl != -1 && nl+1 < len(b) {
			start = nl + 1
		} else {
			start = characterStart(b, from)
		}
	}
	return start, len(b)
}

// CharacterEnd is the last character boundary at or before index.
func CharacterEnd(b []byte, index int) int {
	end := index
	for end > 0 && end < len(b) && b[end]&0xc0 == 0x80 {
		end--
	}
	return end
}

// characterStart is the first character boundary at or after index.
func characterStart(b []byte, index int) int {
	start := index
	for start < len(b) && b[start]&0xc0 == 0x80 {
		start++
	}
	return start
}

func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{newline})
	if b[len(b)-1] != newline {
		n++
	}
	return n
}

// UTF8ByteLength is the UTF-8 byte length of content, malformed bytes counted
// as the U+FFFD they decode to (pi utf8ByteLength counts a lone surrogate as
// three bytes).
func UTF8ByteLength(content string) int {
	return len(wellFormed(content))
}

func countNewlines(text string) int {
	return strings.Count(text, "\n")
}

// lines is the number of lines of text with newlines newlines; a final
// unterminated line counts.
func lines(newlines int, terminated bool) int {
	if terminated {
		return newlines
	}
	return newlines + 1
}

type storedChunk struct {
	text     string
	bytes    int
	newlines int
}

// OutputBuffer is the bounded running output of one stream, such as a tool
// call's or a process's. Accepting a chunk costs time proportional to the
// chunk: head retention stops storing once the window is full, and tail
// retention drops stored text the window no longer needs when it snapshots.
// Counts of the whole stream are kept so the dropped totals stay exact. It is
// not safe for concurrent use.
type OutputBuffer struct {
	limits Limits
	// decoder decodes byte chunks. A string chunk or a skip ends an
	// incomplete character of earlier bytes (it becomes U+FFFD); the next
	// byte chunk then starts a new character. Only a byte-order mark at the
	// very start of the output is dropped, never a U+FEFF later in it.
	decoder decoder
	started bool
	// chunks are the stored chunks: for head the start of the stream, for
	// tail a suffix that still contains the next window.
	chunks         []storedChunk
	storedBytes    int
	storedNewlines int
	full           bool
	totalBytes     int
	totalNewlines  int
	endsWithNL     bool
}

// NewOutputBuffer makes a buffer with the given limits.
func NewOutputBuffer(limits Limits) *OutputBuffer {
	return &OutputBuffer{limits: limits, endsWithNL: true}
}

// StoredBytes is the number of bytes held; bounded by the limits plus one
// chunk.
func (o *OutputBuffer) StoredBytes() int { return o.storedBytes }

// Push accepts a byte chunk; it reports whether anything was accepted.
// skipped, when not nil, is output omitted right before the chunk, which must
// be more than the tail window by at least one byte or line; only tail
// retention accepts it.
func (o *OutputBuffer) Push(chunk []byte, skipped *Skip) (bool, error) {
	pending := ""
	if skipped != nil {
		pending = o.decoder.flush()
	}
	text := o.decoder.decode(chunk)
	first := !o.started && pending == "" && skipped == nil
	if first && strings.HasPrefix(text, "\ufeff") {
		text = text[len("\ufeff"):]
	}
	return o.push(pending, text, skipped)
}

// PushString accepts a text chunk; see Push. It ends an incomplete character
// left by an earlier byte chunk.
func (o *OutputBuffer) PushString(chunk string, skipped *Skip) (bool, error) {
	return o.push(o.decoder.flush(), wellFormed(chunk), skipped)
}

func (o *OutputBuffer) push(pending, text string, skipped *Skip) (bool, error) {
	if pending != "" || text != "" || skipped != nil {
		o.started = true
	}
	if skipped == nil {
		return o.accept(pending + text), nil
	}
	if o.limits.Retain != RetainTail {
		return false, ErrSkipNeedsTail
	}
	o.accept(pending)
	o.skip(*skipped)
	o.accept(text)
	return true, nil
}

// skip counts omitted output; nothing stored before it can be in the window
// once the text after it arrives.
func (o *OutputBuffer) skip(s Skip) {
	if s.Bytes == 0 {
		return
	}
	o.totalBytes += s.Bytes
	o.totalNewlines += s.Newlines
	o.endsWithNL = s.EndsWithNewline
	o.chunks = nil
	o.storedBytes = 0
	o.storedNewlines = 0
}

// End flushes an incomplete trailing character as a replacement character;
// call it when the stream ends.
func (o *OutputBuffer) End() {
	o.accept(o.decoder.flush())
}

func (o *OutputBuffer) accept(text string) bool {
	if text == "" {
		return false
	}
	n := len(text)
	newlines := countNewlines(text)
	o.totalBytes += n
	o.totalNewlines += newlines
	o.endsWithNL = strings.HasSuffix(text, "\n")
	if o.full {
		return true
	}
	o.chunks = append(o.chunks, storedChunk{text: text, bytes: n, newlines: newlines})
	o.storedBytes += n
	o.storedNewlines += newlines
	if o.limits.Retain == RetainHead {
		// Nothing past a full window is ever needed.
		o.full = o.storedBytes > o.limits.MaxBytes || o.storedNewlines >= o.limits.MaxLines
		return true
	}
	// Drop leading chunks while the rest still holds more than a window:
	// more than MaxBytes bytes or MaxLines newlines, plus one, so the
	// window's line start can still be found. Each chunk is dropped once.
	for len(o.chunks) > 1 {
		first := o.chunks[0]
		bytesAfter := o.storedBytes - first.bytes
		newlinesAfter := o.storedNewlines - first.newlines
		if bytesAfter <= o.limits.MaxBytes+1 && newlinesAfter <= o.limits.MaxLines+1 {
			break
		}
		o.chunks = o.chunks[1:]
		o.storedBytes = bytesAfter
		o.storedNewlines = newlinesAfter
	}
	return true
}

// Snapshot returns the retained, sanitized output and what the limits dropped
// from the whole stream.
func (o *OutputBuffer) Snapshot() Bounded {
	var stored string
	if len(o.chunks) == 1 {
		stored = o.chunks[0].text
	} else {
		var b strings.Builder
		for _, c := range o.chunks {
			b.WriteString(c.text)
		}
		stored = b.String()
	}
	kept := BoundOutput(stored, o.limits)
	storedLines := lines(o.storedNewlines, stored == "" || strings.HasSuffix(stored, "\n"))
	keptLines := storedLines - kept.DroppedLines
	// Tail windows never reach back before this one, but finding a later
	// window's first line needs what precedes it: keep the shortest suffix
	// longer than the window by a byte or a line, as accept does.
	if o.limits.Retain == RetainTail || len(o.chunks) > 1 {
		text := stored
		n := o.storedBytes
		if o.limits.Retain == RetainTail {
			text = tailMargin(stored, o.limits)
			n = len(text)
		}
		o.chunks = nil
		o.storedNewlines = 0
		if text != "" {
			c := storedChunk{text: text, bytes: n, newlines: countNewlines(text)}
			o.chunks = []storedChunk{c}
			o.storedNewlines = c.newlines
		}
		o.storedBytes = n
	}
	return Bounded{
		Text:         SanitizeOutput(kept.Text),
		DroppedBytes: o.totalBytes - kept.Bytes,
		DroppedLines: lines(o.totalNewlines, o.endsWithNL) - keptLines,
	}
}

// tailMargin is the shortest suffix of text with more than MaxBytes bytes or
// more than MaxLines newlines, or all of it. The tail window of any text that
// ends with this suffix, followed by anything, is the same as of text
// followed by it.
func tailMargin(text string, limits Limits) string {
	b := []byte(text)
	byteStart := 0
	if len(b) > limits.MaxBytes {
		byteStart = CharacterEnd(b, len(b)-limits.MaxBytes-1)
	}
	lineStart := 0
	newlines := 0
	for i := lastIndexAtOrBefore(b, newline, len(b)-1); i != -1; i = lastIndexAtOrBefore(b, newline, i-1) {
		newlines++
		if newlines > limits.MaxLines {
			lineStart = i
			break
		}
		if i == 0 {
			break
		}
	}
	start := max(byteStart, lineStart)
	if start == 0 {
		return text
	}
	return text[start:]
}
