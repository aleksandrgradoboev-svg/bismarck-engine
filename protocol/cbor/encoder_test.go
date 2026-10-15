package cbor

import (
	"reflect"
	"strings"
	"testing"
)

// TestEncodeRejectsEmbeddedStruct: Encode is exported, so a struct layout it
// cannot represent has to be an error rather than a silently short message. An
// embedded struct used to vanish from the wire entirely.
func TestEncodeRejectsEmbeddedStruct(t *testing.T) {
	// Both spellings matter: encoding/json flattens an embedded field whether
	// the embedded type is exported or not, so both are layouts a caller could
	// reasonably expect to work.
	type hidden struct {
		X string `cbor:"x"`
	}
	type Visible struct {
		X string `cbor:"x"`
	}

	t.Run("unexported_embedded", func(t *testing.T) {
		type outer struct {
			hidden
			Y string `cbor:"y"`
		}
		_, err := Encode(outer{hidden: hidden{X: "a"}, Y: "b"}, nil)
		if err == nil {
			t.Fatal("Encode accepted a struct whose embedded field it cannot encode")
		}
		if !strings.Contains(err.Error(), "embedded structs are not flattened") {
			t.Errorf("error does not explain the layout problem: %q", err)
		}
		if !strings.Contains(err.Error(), "cbor tag") {
			t.Errorf("error does not say how to resolve it: %q", err)
		}
	})

	t.Run("exported_embedded", func(t *testing.T) {
		type outer struct {
			Visible
			Y string `cbor:"y"`
		}
		if _, err := Encode(outer{Visible: Visible{X: "a"}, Y: "b"}, nil); err == nil {
			t.Fatal("Encode accepted an embedded struct")
		}
	})

	t.Run("embedded_pointer", func(t *testing.T) {
		type outer struct {
			*Visible
			Y string `cbor:"y"`
		}
		if _, err := Encode(outer{Visible: &Visible{X: "a"}, Y: "b"}, nil); err == nil {
			t.Fatal("Encode accepted an embedded struct pointer")
		}
	})

	// The two documented escapes must both work: a tag names the field, and
	// cbor:"-" drops it deliberately rather than silently. The tag case uses the
	// unexported embedded type on purpose — that is the layout whose value is
	// only reachable reflectively, so it is the one that would panic if the
	// escape did not actually work.
	t.Run("tagged_embedded_is_a_named_field", func(t *testing.T) {
		type outer struct {
			hidden `cbor:"nested"`
			Y      string `cbor:"y"`
		}
		got, err := Encode(outer{hidden: hidden{X: "a"}, Y: "b"}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, err := Decode(got, nil)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		want := any(map[string]any{"nested": map[string]any{"x": "a"}, "y": "b"})
		if !reflect.DeepEqual(decoded, want) {
			t.Errorf("encoded %#v, want %#v", decoded, want)
		}
	})

	t.Run("ignored_embedded", func(t *testing.T) {
		type outer struct {
			Visible `cbor:"-"`
			Y       string `cbor:"y"`
		}
		got, err := Encode(outer{Visible: Visible{X: "a"}, Y: "b"}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, err := Decode(got, nil)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !reflect.DeepEqual(decoded, any(map[string]any{"y": "b"})) {
			t.Errorf("encoded %#v, want only the tagged field", decoded)
		}
	})
}

// TestEncodeRejectsDuplicateWireNames: two fields sharing a cbor name produce a
// map with a repeated key, which this package's own Decode refuses — so the
// bytes are unreadable by every peer, including us.
func TestEncodeRejectsDuplicateWireNames(t *testing.T) {
	type duplicate struct {
		A string `cbor:"x"`
		B string `cbor:"x"`
	}
	got, err := Encode(duplicate{A: "1", B: "2"}, nil)
	if err == nil {
		t.Fatalf("Encode accepted a duplicate wire name and produced %x", got)
	}
	if !strings.Contains(err.Error(), `both encode to the property "x"`) {
		t.Errorf("error does not name the collision: %q", err)
	}
	if !strings.Contains(err.Error(), "rename one") {
		t.Errorf("error does not say how to resolve it: %q", err)
	}

	// The untagged form collides just as invisibly: an explicit tag on one field
	// can shadow another field's Go name.
	type shadowed struct {
		X string
		B string `cbor:"X"`
	}
	if _, err := Encode(shadowed{X: "1", B: "2"}, nil); err == nil {
		t.Error("Encode accepted a tag colliding with an untagged field name")
	}
}

// TestEncodeRejectsBadLayoutOnEveryCall guards the cache: a layout error must
// not be resolved once and then forgotten.
func TestEncodeRejectsBadLayoutOnEveryCall(t *testing.T) {
	type duplicate struct {
		A string `cbor:"x"`
		B string `cbor:"x"`
	}
	first, firstErr := Encode(duplicate{}, nil)
	second, secondErr := Encode(duplicate{}, nil)
	if firstErr == nil || secondErr == nil {
		t.Fatalf("Encode accepted a duplicate wire name: %x %x", first, second)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Errorf("cached layout error diverges\nfirst  %q\nsecond %q", firstErr, secondErr)
	}
}
