package ai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Ported from pi packages/ai/test/streaming-json.test.ts (upstream f715a15c).
// Pieces are cut by characters, not UTF-16 units: a Go string cannot hold half
// a surrogate pair, so pi's raw-pair cases have no Go form. Adapted: the
// expected value of a complete document is DecodeOrderedValue's (JSON.parse
// here), with numbers that are not finite as nil and lone surrogates as
// U+FFFD, as the engine holds both; the comparison with parseStreamingJson is
// in ai/providers, which owns it; the memory cases measure allocated bytes
// with runtime.MemStats, and the case that drops the stream to the collector
// has no Go form (a Go value is collected when unreachable, with nothing to
// hold it). Added: an escaped high surrogate whose pair does not follow.

// lcg is pi's seeded random numbers, so every run checks the same documents.
func lcg(seed int) func() float64 {
	state := seed
	return func() float64 {
		state = (state*1103515245 + 12345) & 0x7fffffff
		return float64(state) / 0x7fffffff
	}
}

func pickOf[T any](rand func() float64, items []T) T {
	return items[int(math.Floor(rand()*float64(len(items))))]
}

var stringPieces = []string{"a", "b", "x y", " ", "\n", "\t", "\\", `"`, "/", "é", "😀", "\u0001", "\u001f", "\u2028", "{", "]", ","}

func randomJSONString(rand func() float64) string {
	var b strings.Builder
	length := int(math.Floor(rand() * 6))
	for range length {
		b.WriteString(pickOf(rand, stringPieces))
	}
	return b.String()
}

func randomJSONScalar(rand func() float64) any {
	return pickOf(rand, []func() any{
		func() any { return randomJSONString(rand) },
		func() any { return math.Floor(rand()*2000) - 1000 },
		func() any { return rand() * 100 },
		func() any { return 1e21 },
		func() any { return -5e-8 },
		func() any { return math.Copysign(0, -1) },
		func() any { return 0.0 },
		func() any { return true },
		func() any { return false },
		func() any { return nil },
	})()
}

func randomJSONValue(rand func() float64, depth int) any {
	roll := rand()
	if depth > 3 || roll < 0.3 {
		return randomJSONScalar(rand)
	}
	if roll < 0.65 {
		var members OrderedObject
		count := int(math.Floor(rand() * 4))
		for range count {
			key := randomJSONString(rand)
			value := randomJSONValue(rand, depth+1)
			if i := members.indexOf(key); i >= 0 {
				members[i].Value = value
			} else {
				members = append(members, OrderedField{Key: key, Value: value})
			}
		}
		if members == nil {
			members = OrderedObject{}
		}
		return members
	}
	count := int(math.Floor(rand() * 4))
	elements := make([]any, count)
	for i := range elements {
		elements[i] = randomJSONValue(rand, depth+1)
	}
	return elements
}

