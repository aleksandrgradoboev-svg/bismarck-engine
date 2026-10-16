package jsonl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The line formats of main.jsonl and the sidecars (pi's MainMarker and
// SidecarRecord), and their validation on read. Field order follows pi's
// object literals, so the files read the same.

const formatVersion = 1

// Main-file write types beyond pi's StorageWrite discriminators.
const (
	opTaskSidecar = "task.sidecar"
)

type valueOp struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type idOp struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

type ordinalOp struct {
	Type    string `json:"type"`
	ID      int64  `json:"id"`
	Ordinal int    `json:"ordinal"`
}

type createOp struct {
	Type    string                 `json:"type"`
	Record  durable.DocumentCreate `json:"record"`
	Ordinal int                    `json:"ordinal"`
}

type markerLine struct {
	Format int         `json:"format"`
	Type   string      `json:"type"`
	Seq    durable.Seq `json:"seq"`
	Writes []any       `json:"writes"`
}

type sidecarLine struct {
	Format  int         `json:"format"`
	Type    string      `json:"type"`
	Seq     durable.Seq `json:"seq"`
	Ordinal int         `json:"ordinal"`
	Payload any         `json:"payload"`
}

type taskPayload struct {
	Type  string             `json:"type"`
	Value durable.TaskRecord `json:"value"`
}

type documentPayload struct {
	Type    string             `json:"type"`
	ID      durable.DocumentID `json:"id"`
	Content wireContent        `json:"content"`
}

// wireContent is DocumentContent with pi's required members: a base always
// has its value and a delta its ops, even when empty, which
// DocumentContent's omitempty would drop.
type wireContent durable.DocumentContent

func (c wireContent) MarshalJSON() ([]byte, error) {
	if c.Kind == durable.ContentBase {
		value := c.Value
		if value == nil {
			value = map[string]any{}
		}
		return json.Marshal(struct {
			Kind    string         `json:"kind"`
			Version int            `json:"version"`
			Value   map[string]any `json:"value"`
		}{c.Kind, c.Version, value})
	}
	ops := c.Ops
	if ops == nil {
		ops = []delta.Op{}
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Version int        `json:"version"`
		Ops     []delta.Op `json:"ops"`
	}{c.Kind, c.Version, ops})
}

func jsonLine(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// mainOp is one decoded main-file write.
type mainOp struct {
	typ string
	// write is set for "conversation", "entry", "submission", a terminal
	// "task" and "document.retire".
	write   durable.StorageWrite
	id      int64
	ordinal int
	record  durable.DocumentCreate
}

type mainMarker struct {
	seq    durable.Seq
	writes []mainOp
}

// sidecarRecord is one decoded sidecar line; text is the line without its
// newline, written back unchanged when a sidecar is compacted.
type sidecarRecord struct {
	seq     durable.Seq
	ordinal int
	isTask  bool
	task    durable.TaskRecord
	docID   durable.DocumentID
	content durable.DocumentContent
	text    []byte
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the bound of pi's
// isSafeInteger.
const maxSafeInteger = 1<<53 - 1

func safeInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, i >= -maxSafeInteger && i <= maxSafeInteger
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
		return 0, false
	}
	return int64(f), true
}

