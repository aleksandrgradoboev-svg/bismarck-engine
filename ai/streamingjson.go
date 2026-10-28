package ai

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// StreamingJSONParser is pi's StreamingJsonParser (utils/streaming-json.ts,
// upstream f715a15c, #9265): incremental, tolerant parsing of JSON text that
// arrives in pieces, such as streamed tool-call arguments.
//
// Append reads each piece once, so a stream costs time and memory linear in
// its length, and Snapshot returns the value of the text read so far.
// Snapshots are plain values — nil, bool, float64, string, []any and
// OrderedObject, as DecodeOrderedValue returns them — and never change once
// returned: the parser copies the containers that are still open (the "open
// path") into every new snapshot and shares completed containers and strings.
// The rules for incomplete text follow pi's parseStreamingJson:
//
//   - An unterminated string shows the characters read so far; an
//     unterminated escape is held back until complete.
//   - An unterminated container shows the members and elements read so far.
//   - A member whose key or value has not started is omitted.
//   - An unterminated literal shows its value once it is a prefix of true,
//     false or null; an unterminated number shows its value when the digits
//     read so far form a number (an exponent without digits is ignored).
//
// Malformed text is read leniently: raw control characters in strings are
// kept, an invalid escape such as `\d` stays literal text, missing commas and
// colons are tolerated, values in invalid positions and unknown words are
// dropped, and text after the root value is ignored. The parser never fails.
// Callers that need the exact value of the finished text parse it once.
//
// Append also returns the changes the piece made (JSONChange), for a writer
// that keeps its own copy of the value: a set when a key, an array item or a
// finished value appears (a new string is set once with its first text, a
// container empty), an append when the piece extended an open string.
// Unterminated numbers and literals are not reported until they end. A
// container nested more than 32 levels deep is reported as one set of its
// whole value when it closes.
//
// Cost: copying the open path costs its member and element count. Each Append
// earns copy credit proportional to its length plus a constant; Snapshot
// spends it and returns the previous snapshot while the open path costs more
// than the credit. So a document that keeps a very large container open shows
// a preview that lags a little behind the text, and no input makes the total
// work grow faster than the text.
//
// Go differences: lengths count UTF-16 units, as pi's do; a JSON number past
// float64's range is nil, as the engine holds every number that is not finite
// (nullNonFinite in ai/providers); and an escaped high surrogate is held back
// until the next character shows whether it pairs, where pi shows it alone —
// a lone one becomes U+FFFD, as DecodeOrderedValue reads it. A raw surrogate
// split across pieces cannot occur: a Go string holds whole characters.
type StreamingJSONParser struct {
	root     any
	hasRoot  bool
	done     bool
	stack    []*jsonFrame
	str      *openJSONString
	scalar   string
	scalarAt *jsonSlot
	// pathCost is the sum of size+1 over the open path: what a fresh
	// snapshot copies.
	pathCost int
	credit   int
	dirty    bool
	// snapshot and hasSnapshot are the last snapshot handed out; ok is
	// whether a value had started.
	snapshot    any
	snapshotOK  bool
	hasSnapshot bool
	// version counts the snapshots handed out that differ from the one
	// before, so a caller can tell a new snapshot without comparing values.
	version int
	changes []*JSONChange
}

// JSONChange is one change a piece of text made to the value (pi
// JsonChange). Kind is JSONChangeSet, with Value, or JSONChangeAppend, with
// Text added to the string at Path. Path holds member keys (string) and array
// indices (int) from the root.
type JSONChange struct {
	Kind  string
	Path  []any
	Value any
	Text  string
	// grow holds the text of a change the piece keeps extending, so that
	// extending it costs the new text only (a JS string concatenation is a
	// rope; a Go one copies).
	grow *strings.Builder
}

// Kinds of JSONChange.
const (
	JSONChangeSet    = "set"
	JSONChangeAppend = "append"
)

type jsonSlotKind uint8

const (
	slotRoot jsonSlotKind = iota
	slotMember
	slotElement
)

// jsonSlot is where a value sits in its parent; nil for a value in an
// invalid position, which is read but dropped.
type jsonSlot struct {
	kind  jsonSlotKind
	key   string
	index int
}

type jsonPhase uint8

const (
	phaseKey jsonPhase = iota
	phaseColon
	phaseValue
	phaseComma
)