func quoteJSON(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// renderJSON is the JSON text of value, with random insignificant whitespace
// when spaced.
func renderJSON(value any, rand func() float64, spaced bool) string {
	space := func() string {
		if !spaced {
			return ""
		}
		return pickOf(rand, []string{"", "", " ", "\n  ", "\t", "\r\n"})
	}
	switch v := value.(type) {
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = renderJSON(item, rand, spaced)
		}
		return "[" + space() + strings.Join(parts, space()+","+space()) + space() + "]"
	case OrderedObject:
		parts := make([]string, len(v))
		for i, f := range v {
			parts[i] = quoteJSON(f.Key) + space() + ":" + space() + renderJSON(f.Value, rand, spaced)
		}
		return "{" + space() + strings.Join(parts, space()+","+space()) + space() + "}"
	case string:
		return quoteJSON(v)
	case float64:
		if v == 0 && math.Signbit(v) {
			return "-0"
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return "null"
}

func randomJSONDocuments(seed, count int, spaced bool) []string {
	rand := lcg(seed)
	docs := make([]string, count)
	for i := range docs {
		var value any
		if rand() < 0.8 {
			value = OrderedObject{{Key: "path", Value: randomJSONString(rand)}, {Key: "args", Value: randomJSONValue(rand, 0)}}
		} else {
			value = randomJSONValue(rand, 0)
		}
		docs[i] = renderJSON(value, rand, spaced)
	}
	return docs
}

// The reference for StreamingJSONParser on a prefix of valid JSON (strings may
// also hold raw control characters and invalid escapes): a plain
// recursive-descent reading of the whole prefix that follows the documented
// rules, written independently of the incremental parser.

type refRead struct {
	value    any
	complete bool
	end      int
}

const refSpace = " \t\n\r"

func refSkipSpace(text string, start int) int {
	for start < len(text) && strings.IndexByte(refSpace, text[start]) >= 0 {
		start++
	}
	return start
}

func referenceSnapshot(text string, pendingScalars bool) (any, bool) {
	read, ok := refValue(text, refSkipSpace(text, 0), pendingScalars)
	if !ok {
		return nil, false
	}
	return read.value, true
}

func refValue(text string, start int, pending bool) (refRead, bool) {
	if start >= len(text) {
		return refRead{}, false
	}
	switch text[start] {
	case '"':
		return refString(text, start), true
	case '{':
		return refObject(text, start, pending), true
	case '[':
		return refArray(text, start, pending), true
	}
	return refScalar(text, start, pending)
}

func refString(text string, start int) refRead {
	var value strings.Builder
	var high rune
	flush := func() {
		if high != 0 {
			value.WriteRune(utf8.RuneError)
			high = 0
		}
	}
	index := start + 1
	for index < len(text) {
		c := text[index]
		if c == '"' {
			flush()
			return refRead{value.String(), true, index + 1}
		}
		if c != '\\' {
			flush()
			value.WriteByte(c)
			index++
			continue
		}
		if index+1 >= len(text) {
			return refRead{value.String(), false, len(text)}
		}
		next := text[index+1]
		if decoded, ok := jsonSimpleEscapes[next]; ok {
			flush()
			value.WriteString(decoded)
			index += 2
			continue
		}
		if next == 'u' {
			end := index + 2
			for end < index+6 && end < len(text) && isJSONHexDigit(text[end]) {
				end++
			}
			if end == index+6 {
				unit, _ := strconv.ParseUint(text[index+2:end], 16, 16)
				r := rune(unit)
				switch {
				case r >= 0xD800 && r < 0xDC00:
					flush()
					high = r
				case r >= 0xDC00 && r < 0xE000 && high != 0:
					value.WriteRune(0x10000 + (high-0xD800)<<10 + (r - 0xDC00))
					high = 0
				case r >= 0xDC00 && r < 0xE000:
					value.WriteRune(utf8.RuneError)
				default:
					flush()
					value.WriteRune(r)
				}
				index = end
				continue
			}
			if end == len(text) {
				// An escaped high surrogate waiting for its pair is held back.
				return refRead{value.String(), false, end}
			}
			flush()
			value.WriteString(text[index:end])
			index = end
			continue
		}
		flush()
		value.WriteByte('\\')
		index++
	}
	return refRead{value.String(), false, len(text)}
}

func refSet(members OrderedObject, key string, value any) OrderedObject {
	if i := members.indexOf(key); i >= 0 {
		members[i].Value = value
		return members
	}
	return append(members, OrderedField{Key: key, Value: value})
}

func refObject(text string, start int, pending bool) refRead {
	members := OrderedObject{}
	partial := func() refRead { return refRead{jsKeyOrder(members), false, len(text)} }
	index := refSkipSpace(text, start+1)
	if index >= len(text) {
		return partial()
	}
	if text[index] == '}' {
		return refRead{members, true, index + 1}
	}
	for {
		key := refString(text, index)
		if !key.complete {
			return partial()
		}
		index = refSkipSpace(text, key.end)
		if index >= len(text) {
			return partial()
		}
		index = refSkipSpace(text, index+1)
		member, ok := refValue(text, index, pending)
		if !ok {
			return partial()
		}
		members = refSet(members, key.value.(string), member.value)
		if !member.complete {
			return partial()
		}
		index = refSkipSpace(text, member.end)
		if index >= len(text) {
			return partial()
		}
		if text[index] == '}' {
			return refRead{jsKeyOrder(members), true, index + 1}
		}
		index = refSkipSpace(text, index+1)
		if index >= len(text) {
			return partial()
		}
	}
}

func refArray(text string, start int, pending bool) refRead {
	elements := []any{}
	index := refSkipSpace(text, start+1)
	if index >= len(text) {
		return refRead{elements, false, len(text)}
	}
	if text[index] == ']' {
		return refRead{elements, true, index + 1}
	}
	for {
		element, ok := refValue(text, index, pending)
		if !ok {
			return refRead{elements, false, len(text)}
		}
		elements = append(elements, element.value)
		if !element.complete {
			return refRead{elements, false, len(text)}
		}
		index = refSkipSpace(text, element.end)
		if index >= len(text) {
			return refRead{elements, false, len(text)}
		}
		if text[index] == ']' {
			return refRead{elements, true, index + 1}
		}
		index = refSkipSpace(text, index+1)
		if index >= len(text) {
			return refRead{elements, false, len(text)}
		}
	}
}

func refScalar(text string, start int, pending bool) (refRead, bool) {
	end := start
	for end < len(text) && !strings.ContainsRune(refSpace+`,:{}[]"`, rune(text[end])) {
		end++
	}
	token := text[start:end]
	if end < len(text) {
		value, _ := DecodeOrderedValue([]byte(token))
		return refRead{finite(value), true, end}, true
	}
	if !pending {
		return refRead{}, false
	}
	for _, literal := range jsonLiterals {
		if strings.HasPrefix(literal.word, token) {
			return refRead{literal.value, false, end}, true
		}
	}
	if jsonNumberPattern.MatchString(token) {
		return refRead{jsonNumber(token), false, end}, true
	}
	if mantissa := jsonNumberWithoutExponentDigits.FindStringSubmatch(token); mantissa != nil {
		return refRead{jsonNumber(mantissa[1]), false, end}, true
	}
	return refRead{}, false
}

// finite is v with every number that is not finite as nil.
func finite(v any) any {
	switch t := v.(type) {
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return nil
		}
	case OrderedObject:
		out := make(OrderedObject, len(t))
		for i, f := range t {
			out[i] = OrderedField{Key: f.Key, Value: finite(f.Value)}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = finite(e)
		}
		return out
	}
	return v
}

// jsonEqual compares two values as pi's toEqual does: -0 differs from 0, key
// order counts.
func jsonEqual(a, b any) bool {
	fa, aFloat := a.(float64)
	fb, bFloat := b.(float64)
	if aFloat && bFloat {
		return fa == fb && math.Signbit(fa) == math.Signbit(fb)
	}
	switch x := a.(type) {
	case OrderedObject:
		y, ok := b.(OrderedObject)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !jsonEqual(x[i].Value, y[i].Value) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// cloneSnapshot is a structuredClone of a snapshot.
func cloneSnapshot(v any) any {
	switch t := v.(type) {
	case OrderedObject:
		out := make(OrderedObject, len(t))
		for i, f := range t {
			out[i] = OrderedField{Key: f.Key, Value: cloneSnapshot(f.Value)}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneSnapshot(e)
		}
		return out
	}
	return v
}

type streamStep struct {
	read     string
	snapshot any
	ok       bool
	copy     any
}

// pieces cuts text into pieces of size characters, or of a random size of 1
// to 12 when size is 0.
func pieces(text string, size int, rand func() float64) []string {
	runes := []rune(text)
	var out []string
	for index := 0; index < len(runes); {
		n := size
		if n == 0 {
			n = 1 + int(math.Floor(rand()*12))
		}
		end := min(index+n, len(runes))
		out = append(out, string(runes[index:end]))
		index = end
	}
	return out
}

func streamJSON(text string, size int, rand func() float64) []streamStep {
	parser := NewStreamingJSONParser()
	var steps []streamStep
	read := ""
	for _, piece := range pieces(text, size, rand) {
		parser.Append(piece)
		read += piece
		snapshot, ok := parser.Snapshot()
		steps = append(steps, streamStep{read, snapshot, ok, cloneSnapshot(snapshot)})
	}
	return steps
}

func finalJSONSnapshot(text string, size int) (any, bool) {
	if size == 0 {
		size = max(utf8.RuneCountInString(text), 1)
	}
	steps := streamJSON(text, size, nil)
	if len(steps) == 0 {
		return nil, false
	}
	last := steps[len(steps)-1]
	return last.snapshot, last.ok
}

// chunkSizes are pi's: 1, 2, 3, 7 and random (0).
var chunkSizes = []int{1, 2, 3, 7, 0}

func read32(text string) int {
	hash := int32(7)
	for _, unit := range []rune(text) {
		hash = hash*31 + int32(unit)
	}
	if hash < 0 {
		hash = -hash
	}
	return int(hash) + 1
}

func randFor(text string, size int) func() float64 {
	if size != 0 {
		return nil
	}
	return lcg(read32(text))
}

func expectSnapshot(t *testing.T, label string, got any, gotOK bool, want any, wantOK bool) {
	t.Helper()
	if gotOK != wantOK || (wantOK && !jsonEqual(got, want)) {
		t.Fatalf("%s:\n got  %#v (%v)\n want %#v (%v)", label, got, gotOK, want, wantOK)
	}
}

func TestStreamingJSONMatchesTheReferenceAfterEveryPiece(t *testing.T) {
	for _, text := range randomJSONDocuments(1, 250, true) {
		for _, size := range chunkSizes {
			for _, step := range streamJSON(text, size, randFor(text, size)) {
				want, wantOK := referenceSnapshot(step.read, true)
				expectSnapshot(t, strconv.Quote(step.read)+" size "+strconv.Itoa(size), step.snapshot, step.ok, want, wantOK)
			}
		}
	}
}

func TestStreamingJSONReadsCompleteDocumentsLikeJSONParse(t *testing.T) {
	docs := append(randomJSONDocuments(3, 300, true), randomJSONDocuments(4, 300, false)...)
	for _, text := range docs {
		want, err := DecodeOrderedValue([]byte(text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for _, size := range chunkSizes {
			if size == 0 {
				size = 5
			}
			got, ok := finalJSONSnapshot(text, size)
			expectSnapshot(t, text, got, ok, finite(want), true)
		}
	}
}

func TestStreamingJSONNeverChangesASnapshotOnceReturned(t *testing.T) {
	for _, text := range randomJSONDocuments(5, 150, true) {
		for _, step := range streamJSON(text, 0, lcg(read32(text))) {
			if !jsonEqual(step.snapshot, step.copy) {
				t.Fatalf("%q: snapshot changed to %#v", step.read, step.snapshot)
			}
		}
	}
}

func TestStreamingJSONRepairsControlCharactersAndInvalidEscapes(t *testing.T) {
	rand := lcg(6)
	malformed := []string{"\n", "\t", "\u0000", `\d`, `\x41`, `\u12`, `\uZZZZ`, `\u00e9`, `\\`, `\q`}
	for round := range 300 {
		var content strings.Builder
		for range 6 {
			content.WriteString(pickOf(rand, append(slices.Clone(malformed), "a", " ", "é")))
		}
		text := `{"path":"p","content":"` + content.String() + `","n":` + strconv.Itoa(round) + `}`
		for _, step := range streamJSON(text, 1, nil) {
			want, wantOK := referenceSnapshot(step.read, true)
			expectSnapshot(t, strconv.Quote(step.read), step.snapshot, step.ok, want, wantOK)
		}
	}
}

func TestStreamingJSONHoldsAnEscapeBackUntilComplete(t *testing.T) {
	for _, tc := range []struct{ text, value string }{
		{`{"a":"x\u00e9y"}`, "xéy"},
		{`{"a":"x\ny"}`, "x\ny"},
		{`{"a":"x\\y"}`, `x\y`},
		{`{"a":"x\"y"}`, `x"y`},
		{`{"a":"x\/y"}`, "x/y"},
	} {
		for split := 0; split <= len(tc.text); split++ {
			parser := NewStreamingJSONParser()
			parser.Append(tc.text[:split])
			got, ok := parser.Snapshot()
			want, wantOK := referenceSnapshot(tc.text[:split], true)
			expectSnapshot(t, tc.text[:split], got, ok, want, wantOK)
			parser.Append(tc.text[split:])
			got, ok = parser.Snapshot()
			expectSnapshot(t, tc.text, got, ok, OrderedObject{{Key: "a", Value: tc.value}}, true)
		}
	}
}

func TestStreamingJSONJoinsEscapedSurrogatePairsSplitAcrossPieces(t *testing.T) {
	text := `{"a":"\ud83d\ude00!"}`
	for split := 0; split <= len(text); split++ {
		parser := NewStreamingJSONParser()
		parser.Append(text[:split])
		parser.Snapshot()
		parser.Append(text[split:])
		got, ok := parser.Snapshot()
		expectSnapshot(t, text[:split], got, ok, OrderedObject{{Key: "a", Value: "😀!"}}, true)
	}
	// Go: a high surrogate whose pair has not arrived is held back, and one
	// that finds no pair becomes U+FFFD, as DecodeOrderedValue reads it.
	parser := NewStreamingJSONParser()
	parser.Append(`{"a":"\ud83d`)
	got, ok := parser.Snapshot()
	expectSnapshot(t, "held", got, ok, OrderedObject{{Key: "a", Value: ""}}, true)
	parser.Append(`x\ude00"}`)
	got, ok = parser.Snapshot()
	expectSnapshot(t, "unpaired", got, ok, OrderedObject{{Key: "a", Value: "\uFFFDx\uFFFD"}}, true)
}

func TestStreamingJSONKeepsTrailingWhitespaceOfAnUnterminatedString(t *testing.T) {
	parser := NewStreamingJSONParser()
	parser.Append("{\"code\":\"x\t\t  ")
	got, ok := parser.Snapshot()
	expectSnapshot(t, "", got, ok, OrderedObject{{Key: "code", Value: "x\t\t  "}}, true)
}

func TestStreamingJSONKeepsControlCharactersAndInvalidEscapesVisible(t *testing.T) {
	parser := NewStreamingJSONParser()
	parser.Append("{\"a\":\"x\ny \\d \\u12\"")
	got, ok := parser.Snapshot()
	expectSnapshot(t, "", got, ok, OrderedObject{{Key: "a", Value: "x\ny \\d \\u12"}}, true)
}

func TestStreamingJSONKeepsInvalidUnicodeEscapesLiteral(t *testing.T) {
	got, ok := finalJSONSnapshot(`{"path":"a.tex","content":"\usepackage{x}","mode":"w"}`, 1)
	expectSnapshot(t, "usepackage", got, ok, OrderedObject{{Key: "path", Value: "a.tex"}, {Key: "content", Value: `\usepackage{x}`}, {Key: "mode", Value: "w"}}, true)
	got, ok = finalJSONSnapshot(`{"text":"caf\u00e9 \url"}`, 3)
	expectSnapshot(t, "url", got, ok, OrderedObject{{Key: "text", Value: `café \url`}}, true)
	parser := NewStreamingJSONParser()
	parser.Append(`{"text":"ab\u00`)
	got, ok = parser.Snapshot()
	expectSnapshot(t, "held", got, ok, OrderedObject{{Key: "text", Value: "ab"}}, true)
}

func TestStreamingJSONKeepsTheLastValueOfADuplicateKeyInTheFirstPosition(t *testing.T) {
	text := `{"a":1,"b":2,"a":"three"}`
	want, _ := DecodeOrderedValue([]byte(text))
	got, ok := finalJSONSnapshot(text, 1)
	expectSnapshot(t, text, got, ok, want, true)
}

// Added: array-index keys come first, as a JS object lists them, while the
// object is open and once it closes.
func TestStreamingJSONListsArrayIndexKeysFirst(t *testing.T) {
	parser := NewStreamingJSONParser()
	parser.Append(`{"b":1,"2":2,"1":`)
	got, ok := parser.Snapshot()
	expectSnapshot(t, "open", got, ok, OrderedObject{{Key: "2", Value: 2.0}, {Key: "b", Value: 1.0}}, true)
	parser.Append(`1}`)
	got, ok = parser.Snapshot()
	expectSnapshot(t, "closed", got, ok, OrderedObject{{Key: "1", Value: 1.0}, {Key: "2", Value: 2.0}, {Key: "b", Value: 1.0}}, true)
}

type vector struct {
	input string
	value any
	ok    bool
}

func obj(fields ...any) OrderedObject {
	o := OrderedObject{}
	for i := 0; i < len(fields); i += 2 {
		o = append(o, OrderedField{Key: fields[i].(string), Value: fields[i+1]})
	}
	return o
}

func arr(items ...any) []any {
	if items == nil {
		return []any{}
	}
	return items
}

// partial-json (MIT, promplate/partial-json-parser-js tests/examples.test.js)
// and Vercel AI SDK fixJson (Apache-2.0, vercel/ai fix-json.test.ts), as pi
// ports them.
var streamingJSONVectors = []vector{
	{`"`, "", true},
	{`["`, arr(""), true},
	{`{"": "`, obj("", ""), true},
	{"n", nil, true},
	{"t", true, true},
	{"f", false, true},
	{"0", 0.0, true},
	{"-1.25e+4", -1.25e4, true},
	{"-1.25e+", -1.25, true},
	{"-1.25e", -1.25, true},
	{`" \x12`, ` \x12`, true},
	{"I", nil, false},
	{"-I", nil, false},
	{"N", nil, false},
	{"", nil, false},
	{"nul", nil, true},
	{"fals", false, true},
	{"12.2", 12.2, true},
	{"-12", -12.0, true},
	{"-", nil, false},
	{"2.5e", 2.5, true},
	{"2.5e-", 2.5, true},
	{"2.5e3", 2.5e3, true},
	{"-2.5e3", -2.5e3, true},
	{"2.5E", 2.5, true},
	{"2.5E-", 2.5, true},
	{"2.5E3", 2.5e3, true},
	{"-2.5E3", -2.5e3, true},
	{"12.34e", 12.34, true},
	{"5e", 5.0, true},
	{`"abc`, "abc", true},
	{`"value with \"quoted\" text and \\ escape`, `value with "quoted" text and \ escape`, true},
	{`"value with \`, "value with ", true},
	{`"\u`, "", true},
	{`"\u12`, "", true},
	{`"text \u00`, "text ", true},
	{`{"a":"\u12`, obj("a", ""), true},
	{`"value with unicode <"`, "value with unicode <", true},
	{"[", arr(), true},
	{"[[1], [2", arr(arr(1.0), arr(2.0)), true},
	{`[["1"], ["2`, arr(arr("1"), arr("2")), true},
	{"[[false], [nu", arr(arr(false), arr(nil)), true},
	{"[[[]], [[]", arr(arr(arr()), arr(arr())), true},
	{"[[{}], [{", arr(arr(obj()), arr(obj())), true},
	{"[1, ", arr(1.0), true},
	{"[[], 123", arr(arr(), 123.0), true},
	{`{"key":`, obj(), true},
	{`{"a": {"b": 1}, "c": {"d": 2`, obj("a", obj("b", 1.0), "c", obj("d", 2.0)), true},
	{`{"a": {"b": "1"}, "c": {"d": 2`, obj("a", obj("b", "1"), "c", obj("d", 2.0)), true},
	{`{"a": {"b": false}, "c": {"d": 2`, obj("a", obj("b", false), "c", obj("d", 2.0)), true},
	{`{"a": {"b": []}, "c": {"d": 2`, obj("a", obj("b", arr()), "c", obj("d", 2.0)), true},
	{`{"a": {"b": {}}, "c": {"d": 2`, obj("a", obj("b", obj()), "c", obj("d", 2.0)), true},
	{`{"ke`, obj(), true},
	{`{"k1": 1, "k2`, obj("k1", 1.0), true},
	{`{"k1": 1, "k2":`, obj("k1", 1.0), true},
	{`{"key": "value"  `, obj("key", "value"), true},
	{`{"a": {"b": {}`, obj("a", obj("b", obj())), true},
	{"[1, [2, 3, [", arr(1.0, arr(2.0, 3.0, arr())), true},
	{"[false, [true, [", arr(false, arr(true, arr())), true},
	{`{"key": {"subKey":`, obj("key", obj()), true},
	{`{"key": 123, "key2": {"subKey":`, obj("key", 123.0, "key2", obj()), true},
	{`{"key": null, "key2": {"subKey":`, obj("key", nil, "key2", obj()), true},
	{`{"key": [1, 2, {`, obj("key", arr(1.0, 2.0, obj())), true},
	{`[1, 2, {"key": "value",`, arr(1.0, 2.0, obj("key", "value")), true},
	{`{"a": {"b": ["c", {"d": "e",`, obj("a", obj("b", arr("c", obj("d", "e")))), true},
	{`{"a": {"b": {"c": {"d":`, obj("a", obj("b", obj("c", obj()))), true},
	{`{"a": 1, "b": [`, obj("a", 1.0, "b", arr()), true},
	{`{"a": 1, "b": {`, obj("a", 1.0, "b", obj()), true},
	{`{"a": 1, "b": "`, obj("a", 1.0, "b", ""), true},
	{"{\n  \"a\": [\n    {\n      \"a1\": \"v1\",\n      \"a2\": \"v2\",\n      \"a3\": \"v3\"\n    }\n  ],\n  \"b\": [\n    {\n      \"b1\": \"n",
		obj("a", arr(obj("a1", "v1", "a2", "v2", "a3", "v3")), "b", arr(obj("b1", "n"))), true},
	{`{"type":"div","children":[{"type":"Card","props":{}`, obj("type", "div", "children", arr(obj("type", "Card", "props", obj()))), true},
	{"12.", nil, false},
	{"12.e", nil, false},
}

func TestStreamingJSONReadsThePortedVectors(t *testing.T) {
	for _, v := range streamingJSONVectors {
		for _, size := range []int{0, 1, 2} {
			got, ok := finalJSONSnapshot(v.input, size)
			expectSnapshot(t, strconv.Quote(v.input)+" size "+strconv.Itoa(size), got, ok, v.value, v.ok)
		}
	}
}

// expectPlainJSON asserts value is plain, finite JSON data.
func expectPlainJSON(t *testing.T, value any, label string) {
	t.Helper()
	switch v := value.(type) {
	case nil, string, bool:
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			t.Fatalf("%s: number %v is not finite", label, v)
		}
	case OrderedObject:
		for _, f := range v {
			expectPlainJSON(t, f.Value, label)
		}
	case []any:
		for _, e := range v {
			expectPlainJSON(t, e, label)
		}
	default:
		t.Fatalf("%s: %T is not plain JSON", label, value)
	}
}

func TestStreamingJSONReadsTheJSONTestSuite(t *testing.T) {
	data, err := os.ReadFile("testdata/json-test-suite.json")
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for name, encoded := range suite.Files {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		// Buffer.toString("utf8"): invalid bytes become U+FFFD.
		text := strings.ToValidUTF8(string(raw), "\uFFFD")
		expected, decodeErr := DecodeOrderedValue([]byte(text))
		for _, size := range []int{0, 1, 3} {
			got, ok := finalJSONSnapshot(text, size)
			if decodeErr == nil {
				expectSnapshot(t, name, got, ok, finite(expected), true)
			} else if ok {
				expectPlainJSON(t, got, name)
			}
		}
		if decodeErr == nil {
			accepted++
		}
	}
	if accepted < 90 {
		t.Fatalf("only %d documents accepted; the suite did not load as expected", accepted)
	}
}

var malformedJSON = []string{
	`{"a":1 "b":2}`,
	`{"a" 1, "b": 2}`,
	`{a:1, 'b': 2}`,
	`{"a":1,,"b":2,}`,
	"[1 2, , 3,]",
	`{"a":[1,2}, "b":3]`,
	"]]]}}}{",
	`{"a":NaN,"b":Infinity,"c":-Infinity,"d":undefined,"e":1}`,
	`{"a":01,"b":1.,"c":.5,"d":+1,"e":0x10,"f":1e}`,
	`{"a":"x"} trailing garbage {"b":2}`,
	`{"a": tru, "b": nulll, "c": falsey}`,
	`{"a":{"b":{"c":`,
	`{"a\": 1}`,
	`"unterminated \`,
	"// comment\n{\"a\":1}",
	"\u0000{\u0001\"a\"\u0002:\u00031}",
	`{"a":"x"}}}]]]`,
}

func TestStreamingJSONNeverFailsOnMalformedText(t *testing.T) {
	for _, text := range malformedJSON {
		want, wantOK := finalJSONSnapshot(text, 0)
		for _, size := range chunkSizes {
			steps := streamJSON(text, size, randFor(text, size))
			for _, step := range steps {
				if step.ok {
					expectPlainJSON(t, step.snapshot, text)
				}
				if !jsonEqual(step.snapshot, step.copy) {
					t.Fatalf("%q: snapshot changed", text)
				}
			}
			last := steps[len(steps)-1]
			expectSnapshot(t, text+" size "+strconv.Itoa(size), last.snapshot, last.ok, want, wantOK)
		}
	}
}

func TestStreamingJSONToleratesMissingCommasAndColons(t *testing.T) {
	for _, tc := range []struct {
		text string
		want any
	}{
		{`{"a":1 "b":2}`, obj("a", 1.0, "b", 2.0)},
		{`{"a" 1, "b": 2}`, obj("a", 1.0, "b", 2.0)},
		{"[1 2, , 3,]", arr(1.0, 2.0, 3.0)},
		{`{"a":NaN,"b":Infinity,"e":1}`, obj("e", 1.0)},
		{`{"a":"x"} trailing garbage {"b":2}`, obj("a", "x")},
	} {
		got, ok := finalJSONSnapshot(tc.text, 0)
		expectSnapshot(t, tc.text, got, ok, tc.want, true)
	}
}

// mirror is a copy of a value kept current by applying the parser's changes,
// as a writer such as Durable keeps one.
type mirror struct {
	value any
	ok    bool
}

func (m *mirror) apply(changes []JSONChange) {
	for _, change := range changes {
		if len(change.Path) == 0 {
			if change.Kind == JSONChangeSet {
				m.value, m.ok = cloneSnapshot(change.Value), true
			} else {
				m.value = m.value.(string) + change.Text
			}
			continue
		}
		m.value = mirrorSet(m.value, change.Path, change)
	}
}

func mirrorSet(container any, path []any, change JSONChange) any {
	key := path[0]
	get := func() any {
		switch c := container.(type) {
		case OrderedObject:
			v, _ := c.Get(key.(string))
			return v
		case []any:
			return c[key.(int)]
		}
		return nil
	}
	var value any
	switch {
	case len(path) > 1:
		value = mirrorSet(get(), path[1:], change)
	case change.Kind == JSONChangeSet:
		value = cloneSnapshot(change.Value)
	default:
		value = get().(string) + change.Text
	}
	switch c := container.(type) {
	case OrderedObject:
		k := key.(string)
		if i := c.indexOf(k); i >= 0 {
			c[i].Value = value
			return c
		}
		return jsKeyOrder(append(c, OrderedField{Key: k, Value: value}))
	case []any:
		i := key.(int)
		if i == len(c) {
			return append(c, value)
		}
		c[i] = value
		return c
	}
	return container
}

func TestStreamingJSONSurvivesRandomCorruption(t *testing.T) {
	rand := lcg(7)
	noise := []string{`"`, `\`, "{", "}", "[", "]", ",", ":", "x", "1", "-", "\n", "t"}
	for _, text := range randomJSONDocuments(8, 300, true) {
		var characters []string
		for _, r := range text {
			characters = append(characters, string(r))
		}
		for range 3 {
			at := int(math.Floor(rand() * float64(len(characters)+1)))
			if rand() < 0.5 {
				if at < len(characters) {
					characters = slices.Delete(characters, at, at+1)
				}
			} else {
				characters = slices.Insert(characters, at, pickOf(rand, noise))
			}
		}
		corrupted := strings.Join(characters, "")
		for _, step := range streamJSON(corrupted, 0, lcg(read32(corrupted))) {
			if step.ok {
				expectPlainJSON(t, step.snapshot, corrupted)
			}
		}
		parser := NewStreamingJSONParser()
		m := &mirror{}
		for _, piece := range pieces(corrupted, 4, nil) {
			m.apply(parser.Append(piece))
		}
		// A space ends a number or literal at the end, which snapshots show
		// and changes do not report yet.
		m.apply(parser.Append(" "))
		got, ok := parser.Snapshot()
		expectSnapshot(t, corrupted, m.value, m.ok, got, ok)
	}
}

