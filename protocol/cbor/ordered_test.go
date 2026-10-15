package cbor

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestEncodeOrderedObjectKeepsAuthoredOrder: an OrderedObject is a slice, so
// the encoder has to recognise it as an object and emit its keys where the
// caller put them — including inside another OrderedObject and inside an array,
// which is where a Go map would otherwise re-sort them.
func TestEncodeOrderedObjectKeepsAuthoredOrder(t *testing.T) {
	value := OrderedObject{
		{Key: "path", Value: "/tmp"},
		{Key: "depth", Value: float64(1)},
		{Key: "filters", Value: []any{OrderedObject{{Key: "name", Value: "go"}, {Key: "enabled", Value: true}}}},
		{Key: "nested", Value: OrderedObject{{Key: "zeta", Value: float64(1)}, {Key: "alpha", Value: float64(2)}}},
	}
	encoded, err := Encode(value, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// a4 is a 4-entry map, so the value is an object rather than an array, and
	// the keys follow in the order given.
	const want = "a4" +
		"6470617468" + "642f746d70" + // "path": "/tmp"
		"6564657074 68" + "01" + // "depth": 1
		"6766696c7465727381" + "a2" + "646e616d65" + "62676f" + "67656e61626c6564" + "f5" +
		"666e6573746564" + "a2" + "647a657461" + "01" + "65616c706861" + "02"
	if got := hex.EncodeToString(encoded); got != strings.ReplaceAll(want, " ", "") {
		t.Errorf("encoding = %s, want %s", got, strings.ReplaceAll(want, " ", ""))
	}

	decoded, err := Decode(encoded, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	object, ok := decoded.(map[string]any)
	if !ok || len(object) != 4 || object["path"] != "/tmp" {
		t.Fatalf("decoded = %#v, want the same object", decoded)
	}
}

// TestEncodeOrderedObjectRejectsDuplicateKey: a repeated key encodes a map this
// package's own Decode refuses, so it would be unreadable by every peer.
func TestEncodeOrderedObjectRejectsDuplicateKey(t *testing.T) {
	_, err := Encode(OrderedObject{{Key: "a", Value: 1}, {Key: "a", Value: 2}}, nil)
	if err == nil || !strings.Contains(err.Error(), "must not repeat a key") {
		t.Fatalf("err = %v, want a duplicate-key rejection", err)
	}
}

// TestEncodeOrderedObjectRejectsCycle: cycle detection has to reach through an
// ordered object too, or a self-referential value encodes until it runs out of
// depth or memory.
func TestEncodeOrderedObjectRejectsCycle(t *testing.T) {
	cycle := make(OrderedObject, 1)
	cycle[0] = OrderedField{Key: "self", Value: cycle}
	if _, err := Encode(cycle, nil); err == nil || !strings.Contains(err.Error(), "must not contain cycles") {
		t.Fatalf("err = %v, want a cycle rejection", err)
	}
}
