package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// jsonParseOracle is node's JSON.parse over texts chosen to reach every error
// V8's parser reports, and texts it accepts (jstext's capture).
const jsonParseOracle = "../internal/jstext/testdata/json-parse-errors-node.json"

// parseOrdered accepts exactly the texts JSON.parse accepts: node's verdict on
// each of the oracle's rows.
func TestParseOrderedAcceptsWhatJSONParseAccepts(t *testing.T) {
	data, err := os.ReadFile(jsonParseOracle)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Rows [][2]*string `json:"rows"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("%s: %v", jsonParseOracle, err)
	}
	if len(oracle.Rows) < 1000 {
		t.Fatalf("%s has %d rows; the capture writes over 2000", jsonParseOracle, len(oracle.Rows))
	}
	for _, row := range oracle.Rows {
		text, threw := *row[0], row[1]
		_, err := parseOrdered([]byte(text))
		if (err == nil) != (threw == nil) {
			t.Errorf("parseOrdered(%q): error %v, JSON.parse threw %v", text, err, threw != nil)
		}
	}
}

// FuzzParseOrdered holds parseOrdered to the decoder it replaced — the
// json.Decoder token walk below, which reads strings and numbers with
// encoding/json's own code — on acceptance, every value, and key order, and
// to json.Valid on acceptance.
func FuzzParseOrdered(f *testing.F) {
	for _, seed := range []string{
		`{"b":1,"2":"x","1":{"4":0,"3":0},"a":[1,-0,1e400,-1e-400,0.5,1E2,-12345678901234567]}`,
		`{"a":1,"a":2,"0":3,"0":4}`, `[]`, `{}`, `[[[{}]]]`, ` "x" `, `null`, `true`, `false`, `-0`, `01`, `1.`, `.5`, `-`,
		`"\ud83d\ude00\ud83d\u0041\ude00\u00e9\"\\\/\b\f\n\r\t"`, "\"a\xffb\xe2\x82\"", `"\x"`, `"\u12"`, "\"a\tb\"",
		`{"a" : 1 , "b" :[ 1 , 2 ] }`, `{"a":1,}`, `[1,]`, `{"a"}`, `{1:2}`, `[1 2]`, `"abc`, `{"a":1}x`,
		`123456789012345678901234567890`, `1.7976931348623159e308`, `4.9e-324`, `9007199254740993`, `-9007199254740993`,
		// Past 16 keys the parser finds a repeated key through a map.
		`{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k3":"again","k17":17,"k16":"again","2":2}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := parseOrdered(data)
		want, wantErr := referenceOrderedValue(data)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("parseOrdered(%q) error %v, reference error %v", data, err, wantErr)
		}
		if valid := json.Valid(data); (err == nil) != valid {
			t.Fatalf("parseOrdered(%q) error %v, json.Valid %v", data, err, valid)
		}
		if err != nil {
			return
		}
		if !sameOrderedValue(got, want) {
			t.Fatalf("parseOrdered(%q) = %s, reference %s", data, dumpOrdered(got), dumpOrdered(want))
		}
	})
}

// referenceOrderedValue is the json.Decoder token walk DecodeOrderedValue
// used before parseOrdered: encoding/json reads each string and number, and
// the walk builds the objects in JSON.parse's key order.
func referenceOrderedValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := referenceValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing content")
	}
	return v, nil
}

func referenceValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok := tok.(type) {
	case json.Number:
		return jstext.Number(tok), nil
	case json.Delim:
		switch tok {
		case '[':
			arr := []any{}
			for dec.More() {
				e, err := referenceValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, e)
			}
			_, err := dec.Token()
			return arr, err
		case '{':
			obj := OrderedObject{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				value, err := referenceValue(dec)
				if err != nil {
					return nil, err
				}
				if i := obj.indexOf(key.(string)); i >= 0 {
					obj[i].Value = value
				} else {
					obj = append(obj, OrderedField{Key: key.(string), Value: value})
				}
			}
			sort.SliceStable(obj, func(i, j int) bool {
				a, aIndex := jstext.ArrayIndexKey(obj[i].Key)
				b, bIndex := jstext.ArrayIndexKey(obj[j].Key)
				return aIndex && (!bIndex || a < b)
			})
			_, err := dec.Token()
			return obj, err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", tok)
	}
	return tok, nil
}

// sameOrderedValue is deep equality that tells -0 from 0 and holds key order.
func sameOrderedValue(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if !sameOrderedValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case OrderedObject:
		y, ok := b.(OrderedObject)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !sameOrderedValue(x[i].Value, y[i].Value) {
				return false
			}
		}
		return true
	}
	return a == b
}

func dumpOrdered(v any) string {
	var b strings.Builder
	var dump func(any)
	dump = func(v any) {
		switch x := v.(type) {
		case OrderedObject:
			b.WriteString("{")
			for i, f := range x {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "%q:", f.Key)
				dump(f.Value)
			}
			b.WriteString("}")
		case []any:
			b.WriteString("[")
			for i, e := range x {
				if i > 0 {
					b.WriteString(",")
				}
				dump(e)
			}
			b.WriteString("]")
		case float64:
			fmt.Fprintf(&b, "%v(%x)", x, math.Float64bits(x))
		default:
			fmt.Fprintf(&b, "%#v", x)
		}
	}
	dump(v)
	return b.String()
}

// Nesting past encoding/json's limit is refused rather than read a stack
// frame at a time.
func TestParseOrderedRefusesDeepNesting(t *testing.T) {
	if _, err := parseOrdered([]byte(strings.Repeat("[", maxOrderedDepth) + strings.Repeat("]", maxOrderedDepth))); err != nil {
		t.Fatalf("%d levels: %v", maxOrderedDepth, err)
	}
	if _, err := parseOrdered([]byte(strings.Repeat("[", maxOrderedDepth+1) + strings.Repeat("]", maxOrderedDepth+1))); err == nil {
		t.Fatalf("%d levels parsed", maxOrderedDepth+1)
	}
}

// A number past float64's range reads as JSON.parse reads it, ±Infinity, in
// both of DecodeOrderedObject's forms, and a tool call holding one — as a
// session edited by hand might — decodes and replays it as JSON.stringify
// writes Infinity: null. json.Unmarshal refuses the number. The wants are
// node's JSON.stringify(JSON.parse(text)).
func TestDecodeOrderedObjectReadsNumbersAsJSONParse(t *testing.T) {
	plain, ordered, err := DecodeOrderedObject([]byte(`{"a":1e400,"b":[-1e400,{"c":1e400}],"d":1e-400}`))
	if err != nil {
		t.Fatal(err)
	}
	if text, err := jstext.Stringify(ordered); err != nil || text != `{"a":null,"b":[null,{"c":null}],"d":0}` {
		t.Fatalf("ordered = %s, %v; JSON.stringify(JSON.parse(...)) is {\"a\":null,\"b\":[null,{\"c\":null}],\"d\":0}", text, err)
	}
	if a, _ := plain["a"].(float64); !math.IsInf(a, 1) {
		t.Fatalf("plain a = %v, want +Inf", plain["a"])
	}
	var call ToolCall
	if err := json.Unmarshal([]byte(`{"id":"c","name":"n","arguments":{"n":1e400,"m":1}}`), &call); err != nil {
		t.Fatalf("a tool call whose arguments hold 1e400: %v", err)
	}
	raw, err := json.Marshal(call)
	if err != nil || string(raw) != `{"id":"c","name":"n","arguments":{"n":null,"m":1}}` {
		t.Fatalf("tool call round trip = %s, %v; JSON.stringify(JSON.parse(...)) is {\"id\":\"c\",\"name\":\"n\",\"arguments\":{\"n\":null,\"m\":1}}", raw, err)
	}
}
