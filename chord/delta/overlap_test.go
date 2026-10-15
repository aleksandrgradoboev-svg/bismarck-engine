package delta

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// delta.test.ts "overlap". The two upstream cases, verbatim.
func TestOverlap(t *testing.T) {
	// "finds an overlap shorter than the long probe": the 64-unit probe is
	// longer than b, so the head is all of b; only the 1-unit fallback finds it.
	if got := Overlap("abcdefgh", "defghxyz", 65_536); got != 5 {
		t.Errorf("Overlap(abcdefgh, defghxyz, 65536) = %d, want 5", got)
	}
	// "honors a disabled scan"
	if got := Overlap("abcdef", "defghi", 0); got != 0 {
		t.Errorf("Overlap(abcdef, defghi, 0) = %d, want 0", got)
	}
}

// The contract upstream states above the function: the returned n always
// satisfies a.slice(a.length - n) === b.slice(0, n), in UTF-16 code units.
// Giving up returns 0, which emits a set — larger, never wrong.
func TestOverlapInvariant(t *testing.T) {
	cases := []struct{ a, b string }{
		{"abcdefgh", "defghxyz"},
		{"", "abc"},
		{"abc", ""},
		{"abc", "xyz"},
		{"abcabc", "abcabcabc"},
		{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "xxxx"},
		{strings.Repeat("=", 10_000), strings.Repeat("=", 100) + "done"},
		{strings.Repeat("build ok\n", 2_000), "build ok\nbuild ok\ntail"},
		{"héllo wörld", "wörld again"},
		{"emoji 😀😀😀", "😀😀 more"},
		{"日本語テキスト", "テキスト続き"},
		{"ab😀😀😀", "😀😀c"},
		{"zz😁😀", "😀q"},
	}
	for _, tc := range cases {
		a, b := utf16.Encode([]rune(tc.a)), utf16.Encode([]rune(tc.b))
		for _, scan := range []int{0, 1, 3, 5, 64, 1_000, 1 << 20} {
			n := Overlap(tc.a, tc.b, scan)
			if n < 0 || n > len(a) || n > len(b) {
				t.Fatalf("Overlap(%q, %q, %d) = %d: out of range", tc.a, tc.b, scan, n)
			}
			if suffix, prefix := a[len(a)-n:], b[:n]; string(utf16.Decode(suffix)) != string(utf16.Decode(prefix)) {
				t.Errorf("Overlap(%q, %q, %d) = %d: suffix %q != prefix %q", tc.a, tc.b, scan, n, string(utf16.Decode(suffix)), string(utf16.Decode(prefix)))
			}
			if n > 0 && scan > 0 && n > scan {
				t.Errorf("Overlap(%q, %q, %d) = %d: exceeds scan", tc.a, tc.b, scan, n)
			}
		}
	}
}

