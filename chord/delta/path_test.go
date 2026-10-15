package delta

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// RESERVED_SEGMENTS at 64eeb82a4: the three names that reach the prototype
// chain on a JavaScript replica.
func TestReservedSegments(t *testing.T) {
	want := map[string]bool{"__proto__": true, "constructor": true, "prototype": true}
	if !reflect.DeepEqual(ReservedSegments, want) {
		t.Fatalf("ReservedSegments = %v, want %v", ReservedSegments, want)
	}
}

// assertSafePath: reserved keys and negative indices are unsafe; everything
// else, including keys that merely look like indices, passes.
func TestPathValidate(t *testing.T) {
	for _, p := range []Path{nil, {}, {Key("a")}, {Key("a"), Index(0)}, {Key("7")}, {Key("__proto"), Key("constructor_")}, {Index(4294967290)}} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s.Validate() = %v, want nil", p, err)
		}
	}
	cases := []struct {
		path Path
		seg  Seg
	}{
		{Path{Key("__proto__")}, Key("__proto__")},
		{Path{Key("a"), Key("constructor"), Key("b")}, Key("constructor")},
		{Path{Key("prototype")}, Key("prototype")},
		{Path{Key("xs"), Index(-1)}, Index(-1)},
	}
	for _, tc := range cases {
		err := tc.path.Validate()
		var unsafe *UnsafePathError
		if !errors.As(err, &unsafe) {
			t.Errorf("%s.Validate() = %v, want *UnsafePathError", tc.path, err)
			continue
		}
		if unsafe.Segment != tc.seg {
			t.Errorf("%s.Validate() flagged %#v, want %#v", tc.path, unsafe.Segment, tc.seg)
		}
	}
}

func TestUnsafePathErrorMessage(t *testing.T) {
	// delta.test.ts matches /unsafe path/ on the thrown message.
	err := (&UnsafePathError{Segment: Key("__proto__")}).Error()
	if want := "unsafe path segment: __proto__"; len(err) < len(want) || err[:len(want)] != want {
		t.Errorf("Error() = %q, want prefix %q", err, want)
	}
}

// PathError text is upstream's `unresolvable path: ${JSON.stringify(path)}`;
// a decoder reports an unknown id as the bare number.
func TestPathErrorMessage(t *testing.T) {
	cases := []struct {
		ref  PathRef
		want string
	}{
		{Path{Key("a"), Index(1)}, `unresolvable path: ["a",1]`},
		{Path{}, `unresolvable path: []`},
		{Path(nil), `unresolvable path: []`},
		{PathID(0), `unresolvable path: 0`},
	}
	for _, tc := range cases {
		if got := (&PathError{Ref: tc.ref}).Error(); got != tc.want {
			t.Errorf("PathError{%#v}.Error() = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestPathJSON(t *testing.T) {
	cases := []struct {
		path Path
		json string
	}{
		{Path{Key("operation"), Key("message"), Key("content"), Index(0), Key("text")}, `["operation","message","content",0,"text"]`},
		{Path{}, `[]`},
		{Path(nil), `[]`},
		{Path{Key("a\x00b")}, "[\"a\\u0000b\"]"},
		{Path{Key("7"), Index(7)}, `["7",7]`},
	}
	for _, tc := range cases {
		got, err := json.Marshal(tc.path)
		if err != nil {
			t.Errorf("%#v: %v", tc.path, err)
			continue
		}
		if string(got) != tc.json {
			t.Errorf("%#v marshals to %s, want %s", tc.path, got, tc.json)
		}
		if s := tc.path.String(); s != tc.json {
			t.Errorf("%#v.String() = %s, want %s", tc.path, s, tc.json)
		}
		var back Path
		if err := json.Unmarshal(got, &back); err != nil {
			t.Errorf("Unmarshal(%s): %v", got, err)
			continue
		}
		want := tc.path
		if want == nil {
			want = Path{}
		}
		if !reflect.DeepEqual(back, want) {
			t.Errorf("Unmarshal(%s) = %#v, want %#v", got, back, want)
		}
	}
	// Segments are keys or indices; anything else is not a path.
	for _, bad := range []string{`"a"`, `[null]`, `[true]`, `[{}]`, `[[0]]`, `[1.5]`, `[-0.5]`} {
		var p Path
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("Unmarshal(%s) accepted as %#v", bad, p)
		}
	}
	// A negative index decodes (it is an Index) and fails Validate, matching
	// upstream where the number is a Seg and assertSafePath rejects it.
	var neg Path
	if err := json.Unmarshal([]byte(`[-1]`), &neg); err != nil {
		t.Fatalf("Unmarshal([-1]): %v", err)
	}
	if !reflect.DeepEqual(neg, Path{Index(-1)}) || neg.Validate() == nil {
		t.Errorf("Unmarshal([-1]) = %#v (Validate: %v)", neg, neg.Validate())
	}
	// So does an integral number past Number.MAX_SAFE_INTEGER: it is an Index
	// (upstream: a Seg), and Validate refuses it as an address no double can
	// spell exactly.
	var huge Path
	if err := json.Unmarshal([]byte(`[1e300]`), &huge); err != nil {
		t.Fatalf("Unmarshal([1e300]): %v", err)
	}
	if huge.Validate() == nil {
		t.Errorf("Unmarshal([1e300]) = %#v validated", huge)
	}
	// Paths nested in a struct decode through the same method.
	var holder struct{ P Path }
	if err := json.Unmarshal([]byte(`{"P":["a",0]}`), &holder); err != nil || !reflect.DeepEqual(holder.P, Path{Key("a"), Index(0)}) {
		t.Errorf("nested path decode = %#v, %v", holder.P, err)
	}
}

// delta.test.ts codec → "does not collide paths containing null characters":
// the codec keys its dictionary on the JSON form, which String() provides.
func TestPathStringDoesNotCollideOnNulCharacters(t *testing.T) {
	first := Path{Key("a\x00b")}
	second := Path{Key("a"), Key("b")}
	if first.String() == second.String() {
		t.Fatalf("%q collides with %q", first.String(), second.String())
	}
}

func TestSegString(t *testing.T) {
	if got := Key("text").String(); got != "text" {
		t.Errorf("Key.String() = %q", got)
	}
	if got := Index(12).String(); got != "12" {
		t.Errorf("Index.String() = %q", got)
	}
}
