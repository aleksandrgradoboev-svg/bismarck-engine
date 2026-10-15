package codemode

import (
	"errors"
	"regexp"
	"testing"
)

// Ported from pi v1.2.0 packages/codemode/test/source.test.ts.

func i64(n int64) *int64 { return &n }

func TestParseSourcePlain(t *testing.T) {
	for _, in := range []string{"text('hi')", "// just a comment\nreturn 1", "// @optionsx {}\ntext(1)", `text("// @options: {}")`} {
		p, err := ParseSource(in)
		if err != nil || p.Code != in || p.Options != (SourceOptions{}) {
			t.Errorf("%q: %+v %v", in, p, err)
		}
	}
}

func TestParseSourceOptions(t *testing.T) {
	p, err := ParseSource("// @options: {\"timeout_ms\": 10}\nconst a = 1;\ntext(a)")
	if err != nil || p.Code != "\nconst a = 1;\ntext(a)" || *p.Options.TimeoutMs != 10 || p.Options.MaxOutputTokens != nil {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ParseSource("  // @options:{\"max_output_tokens\":0,\"timeout_ms\":1500}\r\ntext(1)")
	if err != nil || *p.Options.MaxOutputTokens != 0 || *p.Options.TimeoutMs != 1500 {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ParseSource("// @options: {}\ntext(1)")
	if err != nil || p.Code != "\ntext(1)" || p.Options != (SourceOptions{}) {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestParseSourceRejectsLateOptionsLine(t *testing.T) {
	for in, line := range map[string]string{
		"// count things\n// @options: {\"timeout_ms\": 1000}\nwhile (true) {}":       "2",
		"\n  // @options: {\"timeout_ms\": 1000}\nwhile (true) {}":                    "2",
		"text(1)\r\n// @options: {\"timeout_ms\": 1}":                                 "2",
		"// @options: {\"timeout_ms\": 1}\ntext(1)\n// @options: {\"timeout_ms\": 2}": "3",
	} {
		_, err := ParseSource(in)
		var se *SourceError
		if !errors.As(err, &se) || se.Message != "The // @options: line must be the first line of the input; found one on line "+line {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestParseSourceRejectsInvalid(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `Expected JavaScript source text \(non-empty\)`},
		{"  \n", `Expected JavaScript source text \(non-empty\)`},
		{"// @options:\ntext(1)", `@options must be a JSON object with supported fields`},
		{"// @options: {timeout_ms: 1}\ntext(1)", `@options must be valid JSON with supported fields`},
		{"// @options: [1]\ntext(1)", `@options must be a JSON object with supported fields`},
		{"// @options: {\"yield\": 1}\ntext(1)", "^@options only supports `max_output_tokens` and `timeout_ms`; got `yield`$"},
		{"// @options: {\"max_output_tokens\": 1.5}\ntext(1)", "^@options field `max_output_tokens` must be a non-negative safe integer$"},
		{"// @options: {\"timeout_ms\": 0}\ntext(1)", "@options field `timeout_ms` must be a positive integer"},
		{"// @options: {\"timeout_ms\": 1}", "^The @options line must be followed by JavaScript source on subsequent lines$"},
		{"// @options: {\"timeout_ms\": 1}\n  \n", "^The @options line must be followed by JavaScript source on subsequent lines$"},
	}
	for _, c := range cases {
		_, err := ParseSource(c.in)
		var se *SourceError
		if !errors.As(err, &se) || !regexp.MustCompile(c.want).MatchString(se.Message) {
			t.Errorf("%q: %v, want /%s/", c.in, err, c.want)
		}
	}
	// The JSON error is V8's own text.
	_, err := ParseSource("// @options: {timeout_ms: 1}\ntext(1)")
	if want := "@options must be valid JSON with supported fields `max_output_tokens` and `timeout_ms`: Expected property name or '}' in JSON at position 1 (line 1 column 2)"; err.Error() != want {
		t.Errorf("got %q\nwant %q", err, want)
	}
}
