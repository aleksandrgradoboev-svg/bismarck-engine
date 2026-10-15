package cbor

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The vectors in testdata/upstream_vectors.json are produced by running
// testdata/gen-vectors.ts against upstream pi's own packages/protocol/src/cbor
// (node gen-vectors.ts, from a tree of that directory). CBOR bytes are visible
// to a *peer*, so unlike every other golden in this port these are not just a
// regression fence: a divergence here means a Go process and a Node process
// cannot talk to each other. Regenerate from upstream, never by hand.
type upstreamVectors struct {
	Encoded []struct {
		Name  string `json:"name"`
		Cbor  string `json:"cbor"`
		Frame string `json:"frame"`
		Error string `json:"error"`
	} `json:"encoded"`
	Rejects []struct {
		Name  string `json:"name"`
		Cbor  string `json:"cbor"`
		Error string `json:"error"`
	} `json:"rejects"`
	Decoded []struct {
		Name  string `json:"name"`
		Hex   string `json:"hex"`
		OK    bool   `json:"ok"`
		JSON  string `json:"json"`
		Error string `json:"error"`
	} `json:"decoded"`
	Bounds []boundsVector `json:"bounds"`
}

// boundsVector is one limit at its bound or one past it, as pi resolves it
// both ways: Hex is the value under the default limits, Encoded or
// EncodeError is what pi's encoder makes of the value under Options, and
// DecodeOK or DecodeError what its decoder makes of Hex.
type boundsVector struct {
	Name        string         `json:"name"`
	Options     map[string]int `json:"options"`
	Hex         string         `json:"hex"`
	Encoded     string         `json:"encoded"`
	EncodeError string         `json:"encodeError"`
	DecodeOK    bool           `json:"decodeOk"`
	DecodeError string         `json:"decodeError"`
}

func loadVectors(t *testing.T) upstreamVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v upstreamVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return v
}

// goValues maps each upstream vector name to the Go value that models the same
// JS value. Names absent here are covered by a divergence case below.
func goValues() map[string]any {
	longString := make([]byte, 300)
	for i := range longString {
		longString[i] = 'x'
	}
	return map[string]any{
		"null":                 nil,
		"true":                 true,
		"false":                false,
		"zero":                 int64(0),
		"one":                  int64(1),
		"twentythree":          int64(23),
		"twentyfour":           int64(24),
		"uint8_max":            int64(255),
		"uint16_edge":          int64(256),
		"uint16_max":           int64(65535),
		"uint32_edge":          int64(65536),
		"uint32_max":           int64(4294967295),
		"uint64_edge":          int64(4294967296),
		"max_safe_int":         int64(maxSafeInteger),
		"neg_one":              int64(-1),
		"neg_twentyfour":       int64(-24),
		"neg_twentyfive":       int64(-25),
		"neg_max_safe":         int64(-maxSafeInteger),
		"float_half":           0.5,
		"float_neg_half":       -0.5,
		"float_pi":             math.Pi,
		"neg_zero":             math.Copysign(0, -1),
		"integral_float":       float64(1),
		"integral_float_big":   1e15,
		"empty_string":         "",
		"ascii":                "hello",
		"unicode":              "héllo ✨ 世界",
		"emoji_surrogate_pair": "👋🏽",
		"long_string":          string(longString),
		"empty_bytes":          []byte{},
		"bytes":                []byte{0, 1, 2, 250, 255},
		"empty_array":          []any{},
		"array_ints":           []any{int64(1), int64(2), int64(3)},
		"nested_array":         []any{int64(1), []any{int64(2), []any{int64(3), []any{int64(4)}}}},
		"empty_map":            map[string]any{},
		"map_simple":           map[string]any{"a": int64(1), "b": "two", "c": true},
		// pi drops undefined properties; Go has no undefined, so the
		// equivalent value simply omits the key. Bytes must match exactly.
		"map_undefined_dropped": map[string]any{"a": int64(1), "c": int64(3)},
		"map_null_kept":         map[string]any{"a": nil},
		"nested_map":            map[string]any{"outer": map[string]any{"inner": map[string]any{"deep": []any{int64(1), int64(2)}}}},
		// Objects whose key order is not already alphabetical are modelled as
		// structs, which is how the port encodes them for real: declaration
		// order reproduces the JS object's insertion order exactly.
		"mixed": vectorMixed{
			ID: "s1", N: 42, OK: true,
			Tags: []any{"a", "b"}, Meta: map[string]any{"x": nil},
		},
		"protocol_hello": vectorHello{Type: "hello", Version: 2, Token: "t0ken"},
		"protocol_request": vectorRequest{
			Type: "request", ID: "req-1",
			Request: vectorPrompt{Command: "prompt", SessionID: "s1", Text: "hi"},
		},
	}
}