func TestStreamingJSONChangesKeepACopyEqualToTheValueReadSoFar(t *testing.T) {
	for _, text := range randomJSONDocuments(9, 200, true) {
		for _, size := range chunkSizes {
			parser := NewStreamingJSONParser()
			m := &mirror{}
			read := ""
			for _, piece := range pieces(text, size, randFor(text, size)) {
				m.apply(parser.Append(piece))
				read += piece
				want, wantOK := referenceSnapshot(read, false)
				expectSnapshot(t, strconv.Quote(read), m.value, m.ok, want, wantOK)
			}
			// Adapted: a space ends a number or literal at the end of the
			// document, which changes do not report until it ends; pi's
			// documents happen to end in no such root.
			m.apply(parser.Append(" "))
			want, _ := DecodeOrderedValue([]byte(text))
			expectSnapshot(t, text, m.value, m.ok, finite(want), true)
		}
	}
}

func changesOf(changes []JSONChange) []JSONChange {
	if changes == nil {
		return []JSONChange{}
	}
	return changes
}

func expectChanges(t *testing.T, got []JSONChange, want ...JSONChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i].Kind != want[i].Kind || !reflect.DeepEqual(got[i].Path, want[i].Path) || got[i].Text != want[i].Text || !jsonEqual(got[i].Value, want[i].Value) {
			t.Fatalf("change %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func set(value any, path ...any) JSONChange {
	if path == nil {
		path = []any{}
	}
	return JSONChange{Kind: JSONChangeSet, Path: path, Value: value}
}

func appended(text string, path ...any) JSONChange {
	return JSONChange{Kind: JSONChangeAppend, Path: path, Text: text}
}

func TestStreamingJSONReportsANumberThatEndsAndAStringThatStartsInOrder(t *testing.T) {
	parser := NewStreamingJSONParser()
	expectChanges(t, parser.Append(`{"n":1`), set(obj()))
	expectChanges(t, parser.Append(`2,"s":"ab`), set(12.0, "n"), set("ab", "s"))
	expectChanges(t, parser.Append(`c","t":tru`), appended("c", "s"))
	expectChanges(t, parser.Append("e}"), set(true, "t"))
	expectChanges(t, changesOf(parser.Append(` {"ignored":1}`)))
}

func TestStreamingJSONReportsNestedContainersEmptyThenTheirItems(t *testing.T) {
	parser := NewStreamingJSONParser()
	expectChanges(t, parser.Append(`{"a":[1,{"b":["x"`),
		set(obj()), set(arr(), "a"), set(1.0, "a", 0), set(obj(), "a", 1), set(arr(), "a", 1, "b"), set("x", "a", 1, "b", 0))
	expectChanges(t, parser.Append(`,"y`), set("y", "a", 1, "b", 1))
	expectChanges(t, parser.Append(`z"]}],"a":"again"}`), appended("z", "a", 1, "b", 1), set("again", "a"))
}

func TestStreamingJSONReportsSplitEscapesOnceComplete(t *testing.T) {
	text := `{"a":"x\u00e9\n\\\"\ud83d\ude00\q"}`
	expected := "xé\n\\\"😀\\q"
	for first := 1; first < len(text); first++ {
		for second := first; second < len(text); second++ {
			parser := NewStreamingJSONParser()
			m := &mirror{}
			for _, piece := range []string{text[:first], text[first:second], text[second:]} {
				m.apply(parser.Append(piece))
			}
			expectSnapshot(t, strconv.Itoa(first)+"/"+strconv.Itoa(second), m.value, m.ok, obj("a", expected), true)
		}
	}
	parser := NewStreamingJSONParser()
	parser.Append(`{"a":"x`)
	expectChanges(t, changesOf(parser.Append(`\u00`)))
	expectChanges(t, parser.Append("e9"), appended("é", "a"))
}

func TestStreamingJSONReportsAContainerDeeperThan32LevelsWholeWhenItCloses(t *testing.T) {
	depth := 40
	text := strings.Repeat(`{"a":`, depth) + `"deep"` + strings.Repeat("}", depth)
	want, _ := DecodeOrderedValue([]byte(text))
	parser := NewStreamingJSONParser()
	m := &mirror{}
	for _, piece := range pieces(text, 3, nil) {
		m.apply(parser.Append(piece))
	}
	expectSnapshot(t, "mirror", m.value, m.ok, want, true)
	changes := NewStreamingJSONParser().Append(text)
	if len(changes) != 34 {
		t.Fatalf("changes = %d, want 34", len(changes))
	}
	last := changes[len(changes)-1]
	inner, _ := DecodeOrderedValue([]byte(text[32*5 : len(text)-32]))
	if last.Kind != JSONChangeSet || len(last.Path) != 32 || !jsonEqual(last.Value, inner) {
		t.Fatalf("last change = %#v", last)
	}
}

func TestStreamingJSONReportsNothingForInvalidPositionsAndTextAfterTheRoot(t *testing.T) {
	parser := NewStreamingJSONParser()
	expectChanges(t, parser.Append(`{"a":1 2, "b" "x"}`), set(obj()), set(1.0, "a"), set("x", "b"))
	expectChanges(t, changesOf(parser.Append(`{"c":3}`)))
}

// allocatedBytes is the memory fn allocates.
func allocatedBytes(fn func()) float64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc - before.TotalAlloc)
}

