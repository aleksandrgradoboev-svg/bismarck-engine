package main

// Twin of ../pi/canon.mjs. Keep the two in lockstep: any change to what is
// normalized must land on both sides, or the differential starts comparing
// apples to oranges.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// rawNumber is a JSON number kept as its original literal text, so 1024 vs
// 1024.0 vs 1e3 stays visible instead of being laundered through a float.
type rawNumber string

// parsePreservingNumbers decodes JSON into any, with numbers as rawNumber and
// objects as orderedObject (insertion order preserved).
func parsePreservingNumbers(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// orderedObject is a JSON object that remembers its key order.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return parseFromToken(dec, tok)
}

func parseFromToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &orderedObject{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T, want string", kt)
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				obj.keys = append(obj.keys, key)
				obj.vals[key] = val
			}
			if _, err := dec.Token(); err != nil { // closing }
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // closing ]
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case json.Number:
		return rawNumber(t.String()), nil
	default:
		return tok, nil // string, bool, nil
	}
}

// encodeString matches JSON.stringify (ES2019 well-formed): escape the
// mandatory control set and lone surrogates, nothing else. In particular
// U+2028/U+2029 are NOT escaped, and neither are <, >, & — Go's encoding/json
// escapes all five by default, which is why this writer exists.
func encodeString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range utf16Units(s) {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r >= 0xd800 && r <= 0xdfff:
			// Lone surrogate (paired ones were recombined by utf16Units).
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(rune(r))
		}
	}
	b.WriteByte('"')
	return b.String()
}

// utf16Units yields code points, recombining valid surrogate pairs, so lone
// surrogates can be spotted and escaped exactly like JSON.stringify does.
func utf16Units(s string) []rune {
	units := utf16.Encode([]rune(s))
	out := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xd800 && u <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff {
			out = append(out, utf16.DecodeRune(rune(u), rune(units[i+1])))
			i++
			continue
		}
		out = append(out, rune(u))
	}
	// Guard against invalid UTF-8 in the source producing replacement chars.
	if !utf8.ValidString(s) {
		return []rune(s)
	}
	return out
}

// canonicalBody renders with object keys SORTED, arrays untouched, numbers verbatim.
func canonicalBody(v any, indent int) string {
	pad := strings.Repeat("  ", indent)
	padIn := strings.Repeat("  ", indent+1)
	switch t := v.(type) {
	case nil:
		return "null"
	case rawNumber:
		return string(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return encodeString(t)
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = padIn + canonicalBody(e, indent+1)
		}
		return "[\n" + strings.Join(parts, ",\n") + "\n" + pad + "]"
	case *orderedObject:
		if len(t.keys) == 0 {
			return "{}"
		}
		keys := append([]string(nil), t.keys...)
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = padIn + encodeString(k) + ": " + canonicalBody(t.vals[k], indent+1)
		}
		return "{\n" + strings.Join(parts, ",\n") + "\n" + pad + "}"
	default:
		panic(fmt.Sprintf("canonicalBody: unexpected %T", v))
	}
}

// orderReport emits one line per object, "<path>\t<keys in original order>".
// Traversal is by SORTED keys so both sides walk identical paths even when
// their orders differ.
func orderReport(v any, path string, lines *[]string) {
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			orderReport(e, fmt.Sprintf("%s[%d]", path, i), lines)
		}
	case *orderedObject:
		*lines = append(*lines, path+"\t"+strings.Join(t.keys, ","))
		keys := append([]string(nil), t.keys...)
		sort.Strings(keys)
		for _, k := range keys {
			orderReport(t.vals[k], path+"."+k, lines)
		}
	}
}

func canonicalize(wire []byte) (body string, order string, err error) {
	parsed, err := parsePreservingNumbers(wire)
	if err != nil {
		return "", "", err
	}
	var lines []string
	orderReport(parsed, "$", &lines)
	return canonicalBody(parsed, 0) + "\n", strings.Join(lines, "\n") + "\n", nil
}

// marshalWire serializes a payload the way it goes on the wire, with Go's
// HTML escaping disabled so the bytes are comparable to JSON.stringify.
func marshalWire(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
