package providers

import (
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// ToolCallArgumentsStream is pi's ToolCallArgumentsStream (utils/
// streaming-json.ts, upstream f715a15c): the arguments of one streaming tool
// call — the raw text, for the one full parse when the call ends, and an
// ai.StreamingJSONParser for the partial arguments in between. Providers keep
// one per call in their own stream state, never on the content block, so the
// raw text never reaches partial messages or transcripts.
//
// The partial arguments come in the two forms a ToolCall holds them: the map
// and the object in pi's key order. A snapshot shares its completed
// containers with the one before, so their maps are converted once and
// reused; only the containers still open are converted again. Partial
// arguments, like pi's snapshots, are never changed afterwards and must not be
// changed by their readers.
type ToolCallArgumentsStream struct {
	text    strings.Builder
	parser  *ai.StreamingJSONParser
	version int
	args    map[string]any
	order   ai.OrderedObject
	plain   plainMemo
}

// NewToolCallArgumentsStream returns the arguments of a call that has
// streamed nothing yet.
func NewToolCallArgumentsStream() *ToolCallArgumentsStream {
	return &ToolCallArgumentsStream{parser: ai.NewStreamingJSONParser(), args: map[string]any{}}
}

// Text is the raw argument text received so far.
func (s *ToolCallArgumentsStream) Text() string { return s.text.String() }

// Append appends a piece of argument text and returns the partial arguments:
// a new map whenever they changed, the same one otherwise.
func (s *ToolCallArgumentsStream) Append(delta string) (map[string]any, ai.OrderedObject) {
	s.text.WriteString(delta)
	s.parser.Append(delta)
	return s.partial()
}

// Replace takes the provider's complete argument text. When it continues the
// text so far, it returns the missing piece as delta, with continued true;
// otherwise it starts over with text. Either way it returns the partial
// arguments of text.
func (s *ToolCallArgumentsStream) Replace(text string) (delta string, continued bool, args map[string]any, order ai.OrderedObject) {
	if so := s.text.String(); strings.HasPrefix(text, so) {
		delta = text[len(so):]
		if delta == "" {
			args, order = s.partial()
		} else {
			args, order = s.Append(delta)
		}
		return delta, true, args, order
	}
	s.text.Reset()
	s.parser = ai.NewStreamingJSONParser()
	s.version, s.args, s.order, s.plain = 0, map[string]any{}, nil, plainMemo{}
	args, order = s.Append(text)
	return "", false, args, order
}

// Finish is the final arguments: one full parse of the complete text, which
// reports malformed arguments (parseToolCallArguments).
func (s *ToolCallArgumentsStream) Finish() (map[string]any, ai.OrderedObject, string) {
	return parseToolCallArguments(s.text.String())
}

func (s *ToolCallArgumentsStream) partial() (map[string]any, ai.OrderedObject) {
	snapshot, _ := s.parser.Snapshot()
	if version := s.parser.SnapshotVersion(); version != s.version {
		s.version = version
		// Tool arguments are an object; any other partial value shows as no
		// arguments yet.
		if object, isObject := snapshot.(ai.OrderedObject); isObject {
			s.order = object
			s.args = s.plain.object(object)
		} else {
			s.order, s.args = nil, map[string]any{}
		}
	}
	return s.args, s.order
}

// plainMemo converts snapshot values to the map form, reusing the conversion
// of a container the previous snapshot shared. A container is known by its
// first element's address: a completed one is the same storage in every
// snapshot, an open one a fresh copy. Each conversion keeps only the entries
// it reached, so the memo never outgrows one snapshot.
type plainMemo struct{ prev, next plainTables }

type plainTables struct {
	objects map[*ai.OrderedField]map[string]any
	arrays  map[*any][]any
}

func (m *plainMemo) object(o ai.OrderedObject) map[string]any {
	m.next = plainTables{objects: map[*ai.OrderedField]map[string]any{}, arrays: map[*any][]any{}}
	plain := m.convertObject(o)
	m.prev, m.next = m.next, plainTables{}
	return plain
}

func (m *plainMemo) convertObject(o ai.OrderedObject) map[string]any {
	if len(o) == 0 {
		return map[string]any{}
	}
	plain, ok := m.prev.objects[&o[0]]
	if !ok {
		plain = make(map[string]any, len(o))
		for _, f := range o {
			plain[f.Key] = m.convert(f.Value)
		}
	}
	m.next.objects[&o[0]] = plain
	return plain
}

func (m *plainMemo) convert(v any) any {
	switch t := v.(type) {
	case ai.OrderedObject:
		return m.convertObject(t)
	case []any:
		if len(t) == 0 {
			return []any{}
		}
		plain, ok := m.prev.arrays[&t[0]]
		if !ok {
			plain = make([]any, len(t))
			for i, e := range t {
				plain[i] = m.convert(e)
			}
		}
		m.next.arrays[&t[0]] = plain
		return plain
	}
	return v
}