var costContent = func() string {
	var b strings.Builder
	for line := range 20_000 {
		b.WriteString(`const line` + strconv.Itoa(line) + ` = "value ` + strconv.Itoa(line) + `"; // \ done` + "\n")
	}
	return b.String()
}()

// writeArguments are write-style arguments with size characters of content.
func writeArguments(size int) string {
	data, _ := json.Marshal(OrderedObject{{Key: "path", Value: "/src/file.ts"}, {Key: "content", Value: costContent[:size]}})
	return string(data)
}

func streamAllocation(text string, delta int) float64 {
	return allocatedBytes(func() {
		parser := NewStreamingJSONParser()
		for index := 0; index < len(text); index += delta {
			parser.Append(text[index:min(index+delta, len(text))])
			parser.Snapshot()
		}
	})
}

func TestStreamingJSONAllocatesLinearlyInTheStreamedText(t *testing.T) {
	small, large := writeArguments(50_000), writeArguments(500_000)
	smallBytes, largeBytes := streamAllocation(small, 32), streamAllocation(large, 32)
	if perChar := largeBytes / float64(len(large)); perChar > 200 {
		t.Fatalf("%.0f bytes per character", perChar)
	}
	if ratio := largeBytes / smallBytes; ratio > 20 {
		t.Fatalf("ten times the text allocates %.1f times as much", ratio)
	}
}

