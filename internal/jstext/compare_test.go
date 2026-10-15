package jstext

import "testing"

// Each pair is ordered as node orders it (`a < b` true); byte order agrees
// except where noted.
func TestCompareUTF16(t *testing.T) {
	for _, tc := range []struct{ less, more string }{
		{"a", "b"},
		{"", "a"},
		{"a", "ab"},
		{"é", "｡"},
		{"😀", "｡"}, // byte order puts ｡ (EF BD A1) first
		{"𝔞", "ﬀ"}, // byte order puts ﬀ (EF AC 80) first
		{"😀", "😁"}, // same high surrogate: the low surrogates decide
		{"\U00010000", "\U0010FFFF"},
		{"\uD7FF", "😀"}, // below the surrogates, byte order agrees
	} {
		if c := CompareUTF16(tc.less, tc.more); c >= 0 {
			t.Errorf("CompareUTF16(%q, %q) = %d, want < 0", tc.less, tc.more, c)
		}
		if c := CompareUTF16(tc.more, tc.less); c <= 0 {
			t.Errorf("CompareUTF16(%q, %q) = %d, want > 0", tc.more, tc.less, c)
		}
	}
	if c := CompareUTF16("😀a", "😀a"); c != 0 {
		t.Errorf("equal strings compare %d", c)
	}
}

// Invalid bytes all decode to U+FFFD, so strings that differ only there fall
// back to byte order: distinct strings never compare equal, and a sort by
// CompareUTF16 has one outcome whatever order its input came in.
func TestCompareUTF16InvalidUTF8IsTotal(t *testing.T) {
	for _, tc := range []struct{ less, more string }{
		{"\xfe", "\xff"},
		{"a\xef\xbf\xbd", "a\xff"}, // U+FFFD itself, then an invalid byte
		{"/a/\xfe.go", "/a/\xff.go"},
	} {
		if c := CompareUTF16(tc.less, tc.more); c >= 0 {
			t.Errorf("CompareUTF16(%q, %q) = %d, want < 0", tc.less, tc.more, c)
		}
		if c := CompareUTF16(tc.more, tc.less); c <= 0 {
			t.Errorf("CompareUTF16(%q, %q) = %d, want > 0", tc.more, tc.less, c)
		}
	}
}
