package output

import (
	"strings"
	"unicode/utf8"
)

// decoder is the WHATWG UTF-8 decoder (Encoding Standard, "UTF-8 decoder")
// with replacement errors and no BOM handling — what pi's
// TextDecoder("utf-8", { ignoreBOM: true }) does. It differs from Go's
// utf8.DecodeRune on malformed input: one U+FFFD stands for a maximal subpart
// of a sequence, not for each byte of it.
//
// With stream set, decode holds back the bytes of an incomplete trailing
// character for the next call; flush ends them as one U+FFFD.
type decoder struct {
	codePoint  rune
	needed     int
	seen       int
	lowerBound byte
	upperBound byte
}

func (d *decoder) reset() {
	d.codePoint, d.needed, d.seen = 0, 0, 0
	d.lowerBound, d.upperBound = 0x80, 0xbf
}

// decode decodes bytes, keeping an incomplete trailing character pending.
func (d *decoder) decode(bytes []byte) string {
	var b strings.Builder
	b.Grow(len(bytes))
	if d.lowerBound == 0 {
		d.reset()
	}
	for i := 0; i < len(bytes); i++ {
		c := bytes[i]
		if d.needed == 0 {
			switch {
			case c <= 0x7f:
				b.WriteByte(c)
			case c >= 0xc2 && c <= 0xdf:
				d.needed, d.codePoint = 1, rune(c&0x1f)
			case c >= 0xe0 && c <= 0xef:
				if c == 0xe0 {
					d.lowerBound = 0xa0
				} else if c == 0xed {
					d.upperBound = 0x9f
				}
				d.needed, d.codePoint = 2, rune(c&0x0f)
			case c >= 0xf0 && c <= 0xf4:
				if c == 0xf0 {
					d.lowerBound = 0x90
				} else if c == 0xf4 {
					d.upperBound = 0x8f
				}
				d.needed, d.codePoint = 3, rune(c&0x07)
			default:
				b.WriteRune(utf8.RuneError)
			}
			continue
		}
		if c < d.lowerBound || c > d.upperBound {
			// The sequence ends here as one U+FFFD and the byte is read again.
			d.reset()
			b.WriteRune(utf8.RuneError)
			i--
			continue
		}
		d.lowerBound, d.upperBound = 0x80, 0xbf
		d.codePoint = d.codePoint<<6 | rune(c&0x3f)
		d.seen++
		if d.seen != d.needed {
			continue
		}
		b.WriteRune(d.codePoint)
		d.reset()
	}
	return b.String()
}

// flush ends an incomplete pending character as U+FFFD.
func (d *decoder) flush() string {
	if d.needed == 0 {
		return ""
	}
	d.reset()
	return string(utf8.RuneError)
}

// wellFormed returns s as text: valid UTF-8 unchanged, and malformed bytes
// replaced as the WHATWG decoder replaces them. A Go string can hold any
// bytes, where a JS string holds UTF-16 that TextEncoder encodes with every
// lone surrogate as U+FFFD; this is the counterpart that makes byte counts
// and slices agree with pi's.
func wellFormed(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var d decoder
	return d.decode([]byte(s)) + d.flush()
}