func TestStreamingJSONKeepsVeryLargeOpenArraysLinearByLettingThePreviewLag(t *testing.T) {
	elements := func(count int) string {
		items := make([]string, count)
		for i := range items {
			items[i] = strconv.Itoa(i)
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`
	}
	small, large := elements(10_000), elements(100_000)
	smallBytes, largeBytes := streamAllocation(small, 16), streamAllocation(large, 16)
	if perChar := largeBytes / float64(len(large)); perChar > 2000 {
		t.Fatalf("%.0f bytes per character", perChar)
	}
	if ratio := largeBytes / smallBytes; ratio > 20 {
		t.Fatalf("ten times the text allocates %.1f times as much", ratio)
	}
	parser := NewStreamingJSONParser()
	shown := 0
	for index := 0; index < len(large); index += 16 {
		parser.Append(large[index:min(index+16, len(large))])
		snapshot, _ := parser.Snapshot()
		var items []any
		if o, ok := snapshot.(OrderedObject); ok {
			if v, ok := o.Get("items"); ok {
				items = v.([]any)
			}
		}
		if len(items) < shown {
			t.Fatalf("the preview went back from %d to %d items", shown, len(items))
		}
		shown = len(items)
	}
	want, _ := DecodeOrderedValue([]byte(large))
	got, ok := parser.Snapshot()
	expectSnapshot(t, "large", got, ok, want, true)
}

func TestStreamingJSONReadsDeepNestingWithoutRecursion(t *testing.T) {
	depth := 200_000
	text := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
	parser := NewStreamingJSONParser()
	for index := 0; index < len(text); index += 7 {
		parser.Append(text[index:min(index+7, len(text))])
		parser.Snapshot()
	}
	value, _ := parser.Snapshot()
	for range depth {
		value = value.([]any)[0]
	}
	if value != 1.0 {
		t.Fatalf("innermost value = %v", value)
	}
}

func TestStreamingJSONReadsOneHugePieceLinearly(t *testing.T) {
	text := writeArguments(500_000)
	bytes := allocatedBytes(func() {
		parser := NewStreamingJSONParser()
		parser.Append(text)
		parser.Snapshot()
	})
	if perChar := bytes / float64(len(text)); perChar > 100 {
		t.Fatalf("%.0f bytes per character", perChar)
	}
}

// TestStreamingJSONMatchesPi replays testdata/streaming-json, captured from
// pi's own StreamingJsonParser (capture.mts), and compares every change and
// every recorded snapshot. Values are compared as JSON text decoded again, as
// pi's were recorded: JSON.stringify writes -0 as 0 and Infinity as null.
func TestStreamingJSONMatchesPi(t *testing.T) {
	data, err := os.ReadFile("testdata/streaming-json/capture-f715a15c.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Lag struct {
			Text  string `json:"text"`
			Piece int    `json:"piece"`
			Items []int  `json:"items"`
		} `json:"lag"`
		Cases []struct {
			Text  string `json:"text"`
			Size  int    `json:"size"`
			Steps []struct {
				Piece    string `json:"piece"`
				Has      *bool  `json:"has"`
				Snapshot string `json:"snapshot"`
				Changes  []struct {
					Kind  string `json:"kind"`
					Path  []any  `json:"path"`
					Value string `json:"value"`
					Text  string `json:"text"`
				} `json:"changes"`
			} `json:"steps"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	asJSON := func(v any) any {
		var b bytes.Buffer
		if err := marshalOrderedValue(&b, v); err != nil {
			t.Fatal(err)
		}
		back, err := DecodeOrderedValue([]byte(b.String()))
		if err != nil {
			t.Fatalf("%s: %v", b.String(), err)
		}
		return back
	}
	decode := func(text string) any {
		v, err := DecodeOrderedValue([]byte(text))
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		return finite(v)
	}
	// The copy credit: how many items of a large open array each snapshot
	// shows, piece by piece, is pi's.
	lag := NewStreamingJSONParser()
	for i, index := 0, 0; index < len(capture.Lag.Text); i, index = i+1, index+capture.Lag.Piece {
		lag.Append(capture.Lag.Text[index:min(index+capture.Lag.Piece, len(capture.Lag.Text))])
		snapshot, _ := lag.Snapshot()
		shown := 0
		if o, ok := snapshot.(OrderedObject); ok {
			if items, ok := o.Get("items"); ok {
				shown = len(items.([]any))
			}
		}
		if shown != capture.Lag.Items[i] {
			t.Fatalf("lag step %d: %d items shown, pi %d", i, shown, capture.Lag.Items[i])
		}
	}
	for _, c := range capture.Cases {
		label := strconv.Quote(c.Text) + " size " + strconv.Itoa(c.Size)
		parser := NewStreamingJSONParser()
		for i, step := range c.Steps {
			changes := parser.Append(step.Piece)
			if len(changes) != len(step.Changes) {
				t.Fatalf("%s step %d: changes %#v, pi %#v", label, i, changes, step.Changes)
			}
			for j, want := range step.Changes {
				got := changes[j]
				path := make([]any, len(want.Path))
				for k, segment := range want.Path {
					if n, isIndex := segment.(float64); isIndex {
						path[k] = int(n)
					} else {
						path[k] = segment
					}
				}
				if got.Kind != want.Kind || !reflect.DeepEqual(got.Path, path) {
					t.Fatalf("%s step %d change %d: %#v, pi %#v", label, i, j, got, want)
				}
				if want.Kind == JSONChangeSet && !jsonEqual(asJSON(got.Value), decode(want.Value)) {
					t.Fatalf("%s step %d change %d: value %#v, pi %s", label, i, j, got.Value, want.Value)
				}
				if want.Kind == JSONChangeAppend && got.Text != want.Text {
					t.Fatalf("%s step %d change %d: text %q, pi %q", label, i, j, got.Text, want.Text)
				}
			}
			snapshot, ok := parser.Snapshot()
			if step.Has == nil {
				continue
			}
			if ok != *step.Has || (ok && !jsonEqual(asJSON(snapshot), decode(step.Snapshot))) {
				t.Fatalf("%s step %d: snapshot %#v (%v), pi %s (%v)", label, i, snapshot, ok, step.Snapshot, *step.Has)
			}
		}
	}
}