func object(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// loneSurrogateEscape reports a \uD800–\uDFFF escape that is not half of a
// pair. encoding/json would decode it as U+FFFD, merging distinct strings,
// so a line holding one is refused rather than read lossily.
func loneSurrogateEscape(text []byte) bool {
	hex := func(i int) (rune, bool) {
		if i+6 > len(text) || text[i] != '\\' || text[i+1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range text[i+2 : i+6] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		r, ok := hex(i)
		if !ok {
			i++ // skip the escaped character, so `\\u` is not an escape
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			low, ok := hex(i + 6)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 11
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		default:
			i += 5
		}
	}
	return false
}

func parseJSON(text []byte, description string) (any, error) {
	if loneSurrogateEscape(text) {
		return nil, corruption(nil, "Unsupported lone surrogate escape in complete %s", description)
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, corruption(err, "Malformed complete %s", description)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, corruption(err, "Malformed complete %s", description)
	}
	return v, nil
}

func decodeInto(raw json.RawMessage, dst any, format string, args ...any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return corruption(err, format, args...)
	}
	return nil
}

func validateMainOperation(value any, raw json.RawMessage, description string) (mainOp, error) {
	m, ok := object(value)
	typ, isString := m["type"].(string)
	if !ok || !isString {
		return mainOp{}, corruption(nil, "Invalid write in %s", description)
	}
	op := mainOp{typ: typ}
	switch typ {
	case "conversation", "entry", "submission":
		v, ok := object(m["value"])
		if _, safe := safeInteger(v["id"]); !ok || !safe {
			return mainOp{}, corruption(nil, "Invalid %s write in %s", typ, description)
		}
		var err error
		switch typ {
		case "conversation":
			var w struct{ Value durable.ConversationRecord }
			err = decodeInto(raw, &w, "Invalid %s write in %s", typ, description)
			op.write = durable.ConversationWrite{Value: w.Value}
		case "entry":
			var w struct{ Value durable.EntryRecord }
			err = decodeInto(raw, &w, "Invalid %s write in %s", typ, description)
			op.write = durable.EntryWrite{Value: w.Value}
		default:
			var w struct{ Value durable.SubmissionRecord }
			err = decodeInto(raw, &w, "Invalid %s write in %s", typ, description)
			op.write = durable.SubmissionWrite{Value: w.Value}
		}
		return op, err
	case "task":
		v, ok := object(m["value"])
		state, isObject := object(v["state"])
		if _, safe := safeInteger(v["id"]); !ok || !safe || !isObject || state["status"] != durable.TaskTerminal {
			return mainOp{}, corruption(nil, "Invalid terminal task write in %s", description)
		}
		var w struct{ Value durable.TaskRecord }
		err := decodeInto(raw, &w, "Invalid terminal task write in %s", description)
		op.write = durable.TaskWrite{Value: w.Value}
		return op, err
	case "document.retire":
		id, safe := safeInteger(m["id"])
		if !safe {
			return mainOp{}, corruption(nil, "Invalid document retirement in %s", description)
		}
		op.id, op.write = id, durable.DocumentRetireWrite{ID: durable.DocumentID(id)}
		return op, nil
	case opTaskSidecar, "document.change":
		id, safeID := safeInteger(m["id"])
		ordinal, safeOrdinal := safeInteger(m["ordinal"])
		if !safeID || !safeOrdinal || ordinal < 0 {
			if typ == opTaskSidecar {
				return mainOp{}, corruption(nil, "Invalid task sidecar write in %s", description)
			}
			return mainOp{}, corruption(nil, "Invalid document change in %s", description)
		}
		op.id, op.ordinal = id, int(ordinal)
		return op, nil
	case "document.create":
		record, ok := object(m["record"])
		_, safeID := safeInteger(record["id"])
		ordinal, safeOrdinal := safeInteger(m["ordinal"])
		if !ok || !safeID || !safeOrdinal || ordinal < 0 {
			return mainOp{}, corruption(nil, "Invalid document creation in %s", description)
		}
		var w struct{ Record durable.DocumentCreate }
		err := decodeInto(raw, &w, "Invalid document creation in %s", description)
		op.record, op.id, op.ordinal = w.Record, int64(w.Record.ID), int(ordinal)
		return op, err
	}
	return mainOp{}, corruption(nil, "Unknown write type in %s", description)
}

func parseMainMarker(text []byte, line int) (mainMarker, error) {
	description := fmt.Sprintf("%s line %d", mainFile, line)
	value, err := parseJSON(text, description)
	if err != nil {
		return mainMarker{}, err
	}
	m, ok := object(value)
	format, _ := safeInteger(m["format"])
	seq, safeSeq := safeInteger(m["seq"])
	writes, isArray := m["writes"].([]any)
	if !ok || format != formatVersion || m["type"] != "commit" || !safeSeq || seq < 1 || !isArray {
		return mainMarker{}, corruption(nil, "Invalid commit marker in %s", description)
	}
	var raw struct{ Writes []json.RawMessage }
	if err := decodeInto(text, &raw, "Invalid commit marker in %s", description); err != nil {
		return mainMarker{}, err
	}
	marker := mainMarker{seq: durable.Seq(seq), writes: make([]mainOp, len(writes))}
	for i, w := range writes {
		if marker.writes[i], err = validateMainOperation(w, raw.Writes[i], description); err != nil {
			return mainMarker{}, err
		}
	}
	return marker, nil
}

func validateDocumentContent(value any, description string) error {
	m, ok := object(value)
	version, safe := safeInteger(m["version"])
	if !ok || !safe || version < 1 {
		return corruption(nil, "Invalid document content in %s", description)
	}
	if _, isObject := object(m["value"]); m["kind"] == durable.ContentBase && isObject {
		return nil
	}
	if _, isArray := m["ops"].([]any); m["kind"] == durable.ContentDelta && isArray {
		return nil
	}
	return corruption(nil, "Invalid document content in %s", description)
}

func parseSidecarRecord(text []byte, file string, line int) (sidecarRecord, error) {
	description := fmt.Sprintf("%s line %d", file, line)
	value, err := parseJSON(text, description)
	if err != nil {
		return sidecarRecord{}, err
	}
	m, ok := object(value)
	format, _ := safeInteger(m["format"])
	seq, safeSeq := safeInteger(m["seq"])
	ordinal, safeOrdinal := safeInteger(m["ordinal"])
	payload, isObject := object(m["payload"])
	payloadType, isString := payload["type"].(string)
	if !ok || format != formatVersion || m["type"] != "record" || !safeSeq || seq < 1 ||
		!safeOrdinal || ordinal < 0 || !isObject || !isString {
		return sidecarRecord{}, corruption(nil, "Invalid sidecar record in %s", description)
	}
	record := sidecarRecord{seq: durable.Seq(seq), ordinal: int(ordinal), text: text}
	switch payloadType {
	case "task":
		v, ok := object(payload["value"])
		state, isObject := object(v["state"])
		if _, safe := safeInteger(v["id"]); !ok || !safe || !isObject || state["status"] == durable.TaskTerminal {
			return sidecarRecord{}, corruption(nil, "Invalid live task record in %s", description)
		}
		var w struct {
			Payload struct{ Value durable.TaskRecord }
		}
		if err := decodeInto(text, &w, "Invalid live task record in %s", description); err != nil {
			return sidecarRecord{}, err
		}
		record.isTask, record.task = true, w.Payload.Value
	case "document":
		if _, safe := safeInteger(payload["id"]); !safe {
			return sidecarRecord{}, corruption(nil, "Invalid document record in %s", description)
		}
		if err := validateDocumentContent(payload["content"], description); err != nil {
			return sidecarRecord{}, err
		}
		var w struct {
			Payload struct {
				ID      durable.DocumentID
				Content durable.DocumentContent
			}
		}
		if err := decodeInto(text, &w, "Invalid document content in %s", description); err != nil {
			return sidecarRecord{}, err
		}
		record.docID, record.content = w.Payload.ID, w.Payload.Content
	default:
		return sidecarRecord{}, corruption(nil, "Unknown sidecar record type in %s", description)
	}
	return record, nil
}
