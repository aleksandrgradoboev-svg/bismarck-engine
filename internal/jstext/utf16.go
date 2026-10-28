package jstext

import "unicode/utf16"

// UTF16Len is the length of s in UTF-16 code units: JavaScript's
// String.prototype.length.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
