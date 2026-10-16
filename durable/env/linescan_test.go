package env

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// pi test/env-line-scan.test.ts. The reference is jstext's TextDecoder,
// which internal/jstext checks against Node's.

// random is pi's mulberry32, so the seeds draw the same files.
func random(seed uint32) func() float64 {
	state := seed
	return func() float64 {
		state += 0x6d2b79f5
		t := state
		t = (t ^ (t >> 15)) * (t | 1)
		t ^= t + (t^(t>>7))*(t|61)
		return float64(t^(t>>14)) / 4294967296
	}
}

// pieces are newlines, ASCII, a byte-order mark, valid multi-byte sequences,
// and bytes that form invalid or truncated sequences.
var pieces = [][]byte{
	{0x0a}, {0x0a}, {0x61}, {0x62, 0x63}, {0xef, 0xbb, 0xbf}, {0xc3, 0xa9}, {0xe2, 0x82, 0xac},
	{0xf0, 0x9f, 0x98, 0x80}, {0xe2, 0x82}, {0xff}, {0x80}, {0xf0, 0x9f}, {0x0d, 0x0a},
}

func randomFile(next func() float64) []byte {
	var out []byte
	n := int(next() * 60)
	for range n {
		out = append(out, pieces[int(next()*float64(len(pieces)))]...)
	}
	return out
}

func TestStreamDecodingKeepsAU_FEFFThatDoesNotStartTheStream(t *testing.T) {
	data := []byte{0xe2, 0x82, 0xef, 0xbb, 0xbf, 0x61}
	var d jstext.TextDecoder
	var text strings.Builder
	for _, b := range data {
		text.WriteString(d.Decode([]byte{b}))
	}
	text.WriteString(d.Flush())
	if got := text.String(); got != "�\ufeffa" || got != jstext.DecodeText(data) {
		t.Fatalf("%q", got)
	}
}

func TestStreamDecodingAgreesWithDecodingTheWholeStream(t *testing.T) {
	for seed := uint32(1); seed <= 5000; seed++ {
		next := random(seed)
		data := randomFile(next)
		var d jstext.TextDecoder
		var text strings.Builder
		for offset := 0; offset < len(data); {
			size := 1 + int(next()*7)
			text.WriteString(d.Decode(data[offset:min(offset+size, len(data))]))
			offset += size
		}
		text.WriteString(d.Flush())
		if got, want := text.String(), jstext.DecodeText(data); got != want {
			t.Fatalf("seed %d: %q, want %q", seed, got, want)
		}
	}
}

func decodeRange(data []byte, from, to int64) string {
	d := jstext.TextDecoder{IgnoreBOM: from > 0}
	return d.Decode(data[from:to]) + d.Flush()
}

func TestLineScannerAgreesWithDecodingAndSplittingTheWholeFile(t *testing.T) {
	for seed := uint32(1); seed <= 5000; seed++ {
		next := random(seed)
		data := randomFile(next)
		lines := strings.Split(jstext.DecodeText(data), "\n")
		startLine := int64(next() * float64(len(lines)+2))
		var endLine int64
		if next() >= 0.3 {
			endLine = startLine + 1 + int64(next()*float64(len(lines)+1))
		}
		scanner, err := NewLineScanner(LineRange{Start: startLine, End: endLine})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for offset := 0; offset < len(data); {
			size := 1 + int(next()*7)
			scanner.Push(data[offset:min(offset+size, len(data))])
			offset += size
		}
		scan := scanner.Finish()
		if scan.Newlines != int64(len(lines)-1) {
			t.Fatalf("seed %d: %d newlines, want %d", seed, scan.Newlines, len(lines)-1)
		}
		end := int64(len(lines))
		if endLine != 0 {
			end = min(endLine, end)
		}
		selected := strings.Join(lines[min(startLine, int64(len(lines))):max(end, min(startLine, int64(len(lines))))], "\n")
		if got := decodeRange(data, scan.Start, scan.End); got != selected {
			t.Fatalf("seed %d: selection %q, want %q", seed, got, selected)
		}
		if scan.SelectedBytes != int64(len(selected)) {
			t.Fatalf("seed %d: %d selected bytes, want %d", seed, scan.SelectedBytes, len(selected))
		}
		if startLine < int64(len(lines)) {
			if got := decodeRange(data, scan.Start, scan.FirstLineEnd); got != lines[startLine] {
				t.Fatalf("seed %d: first line %q, want %q", seed, got, lines[startLine])
			}
			if scan.FirstLineBytes != int64(len(lines[startLine])) {
				t.Fatalf("seed %d: %d first line bytes, want %d", seed, scan.FirstLineBytes, len(lines[startLine]))
			}
			lastLine := end - 1
			lastEnd := int64(len(data))
			if lastLine+1 < int64(len(lines)) {
				lastEnd = scan.LastLineStart + int64(bytes.IndexByte(data[scan.LastLineStart:], '\n'))
			}
			if got := decodeRange(data, scan.LastLineStart, lastEnd); got != lines[lastLine] {
				t.Fatalf("seed %d: last line %q, want %q", seed, got, lines[lastLine])
			}
		} else if scan.Start != int64(len(data)) || scan.End != int64(len(data)) || scan.SelectedBytes != 0 {
			t.Fatalf("seed %d: scan past the last line %+v", seed, scan)
		}
	}
}

func TestLineScannerRejectsEmptyOrInvalidRanges(t *testing.T) {
	for _, r := range []LineRange{{Start: 2, End: 2}, {Start: -1}, {Start: 3, End: 1}} {
		if _, err := NewLineScanner(r); err == nil {
			t.Errorf("%+v was accepted", r)
		}
	}
}