type vectorMixed struct {
	ID   string         `cbor:"id"`
	N    int64          `cbor:"n"`
	OK   bool           `cbor:"ok"`
	Tags []any          `cbor:"tags"`
	Meta map[string]any `cbor:"meta"`
}

type vectorHello struct {
	Type    string `cbor:"type"`
	Version int64  `cbor:"version"`
	Token   string `cbor:"token"`
}

type vectorPrompt struct {
	Command   string `cbor:"command"`
	SessionID string `cbor:"sessionId"`
	Text      string `cbor:"text"`
}

type vectorRequest struct {
	Type    string       `cbor:"type"`
	ID      string       `cbor:"id"`
	Request vectorPrompt `cbor:"request"`
}

// mapKeyOrderDivergence names the one vector whose bytes intentionally differ:
// pi emits map entries in JS insertion order, Go sorts them (see encodeMap).
// Every other multi-key map vector happens to be insertion-ordered already, so
// it round-trips byte-identically.
const mapKeyOrderDivergence = "map_key_order"

func TestEncodeMatchesUpstreamBytes(t *testing.T) {
	vectors := loadVectors(t)
	values := goValues()

	for _, vector := range vectors.Encoded {
		if vector.Name == mapKeyOrderDivergence {
			continue
		}
		value, ok := values[vector.Name]
		if !ok {
			t.Errorf("vector %q has no Go value; add it to goValues or document the divergence", vector.Name)
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Error != "" {
				t.Fatalf("upstream failed to encode this vector: %s", vector.Error)
			}
			got, err := Encode(value, nil)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if want := vector.Cbor; hex.EncodeToString(got) != want {
				t.Errorf("bytes diverge from upstream\n got %s\nwant %s", hex.EncodeToString(got), want)
			}
		})
	}
}

// TestEncodeMapKeyOrderDivergence locks the one deliberate byte divergence: Go
// sorts map keys for determinism where pi uses insertion order. It must stay
// interop-safe, i.e. both encodings decode to the same value.
func TestEncodeMapKeyOrderDivergence(t *testing.T) {
	vectors := loadVectors(t)
	var upstreamHex string
	for _, vector := range vectors.Encoded {
		if vector.Name == mapKeyOrderDivergence {
			upstreamHex = vector.Cbor
		}
	}
	if upstreamHex == "" {
		t.Fatal("missing map_key_order vector")
	}

	value := map[string]any{"z": int64(1), "a": int64(2), "m": int64(3)}
	got, err := Encode(value, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if hex.EncodeToString(got) == upstreamHex {
		t.Fatal("expected a byte divergence here; if upstream now sorts keys, drop this test and fold the vector back in")
	}

	upstreamBytes, err := hex.DecodeString(upstreamHex)
	if err != nil {
		t.Fatalf("decode upstream hex: %v", err)
	}
	fromUpstream, err := Decode(upstreamBytes, nil)
	if err != nil {
		t.Fatalf("decode upstream bytes: %v", err)
	}
	fromGo, err := Decode(got, nil)
	if err != nil {
		t.Fatalf("decode go bytes: %v", err)
	}
	if !reflect.DeepEqual(fromUpstream, fromGo) {
		t.Errorf("orderings are not interchangeable\nupstream %#v\n      go %#v", fromUpstream, fromGo)
	}
	if !reflect.DeepEqual(fromGo, value) {
		t.Errorf("round-trip lost data: %#v", fromGo)
	}
}

func TestEncodeRejectsMatchUpstream(t *testing.T) {
	// Cases whose JS form has no Go equivalent: `undefined` and `function` are
	// not expressible, and a Go slice cannot hold a hole. Go's single nil maps
	// to null, which pi accepts — verified by the map_null_kept vector.
	notExpressible := map[string]bool{"undefined_top": true, "function": true, "array_hole_undefined": true}

	rejects := map[string]any{
		"infinity":       math.Inf(1),
		"nan":            math.NaN(),
		"unsafe_int":     float64(maxSafeInteger + 2),
		"lone_surrogate": string([]byte{0xed, 0xa0, 0x80}), // UTF-8 encoding of U+D800
	}

	for _, vector := range loadVectors(t).Rejects {
		if notExpressible[vector.Name] {
			continue
		}
		value, ok := rejects[vector.Name]
		if !ok {
			t.Errorf("reject vector %q unaccounted for", vector.Name)
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Error == "" {
				t.Fatalf("upstream accepted this vector; it is not a reject case")
			}
			_, err := Encode(value, nil)
			if err == nil {
				t.Fatalf("Encode accepted a value upstream rejects with %q", vector.Error)
			}
			if err.Error() != vector.Error {
				t.Errorf("error text diverges\n got %q\nwant %q", err.Error(), vector.Error)
			}
		})
	}
}

