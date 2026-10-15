package jstext

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"unicode"
)

// whitespaceCapture is testdata/whitespace-node.json, written by
// testdata/capture-whitespace.mjs under node.
type whitespaceCapture struct {
	Node              string    `json:"node"`
	Trim              []rune    `json:"trim"`
	TrimStart         []rune    `json:"trimStart"`
	TrimEnd           []rune    `json:"trimEnd"`
	ParseFloatLeading []rune    `json:"parseFloatLeading"`
	Strings           []trimmed `json:"strings"`
}

type trimmed struct {
	In        string `json:"in"`
	Trim      string `json:"trim"`
	TrimStart string `json:"trimStart"`
	TrimEnd   string `json:"trimEnd"`
}

func loadCapture(t *testing.T) whitespaceCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/whitespace-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var c whitespaceCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("testdata/whitespace-node.json: %v; rerun capture-whitespace.mjs", err)
	}
	if len(c.Trim) == 0 {
		t.Fatal("testdata/whitespace-node.json carries no trim set; rerun capture-whitespace.mjs")
	}
	return c
}

// Every Unicode scalar value is whitespace to IsWhitespace exactly when node's
// trim, trimStart, trimEnd and parseFloat treat it as whitespace.
func TestWhitespaceMatchesNode(t *testing.T) {
	c := loadCapture(t)
	for name, set := range map[string][]rune{
		"trim": c.Trim, "trimStart": c.TrimStart, "trimEnd": c.TrimEnd, "parseFloat": c.ParseFloatLeading,
	} {
		var got []rune
		for r := rune(0); r <= 0x10ffff; r++ {
			if r >= 0xd800 && r <= 0xdfff {
				continue
			}
			if IsWhitespace(r) {
				got = append(got, r)
			}
		}
		if !slices.Equal(got, set) {
			t.Errorf("IsWhitespace set = %U\nnode %s %s set = %U", got, c.Node, name, set)
		}
	}
}

func TestTrimsMatchNode(t *testing.T) {
	for _, s := range loadCapture(t).Strings {
		if got := Trim(s.In); got != s.Trim {
			t.Errorf("Trim(%+q) = %+q, node says %+q", s.In, got, s.Trim)
		}
		if got := TrimStart(s.In); got != s.TrimStart {
			t.Errorf("TrimStart(%+q) = %+q, node says %+q", s.In, got, s.TrimStart)
		}
		if got := TrimEnd(s.In); got != s.TrimEnd {
			t.Errorf("TrimEnd(%+q) = %+q, node says %+q", s.In, got, s.TrimEnd)
		}
	}
}

// TestIsomorphicDecode: every byte becomes the one character with its value,
// whatever it is part of, as the Infra Standard's isomorphic decode reads
// header bytes.
func TestIsomorphicDecode(t *testing.T) {
	for b := 0; b < 256; b++ {
		got := []rune(IsomorphicDecode("a" + string([]byte{byte(b)}) + "z"))
		if len(got) != 3 || got[1] != rune(b) {
			t.Errorf("byte %#x decodes to %q, want a, U+%04X, z", b, string(got), b)
		}
	}
	for in, want := range map[string]string{
		"":             "",
		"gzip":         "gzip",
		"caf\xc3\xa9":  "caf\u00c3\u00a9",
		"\xc2\xa0":     "\u00c2\u00a0",
		"\xff\xfe\x00": "\u00ff\u00fe\x00",
	} {
		if got := IsomorphicDecode(in); got != want {
			t.Errorf("IsomorphicDecode(%+q) = %+q, want %+q", in, got, want)
		}
	}
}

// lowerCapture is testdata/lower-node.json, written by testdata/capture-lower.mjs
// under node.
type lowerCapture struct {
	Node    string `json:"node"`
	Unicode string `json:"unicode"`
	// Lower holds [code point, toLowerCase] for every scalar value that
	// toLowerCase changes.
	Lower   [][2]json.RawMessage `json:"lower"`
	Strings []struct {
		In    string `json:"in"`
		Lower string `json:"lower"`
	} `json:"strings"`
}

func loadLowerCapture(t *testing.T) lowerCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/lower-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var c lowerCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("testdata/lower-node.json: %v; rerun capture-lower.mjs", err)
	}
	if len(c.Lower) == 0 {
		t.Fatal("testdata/lower-node.json carries no mappings; rerun capture-lower.mjs")
	}
	return c
}

// Every Unicode scalar value lowers as node's toLowerCase lowers it. The one
// allowance is a character Go's own Unicode tables do not assign yet: it is
// newer than the tables x/text/cases selects for this toolchain, which lowers
// it to itself (see ToLower).
func TestToLowerMatchesNode(t *testing.T) {
	c := loadLowerCapture(t)
	want := make(map[rune]string, len(c.Lower))
	for _, e := range c.Lower {
		var r rune
		var lower string
		if json.Unmarshal(e[0], &r) != nil || json.Unmarshal(e[1], &lower) != nil {
			t.Fatalf("testdata/lower-node.json: bad entry %s; rerun capture-lower.mjs", e)
		}
		want[r] = lower
	}
	newer := 0
	for r := rune(0); r <= 0x10ffff; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		s := string(r)
		w, ok := want[r]
		if !ok {
			w = s
		}
		got := ToLower(s)
		if got == w {
			continue
		}
		if got == s && !assigned(r) {
			newer++
			continue
		}
		t.Errorf("ToLower(%+q) = %+q, node %s says %+q", s, got, c.Node, w)
	}
	if newer > 0 {
		t.Logf("%d characters newer than Unicode %s lower to themselves; node %s carries Unicode %s", newer, unicode.Version, c.Node, c.Unicode)
	}
}

// Final_Sigma and the one-to-many mappings depend on the whole string.
func TestToLowerStringsMatchNode(t *testing.T) {
	for _, s := range loadLowerCapture(t).Strings {
		if got := ToLower(s.In); got != s.Lower {
			t.Errorf("ToLower(%+q) = %+q, node says %+q", s.In, got, s.Lower)
		}
	}
}

// assigned reports whether Go's Unicode tables assign r a character.
func assigned(r rune) bool { return !unicode.Is(unicode.Cn, r) }
