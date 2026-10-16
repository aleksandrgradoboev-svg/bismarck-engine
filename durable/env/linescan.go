package env

import (
	"bytes"
	"errors"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

const maxSafeInteger = 1<<53 - 1

// errInvalidLineRange is a LineRange that selects nothing or is negative.
var errInvalidLineRange = errors.New("Invalid line range")

// LineScanner computes a LineScan from a file's bytes fed in order, so an
// environment can scan any size in bounded memory (pi LineScanner). Decoded
// sizes come from the same streaming WHATWG decoder as decoding the whole
// file at once.
type LineScanner struct {
	startLine, endLine int64 // endLine 0: to the end
	position           int64
	newlines           int64
	lineStart          int64
	start, end         int64
	hasStart, hasEnd   bool
	firstLineEnd       int64
	hasFirstLineEnd    bool
	lastLineStart      int64
	hasLastLineStart   bool
	selectedBytes      int64
	firstLineBytes     int64
	selection          *jstext.TextDecoder
	firstLine          *jstext.TextDecoder
	// head holds the first bytes until it is known whether they are a
	// byte-order mark; released is set once they are.
	head     []byte
	released bool
	bom      bool
}

// NewLineScanner scans lines r.Start up to r.End.
func NewLineScanner(r LineRange) (*LineScanner, error) {
	if r.Start < 0 || r.Start > maxSafeInteger || r.End < 0 || r.End > maxSafeInteger || (r.End != 0 && r.End <= r.Start) {
		return nil, errInvalidLineRange
	}
	s := &LineScanner{startLine: r.Start, endLine: r.End}
	if r.Start == 0 {
		s.begin(0)
	}
	return s, nil
}

// lastSelected reports whether line is the last selected line.
func (s *LineScanner) lastSelected(line int64) bool { return s.endLine != 0 && line == s.endLine-1 }

// Push feeds the next bytes of the file.
func (s *LineScanner) Push(chunk []byte) {
	if !s.released {
		take := min(3-len(s.head), len(chunk))
		s.head = append(s.head, chunk[:take]...)
		if len(s.head) < 3 {
			return
		}
		s.releaseHead()
		chunk = chunk[take:]
	}
	s.process(chunk)
}

func (s *LineScanner) releaseHead() {
	head := s.head
	s.head, s.released = nil, true
	s.bom = bytes.HasPrefix(head, []byte{0xef, 0xbb, 0xbf})
	s.process(head)
}

func (s *LineScanner) process(chunk []byte) {
	base := s.position
	from := 0
	for offset := 0; ; {
		i := bytes.IndexByte(chunk[offset:], '\n')
		if i < 0 {
			break
		}
		index := offset + i
		// The newline ends line s.newlines; it belongs to the selection
		// between selected lines only.
		s.feed(chunk, base, from, index)
		line := s.newlines
		position := base + int64(index)
		if line == s.startLine {
			s.endFirstLine(position)
		}
		if s.lastSelected(line) {
			s.endSelection(position)
		}
		s.feed(chunk, base, index, index+1)
		from = index + 1
		offset = index + 1
		s.newlines++
		s.lineStart = position + 1
		if s.newlines == s.startLine {
			s.begin(s.lineStart)
		}
		if s.lastSelected(s.newlines) {
			s.lastLineStart, s.hasLastLineStart = s.lineStart, true
		}
	}
	s.feed(chunk, base, from, len(chunk))
	s.position += int64(len(chunk))
}

// Finish ends the file and returns the scan.
func (s *LineScanner) Finish() LineScan {
	if !s.released {
		s.releaseHead()
	}
	size := s.position
	if !s.hasStart {
		return LineScan{Newlines: s.newlines, Start: size, End: size, FirstLineEnd: size, LastLineStart: size}
	}
	if !s.hasFirstLineEnd {
		s.endFirstLine(size)
	}
	if !s.hasEnd {
		s.endSelection(size)
	}
	lastLineStart := s.lineStart // a selection past the last line ends with the last line
	if s.hasLastLineStart {
		lastLineStart = s.lastLineStart
	}
	return LineScan{
		Newlines:       s.newlines,
		Start:          s.start,
		End:            s.end,
		FirstLineEnd:   s.firstLineEnd,
		LastLineStart:  lastLineStart,
		SelectedBytes:  s.selectedBytes,
		FirstLineBytes: s.firstLineBytes,
	}
}

func (s *LineScanner) begin(start int64) {
	s.start, s.hasStart = start, true
	if s.lastSelected(s.startLine) {
		s.lastLineStart, s.hasLastLineStart = start, true
	}
	s.selection = &jstext.TextDecoder{IgnoreBOM: true}
	s.firstLine = &jstext.TextDecoder{IgnoreBOM: true}
}

func (s *LineScanner) endFirstLine(position int64) {
	s.firstLineEnd, s.hasFirstLineEnd = position, true
	if s.firstLine != nil {
		s.firstLineBytes += int64(len(s.firstLine.Flush()))
	}
	s.firstLine = nil
}

func (s *LineScanner) endSelection(position int64) {
	s.end, s.hasEnd = position, true
	if s.selection != nil {
		s.selectedBytes += int64(len(s.selection.Flush()))
	}
	s.selection = nil
}

// feed hands chunk[from:to], which starts at file offset base, to the
// decoders of the ranges still open.
func (s *LineScanner) feed(chunk []byte, base int64, from, to int) {
	// Decoding the whole file drops a leading byte-order mark.
	if s.bom && base+int64(from) < 3 {
		from = min(to, int(3-base))
	}
	if to <= from {
		return
	}
	b := chunk[from:to]
	if s.selection != nil {
		s.selectedBytes += int64(len(s.selection.Decode(b)))
	}
	if s.firstLine != nil {
		s.firstLineBytes += int64(len(s.firstLine.Decode(b)))
	}
}