// openContainer marks, in its parent, a container that is still open: its
// value is the frame's, written into the parent when it closes. Snapshots
// never hold one; they get copies.
type openContainer struct{}

// jsonFrame is an open object or array.
type jsonFrame struct {
	isArray  bool
	elements []any
	fields   OrderedObject
	keys     map[string]int
	// indexKeys is whether a member key is an array index, which a JS object
	// lists first.
	indexKeys bool
	slot      *jsonSlot
	// path is where the container sits, when its changes are reported.
	path []any
	// reportsInside is whether changes inside it are reported; when not, its
	// whole value is reported as it closes.
	reportsInside bool
	// size is its members or elements: what a copy of it costs.
	size  int
	key   string
	phase jsonPhase
}

// openJSONString is an unterminated string.
type openJSONString struct {
	isKey bool
	slot  *jsonSlot
	text  strings.Builder
	// escape read so far: "" for none, otherwise a backslash and up to five
	// more characters.
	escape string
	// highSurrogate is an escaped high surrogate waiting for its pair.
	highSurrogate rune
	path          []any
	// change is the change this string extends, while it is the newest
	// change of the piece.
	change *JSONChange
}

// maxReportedDepth bounds how deep changes are reported, so a path costs O(1)
// to build.
const maxReportedDepth = 32

const (
	creditPerAppend    = 256
	creditPerCharacter = 8
	// maxPendingScalarLength: an unterminated number longer than this is not
	// shown until it ends, so Snapshot stays cheap.
	maxPendingScalarLength = 64
)

var (
	rootSlot                        = &jsonSlot{kind: slotRoot}
	jsonNumberPattern               = regexp.MustCompile(`^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$`)
	jsonNumberWithoutExponentDigits = regexp.MustCompile(`^(-?(?:0|[1-9]\d*)(?:\.\d+)?)[eE][+-]?$`)
	jsonSimpleEscapes               = map[byte]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}
	jsonLiterals                    = []struct {
		word  string
		value any
	}{{"true", true}, {"false", false}, {"null", nil}}
)

// NewStreamingJSONParser returns a parser that has read nothing.
func NewStreamingJSONParser() *StreamingJSONParser { return &StreamingJSONParser{} }

// Append reads the next piece of text and returns the changes it made, in
// order. It costs time linear in the piece.
func (p *StreamingJSONParser) Append(text string) []JSONChange {
	p.credit += creditPerAppend + creditPerCharacter*jstext.UTF16Len(text)
	p.changes = p.changes[:0]
	if p.str != nil {
		p.str.change = nil
	}
	for index := 0; index < len(text) && !p.done; {
		switch {
		case p.str != nil:
			index = p.readString(p.str, text, index)
		case p.scalar != "":
			index = p.readScalar(text, index)
		default:
			p.structural(text[index])
			index++
		}
	}
	changes := make([]JSONChange, len(p.changes))
	for i, change := range p.changes {
		changes[i] = *change
		if grow := change.grow; grow != nil {
			if change.Kind == JSONChangeSet {
				changes[i].Value = grow.String()
			} else {
				changes[i].Text = grow.String()
			}
			changes[i].grow = nil
		}
	}
	// The returned copies are the caller's; the parser keeps none of them.
	clear(p.changes)
	p.changes = p.changes[:0]
	return changes
}

// Snapshot returns the value of the text read so far: ok false before a
// value starts, otherwise a value that never changes afterwards. It returns
// the previous snapshot while the text read since did not change the value,
// or while a fresh copy of the open path is not yet paid for.
func (p *StreamingJSONParser) Snapshot() (value any, ok bool) {
	if p.dirty {
		pending, hasPending := p.pendingScalar()
		cost := p.pathCost
		if hasPending {
			cost++
		}
		if cost <= p.credit {
			p.credit -= cost
			p.dirty = false
			p.snapshot, p.snapshotOK = p.copyOpenPath(pending, hasPending)
			p.hasSnapshot = true
			p.version++
		}
	}
	if !p.hasSnapshot || !p.snapshotOK {
		return nil, false
	}
	return p.snapshot, true
}

// SnapshotVersion is the number of distinct snapshots Snapshot has returned:
// a Snapshot call that leaves it unchanged returned the previous value.
func (p *StreamingJSONParser) SnapshotVersion() int { return p.version }

