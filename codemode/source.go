package codemode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// The codemode source format: pi's source.ts. JavaScript, optionally preceded
// by one options line:
//
//	// @options: {"max_output_tokens": 2000, "timeout_ms": 30000}
//	const text = await tools.read({ path: "package.json" });
//	text(JSON.parse(text).name);

// OptionsPrefix starts the options line.
const OptionsPrefix = "// @options:"

// SourceGrammar is a Lark grammar for providers with grammar-constrained tool
// input. It only fixes the shape of the options line; ParseSource checks the
// options JSON and the code.
const SourceGrammar = `
start: options_source | plain_source
options_source: OPTIONS_LINE NEWLINE SOURCE
plain_source: SOURCE

OPTIONS_LINE: /[ \t]*\/\/ @options:[^\r\n]*/
NEWLINE: /\r?\n/
SOURCE: /[\s\S]+/
`

const (
	supportedFieldsText = "`max_output_tokens` and `timeout_ms`"
	// maxTimeoutMs is the largest delay setTimeout supports, which bounds
	// timeout_ms.
	maxTimeoutMs = 2_147_483_647
	maxSafeInt   = 1<<53 - 1
)

// SourceOptions are the fields of the options line; nil when absent.
type SourceOptions struct {
	// MaxOutputTokens is the token budget for the script's output.
	MaxOutputTokens *int64
	// TimeoutMs is the hard deadline for the whole script, tool calls
	// included.
	TimeoutMs *int64
}

// ParsedSource is a script with its options.
type ParsedSource struct {
	// Code is the script with the options line removed up to its newline, so
	// line numbers are unchanged.
	Code    string
	Options SourceOptions
}

// SourceError is a malformed codemode source.
type SourceError struct{ Message string }

func (e *SourceError) Error() string { return e.Message }

func sourceErr(format string, args ...any) error {
	return &SourceError{Message: fmt.Sprintf(format, args...)}
}

// safeInteger reads v as a non-negative safe integer (Number.isSafeInteger and
// >= 0).
func safeInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f := jstext.Number(n)
	if f < 0 || f > maxSafeInt || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// objectKeys lists a JSON object's keys as Object.keys does: array-index keys
// first, ascending, then the rest in document order.
func objectKeys(text string) []string {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		ia, aIndex := jstext.ArrayIndexKey(a)
		ib, bIndex := jstext.ArrayIndexKey(b)
		switch {
		case aIndex && bIndex:
			return cmp.Compare(ia, ib)
		case aIndex:
			return -1
		case bIndex:
			return 1
		}
		return 0
	})
	return keys
}

func parseOptions(directive string) (SourceOptions, error) {
	var options SourceOptions
	if directive == "" {
		return options, sourceErr("@options must be a JSON object with supported fields %s", supportedFieldsText)
	}
	if err := jstext.JSONSyntaxError(directive); err != nil {
		return options, sourceErr("@options must be valid JSON with supported fields %s: %s", supportedFieldsText, err.Error())
	}
	value, err := jstext.Parse([]byte(directive))
	if err != nil {
		return options, sourceErr("@options must be valid JSON with supported fields %s: %s", supportedFieldsText, err.Error())
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return options, sourceErr("@options must be a JSON object with supported fields %s", supportedFieldsText)
	}
	for _, key := range objectKeys(directive) {
		if key != "max_output_tokens" && key != "timeout_ms" {
			return options, sourceErr("@options only supports %s; got `%s`", supportedFieldsText, key)
		}
	}
	if v, ok := fields["max_output_tokens"]; ok {
		n, ok := safeInteger(v)
		if !ok {
			return options, sourceErr("@options field `max_output_tokens` must be a non-negative safe integer")
		}
		options.MaxOutputTokens = &n
	}
	if v, ok := fields["timeout_ms"]; ok {
		n, ok := safeInteger(v)
		if !ok || n == 0 || n > maxTimeoutMs {
			return options, sourceErr("@options field `timeout_ms` must be a positive integer up to %d", maxTimeoutMs)
		}
		options.TimeoutMs = &n
	}
	return options, nil
}

// ParseSource splits an optional first-line `// @options: {...}` from the
// script. It fails with a *SourceError for empty input, invalid options, and
// an options line that is not the first line, which would otherwise be
// ignored without notice.
func ParseSource(input string) (ParsedSource, error) {
	if jstext.Trim(input) == "" {
		return ParsedSource{}, sourceErr("Expected JavaScript source text (non-empty). Provide JS only, optionally with a first line `// @options: {\"max_output_tokens\": 1000}`.")
	}
	lines := strings.Split(input, "\n")
	for i, line := range lines[1:] {
		if strings.HasPrefix(jstext.TrimStart(line), OptionsPrefix) {
			return ParsedSource{}, sourceErr("The %s line must be the first line of the input; found one on line %d", OptionsPrefix, i+2)
		}
	}
	trimmed := jstext.TrimStart(strings.TrimSuffix(lines[0], "\r"))
	if !strings.HasPrefix(trimmed, OptionsPrefix) {
		return ParsedSource{Code: input}, nil
	}
	code := ""
	if newline := strings.IndexByte(input, '\n'); newline != -1 {
		code = input[newline:]
	}
	if jstext.Trim(code) == "" {
		return ParsedSource{}, sourceErr("The @options line must be followed by JavaScript source on subsequent lines")
	}
	options, err := parseOptions(jstext.Trim(trimmed[len(OptionsPrefix):]))
	if err != nil {
		return ParsedSource{}, err
	}
	return ParsedSource{Code: code, Options: options}, nil
}