// Pinned against pi: every `want` is the number upstream's overlap returns for
// the same input (packages/chord/src/delta/index.ts at 64eeb82a4, run under
// node; all re-checked at 9a139c62b, where overlap is unchanged). The numbers
// must match exactly — including the ones the candidate budget turns into 0 —
// because the tracker puts them on the wire as a "t" count, and a Go producer
// and a pi producer must emit the same ops for the same mutation.
func TestOverlapMatchesUpstream(t *testing.T) {
	// U+1F600 and U+1F601 share their high surrogate, D83D; U+1F9D1 is in the
	// next block, D83E.
	const emoji, grin, other = "😀", "😁", "🧑"
	// T is exactly 64 characters: one full probe head.
	const T = "0123456789ABCDEF" + "GHIJKLMNOPQRSTUVWXYZ" + "abcdefghijklmnopqrstuvwxyz" + "!!"
	cases := []struct {
		name string
		a, b string
		scan int
		want int
	}{
		// A long repetitive tail: the 64-unit head matches at every line start,
		// the first eight candidates are all longer than b, and the budget gives
		// up — 0, which emits a set. Larger, never wrong.
		{"long repetitive tail gives up", strings.Repeat("line\n", 100), strings.Repeat("line\n", 40) + "next\n", 1 << 16, 0},
		// The scan bounds the tail that is searched, and so the answer.
		{"scan bounds the answer", strings.Repeat("line\n", 100), strings.Repeat("line\n", 40) + "next\n", 25, 25},
		// A tail short enough that the answer is within the first eight candidates.
		{"short repetitive tail", strings.Repeat("line\n", 10), strings.Repeat("line\n", 4) + "next\n", 1 << 16, 20},
		// A window that stepped by ten: the long head hits once, at the answer.
		{"window step", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "klmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijNEW", 1 << 16, 62},
		// One run of a single character: the 4-unit head matches at every
		// position, and the fifth candidate is the answer.
		{"single character run", "aaaaaaaa", "aaaa", 1 << 16, 4},
		// The answer is exactly the eighth candidate — the last the budget
		// allows. A budget of seven would give up on it.
		{"eighth candidate", strings.Repeat("a", 11), "aaaa", 1 << 16, 4},
		// ...and one past it, the ninth, is past the budget: 0. A budget of
		// nine would find it.
		{"ninth candidate", strings.Repeat("a", 12), "aaaa", 1 << 16, 0},
		// "ab" repeats 20 times in the tail; the first eight candidates are all
		// too long to be prefixes of b. Giving up returns 0.
		{"gives up past the candidate budget", strings.Repeat("ab", 20), "abab", 1 << 16, 0},
		// The same tail with a scan of 8: the third candidate is the answer.
		{"scan rescues the budget", strings.Repeat("ab", 20), "abab", 8, 4},
		// The probe is 64 units, no more and no less: a 16-unit head would
		// match at each of the nine block starts and spend the budget; a head
		// of all of b would never occur in a. Only the 64-unit head lands on
		// the one candidate.
		{"probe is 64 units", strings.Repeat("0123456789ABCDEF-", 9) + T, T + "END", 1 << 16, 64},
		{"disjoint", "abc", "xyz", 1 << 16, 0},
		{"equal", "same", "same", 1 << 16, 4},
		{"b shorter", "prefix-same", "same", 1 << 16, 4},
		// a.slice(a.length - scan) with a negative scan is the empty tail.
		{"negative scan", "abcdef", "defghi", -3, 0},

		// Non-ASCII: every count is UTF-16 code units. A 64-unit head is 32
		// emoji, not 16; a scan of 5 reaches back 5 units, not 5 bytes.
		//
		// 33 emoji overlap at the end of a, but a's first block of 21 emoji
		// also contains the head. In code units the head is 32 emoji, longer
		// than that block, so the block is skipped and the answer is the first
		// candidate. A 64-BYTE head (16 emoji) matched at six positions inside
		// the block and spent the budget before reaching the answer: 0.
		{"astral, default scan", strings.Repeat(emoji, 21) + "z" + strings.Repeat(emoji, 35), strings.Repeat(emoji, 33) + "w", 1 << 16, 66},
		// A scan of 70 units is 35 emoji: the whole answer is still in the tail.
		{"astral, scan bounds in units", strings.Repeat(emoji, 21) + "z" + strings.Repeat(emoji, 35), strings.Repeat(emoji, 33) + "w", 70, 66},
		// One unit back reaches é whole; one byte back would split it.
		{"scan of one unit", "xé", "éy", 1, 1},
		{"scan of five units", "héllo wörld", "wörld again", 5, 5},
		{"bmp", "héllo wörld", "wörld again", 1 << 16, 5},
		{"cjk, scan of five", "日本語テキスト", "テキスト続き", 5, 4},
		{"cjk", "日本語テキスト", "テキスト続き", 1 << 16, 4},
		{"emoji tail", "emoji 😀😀😀", "😀😀 more", 1 << 16, 4},
		{"emoji tail, scan of five", "emoji 😀😀😀", "😀😀 more", 5, 4},
		// One unit is half an emoji: the tail opens with a lone low surrogate
		// in pi, which nothing in b can match.
		{"emoji tail, scan of one", "emoji 😀😀😀", "😀😀 more", 1, 0},
		{"scan splits a pair", "ab" + strings.Repeat(emoji, 3), strings.Repeat(emoji, 2) + "c", 3, 2},
		{"scan splits a pair, longer", "ab" + strings.Repeat(emoji, 3), strings.Repeat(emoji, 2) + "c", 5, 4},
		{"scan on a pair boundary", "ab" + strings.Repeat(emoji, 3), strings.Repeat(emoji, 2) + "c", 4, 4},

		// The 64th unit of b is the high half of a pair. pi's head ends with
		// that lone high surrogate, so a candidate must continue with an emoji
		// from the same block of 1024 — and the full check then tells 😀 from 😁.
		{"head splits a pair", "prefix-" + strings.Repeat("x", 63) + emoji + "yy", strings.Repeat("x", 63) + emoji + "yyEND", 1 << 16, 67},
		{"head splits a pair, same high surrogate", "prefix-" + strings.Repeat("x", 63) + grin + "yy", strings.Repeat("x", 63) + emoji + "yyEND", 1 << 16, 0},
		// Nine candidates share the head's high surrogate; the budget gives up
		// before the tenth, the answer.
		{"head splits a pair, budget", strings.Repeat(strings.Repeat("x", 63)+grin+"|", 9) + strings.Repeat("x", 63) + emoji, strings.Repeat("x", 63) + emoji + "END", 1 << 16, 0},
		// Nine decoys continue with an emoji from ANOTHER block: none is an
		// occurrence of pi's head, so none costs a candidate.
		{"head splits a pair, other block is no candidate", strings.Repeat(strings.Repeat("x", 63)+other+"|", 9) + strings.Repeat("x", 63) + emoji, strings.Repeat("x", 63) + emoji + "END", 1 << 16, 65},

		// The 1-unit fallback with an astral b: the head is a lone high
		// surrogate, which every emoji in the same block matches.
		{"astral fallback", "zz" + grin + emoji, emoji + "q", 1 << 16, 2},
		{"astral fallback, same high surrogate", "zz" + grin + "q" + grin, grin + "qz", 1 << 16, 2},
		{"astral fallback, budget", strings.Repeat(grin, 9) + emoji, emoji + "q", 1 << 16, 0},
		{"astral fallback, other block is no candidate", strings.Repeat(other, 9) + emoji, emoji + "q", 1 << 16, 2},
		// A two-unit b runs both probes: the whole emoji, then its high half.
		{"single astral b", "ab" + emoji, emoji, 1 << 16, 2},
		{"single astral b, no match", "ab" + grin, emoji, 1 << 16, 0},
	}
	for _, tc := range cases {
		if got := Overlap(tc.a, tc.b, tc.scan); got != tc.want {
			t.Errorf("%s: overlap = %d, want %d", tc.name, got, tc.want)
		}
	}
}