// readString reads string characters from start until the string ends or the
// text does; it returns the next index. Every byte it looks for is ASCII, so
// it walks bytes and slices runs of whole characters.
func (p *StreamingJSONParser) readString(open *openJSONString, text string, start int) int {
	runStart := start
	for index := start; index < len(text); index++ {
		c := text[index]
		if open.escape != "" {
			if p.continueEscape(open, c) {
				runStart = index + 1
				continue
			}
			// Not part of the escape: read it as a normal string character.
			runStart = index
		}
		if c == '"' {
			if index > runStart {
				p.appendString(open, text[runStart:index])
			}
			p.endString(open)
			return index + 1
		}
		if c == '\\' {
			if index > runStart {
				p.appendString(open, text[runStart:index])
			}
			open.escape = `\`
			runStart = index + 1
		}
	}
	if len(text) > runStart {
		p.appendString(open, text[runStart:])
	}
	return len(text)
}

// readScalar reads number or literal characters from start up to the next
// delimiter; it returns the delimiter's index.
func (p *StreamingJSONParser) readScalar(text string, start int) int {
	end := start
	for end < len(text) && !isJSONDelimiter(text[end]) {
		end++
	}
	if end > start {
		p.scalar += text[start:end]
		if p.scalarAt != nil {
			p.dirty = true
		}
	}
	if end < len(text) {
		p.endScalar()
	}
	return end
}

func (p *StreamingJSONParser) top() *jsonFrame {
	if len(p.stack) == 0 {
		return nil
	}
	return p.stack[len(p.stack)-1]
}

// structural handles one byte outside strings. A byte of a non-ASCII
// character starts or continues a scalar, which no rule keeps.
func (p *StreamingJSONParser) structural(c byte) {
	switch c {
	case ' ', '\t', '\n', '\r':
	case '"':
		p.startString()
	case '{', '[':
		p.openContainer(c == '[')
	case '}', ']':
		p.closeContainer()
	case ':':
		if frame := p.top(); frame != nil && !frame.isArray && frame.phase == phaseColon {
			frame.phase = phaseValue
		}
	case ',':
		if frame := p.top(); frame != nil {
			if frame.isArray {
				frame.phase = phaseValue
			} else {
				frame.phase = phaseKey
			}
		}
	default:
		p.scalar = string(c)
		p.scalarAt = p.valueSlot()
		if p.scalarAt != nil {
			p.dirty = true
		}
	}
}

// valueSlot is where a value starting now goes, or nil when this position
// takes no value.
func (p *StreamingJSONParser) valueSlot() *jsonSlot {
	frame := p.top()
	if frame == nil {
		if !p.hasRoot {
			return rootSlot
		}
		return nil
	}
	if frame.isArray {
		return &jsonSlot{kind: slotElement, index: len(frame.elements)}
	}
	// A missing colon is tolerated: `{"a" 1}` reads as `{"a": 1}`. An object
	// reaches either phase only through a key, so the key is set.
	if frame.phase == phaseValue || frame.phase == phaseColon {
		return &jsonSlot{kind: slotMember, key: frame.key}
	}
	return nil
}

func (p *StreamingJSONParser) startString() {
	frame := p.top()
	// A missing comma is tolerated: `{"a": 1 "b": 2}` reads the second member.
	if frame != nil && !frame.isArray && (frame.phase == phaseKey || frame.phase == phaseComma) {
		p.str = &openJSONString{isKey: true}
		return
	}
	slot := p.valueSlot()
	path := p.reportedPath(slot)
	p.str = &openJSONString{slot: slot, path: path}
	p.place(slot, "")
	if path != nil {
		p.str.change = p.report(&JSONChange{Kind: JSONChangeSet, Path: path, Value: ""})
	}
}

// reportedPath is the path of a value at slot in the innermost open
// container, when its changes are reported.
func (p *StreamingJSONParser) reportedPath(slot *jsonSlot) []any {
	if slot == nil {
		return nil
	}
	if slot.kind == slotRoot {
		return []any{}
	}
	frame := p.top()
	if !frame.reportsInside {
		return nil
	}
	path := make([]any, len(frame.path), len(frame.path)+1)
	copy(path, frame.path)
	if slot.kind == slotMember {
		return append(path, slot.key)
	}
	return append(path, slot.index)
}

func (p *StreamingJSONParser) report(change *JSONChange) *JSONChange {
	p.changes = append(p.changes, change)
	return change
}

// continueEscape continues an escape with c; it returns false when c is not
// part of it and must be read normally.
func (p *StreamingJSONParser) continueEscape(open *openJSONString, c byte) bool {
	if open.escape == `\` {
		if decoded, ok := jsonSimpleEscapes[c]; ok {
			open.escape = ""
			p.appendString(open, decoded)
			return true
		}
		if c == 'u' {
			open.escape = `\u`
			return true
		}
		// Invalid escape: keep the backslash as text and read the character
		// normally, as repairJSON does.
		open.escape = ""
		p.appendString(open, `\`)
		return false
	}
	if isJSONHexDigit(c) {
		open.escape += string(c)
		if len(open.escape) < 6 {
			return true
		}
		unit, _ := strconv.ParseUint(open.escape[2:], 16, 16)
		open.escape = ""
		p.appendUnit(open, rune(unit))
		return true
	}
	// `\u` without four hex digits: keep it as text and read the character
	// normally, as repairJSON does.
	literal := open.escape
	open.escape = ""
	p.appendString(open, literal)
	return false
}

// appendUnit appends one UTF-16 code unit decoded from a `\u` escape,
// pairing surrogates.
func (p *StreamingJSONParser) appendUnit(open *openJSONString, unit rune) {
	if utf16.IsSurrogate(unit) && unit < 0xDC00 {
		p.flushHighSurrogate(open)
		open.highSurrogate = unit
		return
	}
	if open.highSurrogate != 0 && utf16.IsSurrogate(unit) {
		pair := utf16.DecodeRune(open.highSurrogate, unit)
		open.highSurrogate = 0
		p.appendText(open, string(pair))
		return
	}
	if utf16.IsSurrogate(unit) {
		unit = utf8.RuneError
	}
	p.appendString(open, string(unit))
}

// flushHighSurrogate appends a held high surrogate that found no pair, as
// U+FFFD.
func (p *StreamingJSONParser) flushHighSurrogate(open *openJSONString) {
	if open.highSurrogate == 0 {
		return
	}
	open.highSurrogate = 0
	p.appendText(open, string(utf8.RuneError))
}

func (p *StreamingJSONParser) appendString(open *openJSONString, text string) {
	p.flushHighSurrogate(open)
	p.appendText(open, text)
}

func (p *StreamingJSONParser) appendText(open *openJSONString, text string) {
	open.text.WriteString(text)
	if open.isKey {
		return
	}
	p.place(open.slot, open.text.String())
	if open.path == nil {
		return
	}
	// Extend this string's newest change while nothing came after it;
	// otherwise report an append.
	if change := open.change; change != nil && p.changes[len(p.changes)-1] == change {
		if change.grow == nil {
			change.grow = &strings.Builder{}
			if change.Kind == JSONChangeSet {
				change.grow.WriteString(change.Value.(string))
			} else {
				change.grow.WriteString(change.Text)
			}
		}
		change.grow.WriteString(text)
		return
	}
	open.change = p.report(&JSONChange{Kind: JSONChangeAppend, Path: open.path, Text: text})
}

func (p *StreamingJSONParser) endString(open *openJSONString) {
	p.flushHighSurrogate(open)
	p.str = nil
	frame := p.top()
	if open.isKey {
		if frame == nil {
			return
		}
		frame.key = open.text.String()
		frame.phase = phaseColon
		return
	}
	p.valueEnded(frame)
}

func (p *StreamingJSONParser) endScalar() {
	token, slot := p.scalar, p.scalarAt
	p.scalar, p.scalarAt = "", nil
	if value, ok := jsonScalarValue(token); ok {
		p.place(slot, value)
		if path := p.reportedPath(slot); path != nil {
			p.report(&JSONChange{Kind: JSONChangeSet, Path: path, Value: value})
		}
	}
	p.valueEnded(p.top())
}

// pendingScalar is the value of the unterminated scalar at the end of the
// text, if it shows.
func (p *StreamingJSONParser) pendingScalar() (pendingValue, bool) {
	if p.scalarAt == nil || p.scalar == "" || jstext.UTF16Len(p.scalar) > maxPendingScalarLength {
		return pendingValue{}, false
	}
	value, ok := jsonPendingScalarValue(p.scalar)
	if !ok {
		return pendingValue{}, false
	}
	return pendingValue{slot: p.scalarAt, value: value}, true
}

type pendingValue struct {
	slot  *jsonSlot
	value any
}

func (p *StreamingJSONParser) openContainer(isArray bool) {
	slot := p.valueSlot()
	path := p.reportedPath(slot)
	p.place(slot, openContainer{})
	frame := &jsonFrame{
		isArray:       isArray,
		slot:          slot,
		path:          path,
		reportsInside: path != nil && len(path) < maxReportedDepth,
		phase:         phaseValue,
	}
	if isArray {
		frame.elements = []any{}
	} else {
		frame.fields = OrderedObject{}
		frame.keys = map[string]int{}
		frame.phase = phaseKey
	}
	p.stack = append(p.stack, frame)
	p.pathCost++
	p.dirty = true
	// A fresh container: the working one keeps changing, and a change never
	// does.
	if path != nil {
		var empty any = OrderedObject{}
		if isArray {
			empty = []any{}
		}
		p.report(&JSONChange{Kind: JSONChangeSet, Path: path, Value: empty})
	}
}

func (p *StreamingJSONParser) closeContainer() {
	frame := p.top()
	// A stray closer outside any container is ignored.
	if frame == nil {
		return
	}
	p.stack = p.stack[:len(p.stack)-1]
	p.pathCost -= frame.size + 1
	p.dirty = true
	value := frame.value()
	// The container's final value replaces its mark in the parent.
	p.write(frame.slot, value)
	// Too deep to report inside: the whole value, final now that it is
	// closed.
	if frame.path != nil && !frame.reportsInside {
		p.report(&JSONChange{Kind: JSONChangeSet, Path: frame.path, Value: value})
	}
	p.valueEnded(p.top())
}

// valueEnded: a value in frame, or the root value when frame is nil, has
// ended.
func (p *StreamingJSONParser) valueEnded(frame *jsonFrame) {
	if frame == nil {
		// Text after the root value is ignored, but only once a root value
		// exists.
		if p.hasRoot {
			p.done = true
		}
		return
	}
	frame.phase = phaseComma
}

// place sets the value at slot in the open path; nothing for a value in an
// invalid position.
func (p *StreamingJSONParser) place(slot *jsonSlot, value any) {
	if slot == nil {
		return
	}
	p.dirty = true
	if slot.kind == slotRoot {
		p.root, p.hasRoot = value, true
		return
	}
	frame := p.top()
	if frame.set(slot, value) {
		frame.size++
		p.pathCost++
	}
}

// write replaces the value at slot, in the innermost open container or the
// root, without counting it: the slot already holds a value.
func (p *StreamingJSONParser) write(slot *jsonSlot, value any) {
	if slot == nil {
		return
	}
	if slot.kind == slotRoot {
		p.root = value
		return
	}
	p.top().set(slot, value)
}

// set writes value at slot and reports whether the slot is new.
func (f *jsonFrame) set(slot *jsonSlot, value any) bool {
	if slot.kind == slotElement {
		if slot.index == len(f.elements) {
			f.elements = append(f.elements, value)
			return true
		}
		f.elements[slot.index] = value
		return false
	}
	// A duplicate key keeps its first position and takes the last value, as
	// JSON.parse does.
	if i, held := f.keys[slot.key]; held {
		f.fields[i].Value = value
		return false
	}
	f.keys[slot.key] = len(f.fields)
	f.fields = append(f.fields, OrderedField{Key: slot.key, Value: value})
	if _, isIndex := jstext.ArrayIndexKey(slot.key); isIndex {
		f.indexKeys = true
	}
	return true
}

// value is the frame's container as a value: its own storage, which it no
// longer changes once closed, in a JS object's key order.
func (f *jsonFrame) value() any {
	if f.isArray {
		return f.elements
	}
	if f.indexKeys {
		return jsKeyOrder(f.fields)
	}
	return f.fields
}

// copyValue is a copy of the frame's container, with pending set at its slot
// and child at childSlot, in a JS object's key order.
func (f *jsonFrame) copyValue(pending *pendingValue, child any, childSlot *jsonSlot) any {
	if f.isArray {
		elements := slices.Clone(f.elements)
		for _, s := range []struct {
			slot  *jsonSlot
			value any
		}{{pendingSlot(pending), pendingContent(pending)}, {childSlot, child}} {
			if s.slot == nil || s.slot.kind != slotElement {
				continue
			}
			if s.slot.index == len(elements) {
				elements = append(elements, s.value)
			} else {
				elements[s.slot.index] = s.value
			}
		}
		return elements
	}
	fields := slices.Clone(f.fields)
	indexKeys := f.indexKeys
	for _, s := range []struct {
		slot  *jsonSlot
		value any
	}{{pendingSlot(pending), pendingContent(pending)}, {childSlot, child}} {
		if s.slot == nil || s.slot.kind != slotMember {
			continue
		}
		if i, held := f.keys[s.slot.key]; held {
			fields[i].Value = s.value
			continue
		}
		fields = append(fields, OrderedField{Key: s.slot.key, Value: s.value})
		if _, isIndex := jstext.ArrayIndexKey(s.slot.key); isIndex {
			indexKeys = true
		}
	}
	if indexKeys {
		return jsKeyOrder(fields)
	}
	return fields
}

func pendingSlot(pending *pendingValue) *jsonSlot {
	if pending == nil {
		return nil
	}
	return pending.slot
}

func pendingContent(pending *pendingValue) any {
	if pending == nil {
		return nil
	}
	return pending.value
}

// jsKeyOrder lists array-index keys first, ascending, then the rest in
// insertion order, as a JS object enumerates its own keys. It returns a new
// slice.
func jsKeyOrder(fields OrderedObject) OrderedObject {
	// Each key is read once: the index keys, sorted, then the rest in order.
	type indexed struct {
		index uint32
		field OrderedField
	}
	var indices []indexed
	rest := make(OrderedObject, 0, len(fields))
	for _, f := range fields {
		if index, isIndex := jstext.ArrayIndexKey(f.Key); isIndex {
			indices = append(indices, indexed{index, f})
		} else {
			rest = append(rest, f)
		}
	}
	slices.SortFunc(indices, func(a, b indexed) int { return cmp.Compare(a.index, b.index) })
	ordered := make(OrderedObject, 0, len(fields))
	for _, i := range indices {
		ordered = append(ordered, i.field)
	}
	return append(ordered, rest...)
}

// copyOpenPath is a fresh snapshot: copies of the open containers, innermost
// first, each holding the copy of its open child.
func (p *StreamingJSONParser) copyOpenPath(pending pendingValue, hasPending bool) (any, bool) {
	if len(p.stack) == 0 {
		if hasPending && pending.slot.kind == slotRoot {
			return pending.value, true
		}
		return p.root, p.hasRoot
	}
	var pendingAt *pendingValue
	if hasPending {
		pendingAt = &pending
	}
	var child any
	var childSlot *jsonSlot
	for depth := len(p.stack) - 1; depth >= 0; depth-- {
		frame := p.stack[depth]
		var innermost *pendingValue
		if depth == len(p.stack)-1 {
			innermost = pendingAt
		}
		child = frame.copyValue(innermost, child, childSlot)
		childSlot = frame.slot
	}
	// The outermost open container is the root unless it sits in an invalid
	// position.
	if childSlot != nil && childSlot.kind == slotRoot {
		return child, true
	}
	return p.root, p.hasRoot
}

func isJSONDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ':', '{', '}', '[', ']', '"':
		return true
	}
	return false
}

func isJSONHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// jsonNumber is a JSON number token's value; one past float64's range is
// nil, as the engine holds every number that is not finite.
func jsonNumber(token string) any {
	value, _ := strconv.ParseFloat(token, 64)
	if math.IsInf(value, 0) {
		return nil
	}
	return value
}

// jsonScalarValue is the value of a complete number or literal token; false
// for anything else, which is dropped.
func jsonScalarValue(token string) (any, bool) {
	for _, literal := range jsonLiterals {
		if token == literal.word {
			return literal.value, true
		}
	}
	if jsonNumberPattern.MatchString(token) {
		return jsonNumber(token), true
	}
	return nil, false
}

// jsonPendingScalarValue is the value an unterminated number or literal token
// shows.
func jsonPendingScalarValue(token string) (any, bool) {
	for _, literal := range jsonLiterals {
		if strings.HasPrefix(literal.word, token) {
			return literal.value, true
		}
	}
	if jsonNumberPattern.MatchString(token) {
		return jsonNumber(token), true
	}
	if mantissa := jsonNumberWithoutExponentDigits.FindStringSubmatch(token); mantissa != nil {
		return jsonNumber(mantissa[1]), true
	}
	return nil, false
}
