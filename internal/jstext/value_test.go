package jstext

import (
	"encoding/json"
	"math"
	"testing"
)

// Boolean(JSON.parse(text)) in node, for each text.
func TestTruthyMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"null", false},
		{"false", false},
		{"true", true},
		{"0", false},
		{"-0", false},
		{"0.0", false},
		{"1e-400", false}, // 0 once parsed
		{"1e400", true},   // Infinity
		{"-1e400", true},  // -Infinity
		{`""`, false},
		{`"0"`, true},
		{`"false"`, true},
		{"[]", true},
		{"{}", true},
		{"[0]", true},
	} {
		v, err := Parse([]byte(tc.text))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tc.text, err)
		}
		if got := Truthy(v); got != tc.want {
			t.Errorf("Truthy(%s) = %v, node says %v", tc.text, got, tc.want)
		}
	}
}

func TestNumberSaturatesLikeJSONParse(t *testing.T) {
	for _, tc := range []struct {
		text string
		want float64
	}{
		{"1e400", math.Inf(1)},
		{"-1e400", math.Inf(-1)},
		{"1e-400", 0},
		{"2.5e-324", 5e-324},
		{"0.1", 0.1},
	} {
		if got := Number(json.Number(tc.text)); got != tc.want {
			t.Errorf("Number(%s) = %v, want %v", tc.text, got, tc.want)
		}
	}
	if got := Number(json.Number("x")); !math.IsNaN(got) {
		t.Errorf(`Number("x") = %v, want NaN`, got)
	}
}

func TestParseTakesOneValue(t *testing.T) {
	for _, text := range []string{"", "1 2", "{} x", "[1"} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("Parse(%q) accepted it", text)
		}
	}
	if v, err := Parse([]byte(" 1e400 \n")); err != nil || v != json.Number("1e400") {
		t.Errorf(`Parse(" 1e400 \n") = %v, %v`, v, err)
	}
}

// TestReparseMatchesJSONRoundTrip: node v26.4.0's
// JSON.parse(JSON.stringify(JSON.parse(input))), as String() reads it, for
// inputs whose only difference is a number past float64's range at some
// depth. The argument itself is left as it was.
func TestReparseMatchesJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"a":[1e400,{"b":-1e400}],"c":1}`, `{"a":[null,{"b":null}],"c":1}`},
		{`[[1e400]]`, `[[null]]`},
		{`1e400`, `null`},
		{`{"x":"1e400","y":1e308}`, `{"x":"1e400","y":1e+308}`},
	} {
		v, err := Parse([]byte(tc.in))
		if err != nil {
			t.Fatal(err)
		}
		before, _ := Stringify(v)
		got, err := Stringify(Reparse(v))
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Reparse(%s) = %s, node %s", tc.in, got, tc.want)
		}
		if after, _ := Stringify(v); after != before {
			t.Errorf("Reparse changed its argument: %s -> %s", before, after)
		}
	}
	if s, _ := ToString(Reparse(mustParse(t, `[1e400,"s",2]`))); s != ",s,2" {
		t.Errorf("String(round-tripped [1e400,\"s\",2]) = %q, node %q", s, ",s,2")
	}
}

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
