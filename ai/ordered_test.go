package ai

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

func TestDecodeOrderedValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantJSON is JSON.stringify of pi's JSON.parse(in), as node writes
		// it: key order is the wire's, which a map[string]any would lose,
		// except that array-index keys come first, ascending, and a number
		// past float64's range is Infinity, which it writes null.
		wantJSON string
		want     any
	}{
		{name: "object keeps JSON.parse order", in: `{"type":"a","id":1,"choices":[]}`, wantJSON: `{"type":"a","id":1,"choices":[]}`},
		{name: "nested objects keep order", in: `{"z":{"b":1,"a":[{"d":true,"c":null}]},"y":"s"}`, wantJSON: `{"z":{"b":1,"a":[{"d":true,"c":null}]},"y":"s"}`},
		{name: "repeated key keeps first slot, last value", in: `{"a":1,"b":2,"a":3}`, wantJSON: `{"a":3,"b":2}`},
		{name: "array", in: `[3,{"b":1,"a":2}]`, wantJSON: `[3,{"b":1,"a":2}]`},
		{
			name:     "array-index keys first, ascending",
			in:       `{"b":1,"2":"x","1":"y","-1":0,"01":2,"4294967295":3,"4294967294":4,"id":"k"}`,
			wantJSON: `{"1":"y","2":"x","4294967294":4,"b":1,"-1":0,"01":2,"4294967295":3,"id":"k"}`,
		},
		{name: "repeated array-index key keeps its place", in: `{"a":1,"3":1,"a":2,"0":0,"3":"again"}`, wantJSON: `{"0":0,"3":"again","a":2}`},
		{
			name:     "only canonical indexes are hoisted, at any depth",
			in:       `{"z":{"10":1,"9":2,"x":3},"1.0":1,"1e1":2,"00":3,"0":4," 1":5,"+1":6}`,
			wantJSON: `{"0":4,"z":{"9":2,"10":1,"x":3},"1.0":1,"1e1":2,"00":3," 1":5,"+1":6}`,
		},
		{name: "index keys inside arrays", in: `[{"2":1,"1":2},{"4294967296":1,"4294967294":2}]`, wantJSON: `[{"1":2,"2":1},{"4294967294":2,"4294967296":1}]`},
		{name: "empty key is not an index", in: `{"":1,"0":2}`, wantJSON: `{"0":2,"":1}`},
		{
			name:     "numbers past float64's range are ±Inf, written null",
			in:       `{"a":1e400,"b":-1e400,"c":1e-400,"d":[1e400,-0,{"e":-1e400}]}`,
			wantJSON: `{"a":null,"b":null,"c":0,"d":[null,0,{"e":null}]}`,
		},
		{name: "a scalar past float64's range", in: `1e400`, want: math.Inf(1)},
		{name: "a negative scalar past float64's range", in: `-1e400`, want: math.Inf(-1)},
		{name: "rounds past the largest float64", in: `1.7976931348623159e308`, want: math.Inf(1)},
		{name: "the largest float64", in: `1.7976931348623157e308`, want: math.MaxFloat64},
		{name: "null", in: `null`, want: nil},
		{name: "number", in: ` 5 `, want: float64(5)},
		{name: "string", in: `"x"`, want: "x"},
		{name: "bool", in: `false`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeOrderedValue([]byte(tc.in))
			if err != nil {
				t.Fatalf("DecodeOrderedValue(%s): %v", tc.in, err)
			}
			if tc.wantJSON != "" {
				b, err := jstext.Stringify(got)
				if err != nil {
					t.Fatal(err)
				}
				if b != tc.wantJSON {
					t.Fatalf("stringify = %s, want %s", b, tc.wantJSON)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// DecodeOrderedObject's twin lists the keys as JSON.parse's object does, and
// its map holds the same members.
func TestDecodeOrderedObjectOrdersKeysAsJSONParse(t *testing.T) {
	plain, ordered, err := DecodeOrderedObject([]byte(`{"b":1,"2":"x","1":{"4":0,"3":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"1":{"3":0,"4":0},"2":"x","b":1}`; string(b) != want {
		t.Fatalf("ordered = %s, want %s (node's JSON.stringify(JSON.parse(...)))", b, want)
	}
	if !reflect.DeepEqual(plain, ordered.Plain()) {
		t.Fatalf("map %v does not hold the ordered twin's members %v", plain, ordered.Plain())
	}
}

// The ±Inf JSON.parse reads a number past float64's range as is kept as a
// number, not only written null, and OrderedObject writes it null wherever
// it sits, as JSON.stringify does, where encoding/json refuses it.
func TestDecodeOrderedValueNumbersPastRangeAreInfinite(t *testing.T) {
	got, err := DecodeOrderedValue([]byte(`{"a":1e400,"b":[-1e400]}`))
	if err != nil {
		t.Fatal(err)
	}
	obj := got.(OrderedObject)
	if obj[0].Value != math.Inf(1) || obj[1].Value.([]any)[0] != math.Inf(-1) {
		t.Fatalf("decoded %#v, want +Inf and [-Inf]", obj)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":null,"b":[null]}`; string(b) != want {
		t.Fatalf("json.Marshal = %s, want %s", b, want)
	}
}

func TestDecodeOrderedValueObjectIsOrdered(t *testing.T) {
	got, err := DecodeOrderedValue([]byte(`{"b":1,"a":{"d":2,"c":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := got.(OrderedObject)
	if !ok {
		t.Fatalf("top-level object decoded as %T, want OrderedObject", got)
	}
	if _, ok := obj[1].Value.(OrderedObject); !ok {
		t.Fatalf("nested object decoded as %T, want OrderedObject", obj[1].Value)
	}
}

func TestDecodeOrderedValueRejectsIncompleteOrTrailing(t *testing.T) {
	for _, in := range []string{``, `{"a":1`, `{"a":1} {}`, `1 2`, `nope`} {
		if v, err := DecodeOrderedValue([]byte(in)); err == nil {
			t.Errorf("DecodeOrderedValue(%q) = %#v, want an error", in, v)
		}
	}
}

// TestOrderedObjectMarshalsNumbersAsJSONStringify requires an OrderedObject
// to write its numbers — in the object, in an array in it, in an object in
// that — as JSON.stringify writes them: negative zero as 0 (json.Marshal
// writes -0) and a number that is not finite as null. want is node's
// JSON.stringify({a:-0, b:[-0, 1.5, Infinity], c:{d:-0, e:-Infinity}, f:NaN}).
func TestOrderedObjectMarshalsNumbersAsJSONStringify(t *testing.T) {
	const want = `{"a":0,"b":[0,1.5,null],"c":{"d":0,"e":null},"f":null}`
	o := OrderedObject{
		{Key: "a", Value: math.Copysign(0, -1)},
		{Key: "b", Value: []any{math.Copysign(0, -1), 1.5, math.Inf(1)}},
		{Key: "c", Value: OrderedObject{{Key: "d", Value: math.Copysign(0, -1)}, {Key: "e", Value: math.Inf(-1)}}},
		{Key: "f", Value: math.NaN()},
	}
	got, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

// TestOrderedObjectUnmarshalKeepsJSONParseOrder requires an OrderedObject
// read back from JSON to list its keys as JSON.parse's object does, at every
// depth, so what it marshals again is what it read — and a null to leave it
// nil, while anything that is not an object fails.
func TestOrderedObjectUnmarshalKeepsJSONParseOrder(t *testing.T) {
	var holder struct {
		O OrderedObject `json:"o"`
	}
	const in = `{"z":1,"1":{"b":[{"y":2,"x":1}],"a":1e2},"a":"s"}`
	if err := json.Unmarshal([]byte(`{"o":`+in+`}`), &holder); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(holder.O)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"1":{"b":[{"y":2,"x":1}],"a":100},"z":1,"a":"s"}`; string(got) != want {
		t.Fatalf("read back = %s, want %s", got, want)
	}
	if v, ok := holder.O.Get("a"); !ok || v != "s" {
		t.Errorf(`Get("a") = %v, %v; want "s", true`, v, ok)
	}
	if v, ok := holder.O.Get("missing"); ok || v != nil {
		t.Errorf(`Get("missing") = %v, %v; want nil, false`, v, ok)
	}
	holder.O = nil
	if err := json.Unmarshal([]byte(`{"o":null}`), &holder); err != nil || holder.O != nil {
		t.Errorf("null read as %#v, %v; want nil", holder.O, err)
	}
	if err := json.Unmarshal([]byte(`{"o":[1]}`), &holder); err == nil {
		t.Error("an array read as an OrderedObject, want an error")
	}
}