func TestDecodeMatchesUpstream(t *testing.T) {
	for _, vector := range loadVectors(t).Decoded {
		t.Run(vector.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatalf("bad vector hex: %v", err)
			}
			got, err := Decode(raw, nil)
			if vector.OK {
				if err != nil {
					t.Fatalf("Decode rejected bytes upstream accepts: %v", err)
				}
				// Upstream reports the value as JSON; compare through the same
				// lens so int64/float64 splits do not register as divergence.
				gotJSON, marshalErr := json.Marshal(got)
				if marshalErr != nil {
					t.Fatalf("marshal decoded value: %v", marshalErr)
				}
				// JS has no distinct -0 in JSON output (JSON.stringify(-0) is
				// "0") while Go's marshaller preserves the sign. The decoded
				// float is the same IEEE value either way, so normalise the
				// rendering rather than the value.
				if string(gotJSON) == "-0" {
					gotJSON = []byte("0")
				}
				if string(gotJSON) != vector.JSON {
					t.Errorf("decoded value diverges\n got %s\nwant %s", gotJSON, vector.JSON)
				}
				return
			}
			if err == nil {
				t.Fatalf("Decode accepted bytes upstream rejects with %q", vector.Error)
			}
			if err.Error() != vector.Error {
				t.Errorf("error text diverges\n got %q\nwant %q", err.Error(), vector.Error)
			}
		})
	}
}

// TestEncodeStructFieldOrder locks the rule that makes protocol messages
// byte-faithful: struct fields go on the wire in declaration order, matching
// the property order of pi's message interfaces, not sorted like maps.
func TestEncodeStructFieldOrder(t *testing.T) {
	type hello struct {
		Type    string `cbor:"type"`
		Version int64  `cbor:"version"`
		Token   string `cbor:"token"`
	}
	got, err := Encode(hello{Type: "hello", Version: 2, Token: "t0ken"}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var want string
	for _, vector := range loadVectors(t).Encoded {
		if vector.Name == "protocol_hello" {
			want = vector.Cbor
		}
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("struct encoding diverges from upstream object\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

func TestEncodeOmitEmptyDropsAbsentOptionalFields(t *testing.T) {
	type item struct {
		A int64   `cbor:"a"`
		B *int64  `cbor:"b,omitempty"`
		C int64   `cbor:"c"`
		D []any   `cbor:"d,omitempty"`
		E *string `cbor:"e,omitempty"`
	}
	got, err := Encode(item{A: 1, C: 3}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var want string
	for _, vector := range loadVectors(t).Encoded {
		if vector.Name == "map_undefined_dropped" {
			want = vector.Cbor
		}
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("omitempty does not match pi's undefined-dropping\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

// boundForm is one Go spelling of a bounds vector's value. Each spelling takes
// its own path through the limit checks, and pi's outcome for the value is
// the outcome for every one of them. A RawItem has no upstream counterpart
// (D56): it is held to what pi does with the same item encoded in place,
// because a relayed span must be bounded exactly as the value would be. Its
// error may say more than pi's, so it need only contain pi's text.
type boundForm struct {
	name  string
	value any
	raw   bool
}

type boundsMapDepth struct {
	A struct {
		B int64 `cbor:"b"`
	} `cbor:"a"`
}

type boundsMap struct {
	A int64 `cbor:"a"`
	B int64 `cbor:"b"`
}

// boundForms spells a bounds vector's value every way the encoder checks it,
// slicing any nested RawItem out of pi's own bytes for the value.
func boundForms(t *testing.T, name string, item RawItem) []boundForm {
	t.Helper()
	family := name
	for _, suffix := range []string{"_at_limit", "_past_limit", "_over_limit"} {
		family = strings.TrimSuffix(family, suffix)
	}
	whole := boundForm{"whole RawItem", item, true}
	switch family {
	case "depth":
		return []boundForm{
			{"value", []any{[]any{int64(1)}}, false},
			whole,
			{"nested RawItem", []any{item[1:]}, true},
		}
	case "map_depth":
		var s boundsMapDepth
		s.A.B = 1
		return []boundForm{
			{"value", map[string]any{"a": map[string]any{"b": int64(1)}}, false},
			{"struct", s, false},
			{"ordered object", OrderedObject{{Key: "a", Value: OrderedObject{{Key: "b", Value: int64(1)}}}}, false},
			whole,
			{"nested RawItem", map[string]any{"a": item[3:]}, true},
		}
	case "text":
		return []boundForm{{"value", string(item[1:]), false}, whole}
	case "bytes":
		return []boundForm{{"value", []byte(item[1:]), false}, whole}
	case "nested_length":
		return []boundForm{
			{"value", []any{[]any{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}}, false},
			whole,
			{"nested RawItem", []any{item[1:]}, true},
		}
	case "array":
		return []boundForm{{"value", []any{int64(1), int64(2)}, false}, whole}
	case "map":
		return []boundForm{
			{"value", map[string]any{"a": int64(1), "b": int64(2)}, false},
			{"struct", boundsMap{A: 1, B: 2}, false},
			{"ordered object", OrderedObject{{Key: "a", Value: int64(1)}, {Key: "b", Value: int64(2)}}, false},
			whole,
		}
	}
	t.Fatalf("bounds vector %q has no Go forms; add them to boundForms", name)
	return nil
}

// TestBoundsMatchUpstream: every limit admits a value exactly at it and
// refuses one a unit past it, in the encoder and the decoder, with pi's text —
// and so does every spelling of the value that the port checks separately:
// structs, OrderedObjects, and RawItems whole or nested. DecodeRaw capturing a
// top-level entry must agree with Decode.
func TestBoundsMatchUpstream(t *testing.T) {
	vectors := loadVectors(t).Bounds
	if len(vectors) == 0 {
		t.Fatal("no bounds vectors: regenerate testdata/upstream_vectors.json from gen-vectors.ts")
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			if (vector.Encoded == "") == (vector.EncodeError == "") || vector.DecodeOK == (vector.DecodeError != "") {
				t.Fatalf("malformed vector: %+v", vector)
			}
			opts := &Options{}
			for name, limit := range vector.Options {
				switch name {
				case "maxByteLength":
					opts.MaxByteLength = ptr(limit)
				case "maxContainerLength":
					opts.MaxContainerLength = ptr(limit)
				case "maxDepth":
					opts.MaxDepth = ptr(limit)
				default:
					t.Fatalf("unknown option %q", name)
				}
			}
			item, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatalf("bad vector hex: %v", err)
			}

			for _, form := range boundForms(t, vector.Name, item) {
				got, err := Encode(form.value, opts)
				switch {
				case vector.EncodeError == "" && err != nil:
					t.Errorf("Encode(%s) refused a value pi encodes as %s: %v", form.name, vector.Encoded, err)
				case vector.EncodeError == "" && hex.EncodeToString(got) != vector.Encoded:
					t.Errorf("Encode(%s) = %x, pi encodes %s", form.name, got, vector.Encoded)
				case vector.EncodeError != "" && err == nil:
					t.Errorf("Encode(%s) = %x, pi refuses it: %s", form.name, got, vector.EncodeError)
				case vector.EncodeError != "" && form.raw && !strings.Contains(err.Error(), vector.EncodeError):
					t.Errorf("Encode(%s) error %q does not carry pi's %q", form.name, err, vector.EncodeError)
				case vector.EncodeError != "" && !form.raw && err.Error() != vector.EncodeError:
					t.Errorf("Encode(%s) error\n got %q\nwant %q", form.name, err, vector.EncodeError)
				}
			}

			var captures [][]string
			switch {
			case strings.HasPrefix(vector.Name, "map_depth_"):
				captures = [][]string{nil, {"a"}}
			case strings.HasPrefix(vector.Name, "map_"):
				captures = [][]string{nil, {"a", "b"}}
			default:
				captures = [][]string{nil}
			}
			for _, keys := range captures {
				got, err := DecodeRaw(item, opts, keys...)
				switch {
				case vector.DecodeOK && err != nil:
					t.Errorf("DecodeRaw capturing %q refused bytes pi decodes: %v", keys, err)
				case vector.DecodeOK && len(keys) == 0:
					if want, _ := Decode(item, nil); !reflect.DeepEqual(got, want) {
						t.Errorf("decoded %#v under the limit, %#v without it", got, want)
					}
				case !vector.DecodeOK && err == nil:
					t.Errorf("DecodeRaw capturing %q = %#v, pi refuses it: %s", keys, got, vector.DecodeError)
				case !vector.DecodeOK && err.Error() != vector.DecodeError:
					t.Errorf("DecodeRaw capturing %q error\n got %q\nwant %q", keys, err, vector.DecodeError)
				}
			}
		})
	}
}

// TestEncodeZeroValuesAreNotOmitted guards the narrower-than-encoding/json
// omitempty rule: 0, "" and false are real protocol values and must survive.
func TestEncodeZeroValuesAreNotOmitted(t *testing.T) {
	type item struct {
		N int64  `cbor:"n,omitempty"`
		S string `cbor:"s,omitempty"`
		B bool   `cbor:"b,omitempty"`
	}
	got, err := Encode(item{}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(got, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := map[string]any{"n": int64(0), "s": "", "b": false}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("zero values were dropped\n got %#v\nwant %#v", decoded, want)
	}
}
