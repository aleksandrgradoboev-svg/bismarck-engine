package chord

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// The value types of types.go: each marshals to exactly the JSON pi's
// provider and consumer build (provider.ts #snapshotInstance, #emit and the
// consumer's invoke literal at 64eeb82a4), and each validates the rules
// wire.ts imposes when the value arrives from a peer. The parsers' own tests
// are in wire_test.go.

// Every container marshals with its discriminator first and its keys in pi's
// literal order, omits a nil address, and writes a nil slice as []. The
// type's own MarshalJSON is checked, as delta's goldens are, because
// json.Marshal re-escapes <, > and & in whatever a Marshaler returns; an
// Encoder with SetEscapeHTML(false) passes these bytes through verbatim.
func TestTypeJSONGoldens(t *testing.T) {
	address := ServiceInstanceAddress{Key: "k", Generation: 1}
	set := delta.Set{Path: delta.Path{delta.Key("value")}, Value: 1}
	cases := []struct {
		value json.Marshaler
		want  string
	}{
		{MethodSnapshot[delta.Op]{Name: "list"}, `{"name":"list","kind":"method"}`},
		{StateSnapshot[delta.Op]{Name: "s"}, `{"name":"s","kind":"state","sequence":0,"ops":[]}`},
		{StateSnapshot[delta.WireOp]{Name: "s", Sequence: 3, Ops: []delta.WireOp{delta.WireSet{Ref: delta.PathID(0), Value: "x"}}},
			`{"name":"s","kind":"state","sequence":3,"ops":[["s",0,"x"]]}`},
		{ServiceInstanceSnapshot[delta.Op]{}, `{"members":[]}`},
		{ServiceInstanceSnapshot[delta.Op]{Instance: &address, Members: []ServiceMemberSnapshot[delta.Op]{MethodSnapshot[delta.Op]{Name: "m"}}},
			`{"instance":{"key":"k","generation":1},"members":[{"name":"m","kind":"method"}]}`},
		{ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Keyed}, `{"serviceId":"s","mode":"keyed","instances":[]}`},
		{StateUpdate[delta.Op]{Member: "m", Sequence: 1}, `{"type":"state","member":"m","sequence":1,"ops":[]}`},
		{StateUpdate[delta.Op]{Instance: &address, Member: "m", Sequence: 2, Ops: []delta.Op{set}},
			`{"type":"state","instance":{"key":"k","generation":1},"member":"m","sequence":2,"ops":[["s",["value"],1]]}`},
		{ResetUpdate[delta.Op]{Snapshot: ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Singleton, Instances: []ServiceInstanceSnapshot[delta.Op]{{
			Members: []ServiceMemberSnapshot[delta.Op]{MethodSnapshot[delta.Op]{Name: "m"}, StateSnapshot[delta.Op]{Name: "s", Sequence: 2, Ops: []delta.Op{delta.Replace{Value: 1}}}},
		}}}}, `{"type":"reset","snapshot":{"serviceId":"s","mode":"singleton","instances":[{"members":[{"name":"m","kind":"method"},{"name":"s","kind":"state","sequence":2,"ops":[["r",1]]}]}]}}`},
		{UnavailableUpdate[delta.Op]{}, `{"type":"unavailable"}`},
		{ReplacedUpdate[delta.Op]{}, `{"type":"replaced","snapshot":{"members":[]}}`},
		{SpawnedUpdate[delta.Op]{Instance: ServiceInstanceSnapshot[delta.Op]{Instance: &address}},
			`{"type":"spawned","instance":{"instance":{"key":"k","generation":1},"members":[]}}`},
		{ClosedUpdate[delta.Op]{Instance: address}, `{"type":"closed","instance":{"key":"k","generation":1}}`},
		{ServiceCall{ServiceID: "a", Member: "b"}, `{"serviceId":"a","member":"b","args":[]}`},
		{ServiceCall{ServiceID: "a", Instance: &address, Member: "b", Args: []Value{nil, "x", 1.5}},
			`{"serviceId":"a","instance":{"key":"k","generation":1},"member":"b","args":[null,"x",1.5]}`},
		// Names are not HTML-escaped, as JSON.stringify does not.
		{StateUpdate[delta.Op]{Member: "a<b&c", Sequence: 1}, `{"type":"state","member":"a<b&c","sequence":1,"ops":[]}`},
		{ServiceCall{ServiceID: "<s>", Member: "m", Args: []Value{"<&>"}}, `{"serviceId":"<s>","member":"m","args":["<&>"]}`},
	}
	for _, tc := range cases {
		got, err := tc.value.MarshalJSON()
		if err != nil {
			t.Errorf("%#v: %v", tc.value, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%#v marshals to %s, want %s", tc.value, got, tc.want)
		}
	}

	// The plain structs marshal by their tags, through json.Marshal.
	plain := []struct {
		value any
		want  string
	}{
		{address, `{"key":"k","generation":1}`},
		{ServiceCatalogueEntry{ServiceID: "s", Mode: Singleton}, `{"serviceId":"s","mode":"singleton"}`},
		// Interface-typed members and updates marshal through their arms.
		{[]ServiceProviderUpdate[delta.Op]{UnavailableUpdate[delta.Op]{}, ClosedUpdate[delta.Op]{Instance: address}},
			`[{"type":"unavailable"},{"type":"closed","instance":{"key":"k","generation":1}}]`},
	}
	for _, tc := range plain {
		got, err := json.Marshal(tc.value)
		if err != nil {
			t.Errorf("%#v: %v", tc.value, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%#v marshals to %s, want %s", tc.value, got, tc.want)
		}
	}
}

// Validate is complete for a value built in Go: it walks addresses, members,
// instances and ops, so a provider can check a snapshot it is about to
// publish with one call, and reports the failing field by name.
func TestTypeValidators(t *testing.T) {
	address := ServiceInstanceAddress{Key: "k", Generation: 1}
	valid := []interface{ Validate() error }{
		ServiceCatalogueEntry{ServiceID: "s", Mode: Singleton},
		address,
		MethodSnapshot[delta.Op]{Name: "m"},
		StateSnapshot[delta.Op]{Name: "s"},
		StateSnapshot[delta.Op]{Name: "s", Sequence: 4, Ops: []delta.Op{delta.Replace{Value: nil}}},
		ServiceInstanceSnapshot[delta.Op]{},
		ServiceInstanceSnapshot[delta.Op]{Instance: &address, Members: []ServiceMemberSnapshot[delta.Op]{MethodSnapshot[delta.Op]{Name: "m"}}},
		ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Keyed},
		StateUpdate[delta.Op]{Member: "m", Sequence: 1},
		StateUpdate[delta.WireOp]{Instance: &address, Member: "m", Sequence: 1, Ops: []delta.WireOp{delta.WireDelete{}}},
		ResetUpdate[delta.Op]{Snapshot: ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Keyed}},
		ResetUpdate[delta.WireOp]{Snapshot: ServiceSubscriptionSnapshot[delta.WireOp]{ServiceID: "s", Mode: Keyed, Instances: []ServiceInstanceSnapshot[delta.WireOp]{
			{Instance: &address, Members: []ServiceMemberSnapshot[delta.WireOp]{MethodSnapshot[delta.WireOp]{Name: "m"}, &StateSnapshot[delta.WireOp]{Name: "s", Ops: []delta.WireOp{delta.Replace{}}}}},
		}}},
		UnavailableUpdate[delta.Op]{},
		ReplacedUpdate[delta.Op]{},
		SpawnedUpdate[delta.Op]{Instance: ServiceInstanceSnapshot[delta.Op]{Instance: &address}},
		ClosedUpdate[delta.Op]{Instance: address},
		ServiceCall{ServiceID: "s", Member: "m"},
		ServiceCall{ServiceID: "s", Instance: &address, Member: "m", Args: []Value{1}},
	}
	for _, v := range valid {
		if err := v.Validate(); err != nil {
			t.Errorf("%#v: Validate() = %v, want nil", v, err)
		}
	}

	invalid := []struct {
		value   interface{ Validate() error }
		mention string
	}{
		{ServiceCatalogueEntry{ServiceID: "", Mode: Keyed}, "serviceId"},
		{ServiceCatalogueEntry{ServiceID: "s", Mode: "other"}, `"other"`},
		// A mode left unset is the zero ServiceMode, "", which pi's isMode
		// refuses like any other string.
		{ServiceCatalogueEntry{ServiceID: "s"}, "mode"},
		{ServiceInstanceAddress{Key: "", Generation: 1}, "key"},
		{ServiceInstanceAddress{Key: "k", Generation: 0}, "generation"},
		{MethodSnapshot[delta.Op]{}, "name"},
		{StateSnapshot[delta.Op]{Name: "s", Sequence: -1}, "sequence"},
		{StateSnapshot[delta.Op]{Name: "s", Ops: []delta.Op{delta.Set{}}}, "ops[0]"},
		{StateSnapshot[delta.Op]{Name: "s", Ops: []delta.Op{nil}}, "ops[0] is nil"},
		// A typed nil is a nil too: the guard reports it rather than
		// dereferencing it.
		{StateSnapshot[delta.Op]{Name: "s", Ops: []delta.Op{(*delta.Replace)(nil)}}, "ops[0] is nil"},
		{ServiceInstanceSnapshot[delta.Op]{Members: []ServiceMemberSnapshot[delta.Op]{(*StateSnapshot[delta.Op])(nil)}}, "members[0] is nil"},
		{ServiceInstanceSnapshot[delta.Op]{Instance: &ServiceInstanceAddress{Key: "k"}}, "instance: generation"},
		{ServiceInstanceSnapshot[delta.Op]{Members: []ServiceMemberSnapshot[delta.Op]{nil}}, "members[0] is nil"},
		{ServiceInstanceSnapshot[delta.Op]{Members: []ServiceMemberSnapshot[delta.Op]{MethodSnapshot[delta.Op]{}}}, "members[0]: name"},
		{ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: "x"}, "mode"},
		{ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s"}, "mode"},
		{ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Keyed, Instances: []ServiceInstanceSnapshot[delta.Op]{{Instance: &ServiceInstanceAddress{}}}}, "instances[0]: instance: key"},
		{StateUpdate[delta.Op]{Member: "m", Sequence: 0}, "state update sequence"},
		{StateUpdate[delta.Op]{Member: "", Sequence: 1}, "state update member"},
		{StateUpdate[delta.Op]{Instance: &ServiceInstanceAddress{Key: "k"}, Member: "m", Sequence: 1}, "state update instance: generation"},
		{StateUpdate[delta.Op]{Member: "m", Sequence: 1, Ops: []delta.Op{delta.Truncate{Path: delta.Path{delta.Key("s")}, Count: -1}}}, "state update ops[0]"},
		{ReplacedUpdate[delta.Op]{Snapshot: ServiceInstanceSnapshot[delta.Op]{Instance: &ServiceInstanceAddress{}}}, "replacement update snapshot: instance"},
		{ResetUpdate[delta.Op]{Snapshot: ServiceSubscriptionSnapshot[delta.Op]{Mode: Singleton}}, "reset update snapshot: serviceId"},
		// The snapshot's own checks run first: a nil op is reported as such,
		// not as a missing replacement.
		{ResetUpdate[delta.Op]{Snapshot: resetOf(StateSnapshot[delta.Op]{Name: "s", Ops: []delta.Op{nil}})}, "ops[0] is nil"},
		{ResetUpdate[delta.Op]{Snapshot: resetOf(&StateSnapshot[delta.Op]{Name: "s", Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("a")}, Value: 1}}})}, "instances[0]: members[0]: a reset must contain full root replacements"},
		{ResetUpdate[delta.Op]{Snapshot: resetOf(MethodSnapshot[delta.Op]{Name: "m"}, StateSnapshot[delta.Op]{Name: "s"})}, "members[1]: a reset must contain full root replacements, one [\"r\", value] op per state; got 0 ops"},
		{SpawnedUpdate[delta.Op]{Instance: ServiceInstanceSnapshot[delta.Op]{Members: []ServiceMemberSnapshot[delta.Op]{nil}}}, "spawn update instance: members[0]"},
		{ClosedUpdate[delta.Op]{}, "close update instance: key"},
		{ServiceCall{ServiceID: "", Member: "m"}, "serviceId"},
		{ServiceCall{ServiceID: "s", Member: ""}, "member"},
		{ServiceCall{ServiceID: "s", Instance: &ServiceInstanceAddress{Key: "k"}, Member: "m"}, "instance: generation"},
	}
	for _, tc := range invalid {
		err := tc.value.Validate()
		if err == nil {
			t.Errorf("%#v: Validate() = nil, want an error mentioning %q", tc.value, tc.mention)
			continue
		}
		if !strings.Contains(err.Error(), tc.mention) {
			t.Errorf("%#v: Validate() = %q, want it to mention %q", tc.value, err, tc.mention)
		}
	}
}

// The seal is per grammar: an arm instantiated for one op type is not a
// member of the other's union. This is a compile-time property, pinned
// here so a refactor that drops O from the marker method is caught.
func TestSealsBindToOneGrammar(t *testing.T) {
	var decoded ServiceProviderUpdate[delta.Op] = StateUpdate[delta.Op]{Member: "m", Sequence: 1}
	if _, ok := any(decoded).(WireServiceProviderUpdate); ok {
		t.Error("a decoded StateUpdate satisfies the wire union")
	}
	var member ServiceMemberSnapshot[delta.WireOp] = StateSnapshot[delta.WireOp]{Name: "s"}
	if _, ok := any(member).(ServiceMemberSnapshot[delta.Op]); ok {
		t.Error("a wire StateSnapshot satisfies the decoded union")
	}
}

// resetOf is a singleton subscription snapshot holding members.
func resetOf(members ...ServiceMemberSnapshot[delta.Op]) ServiceSubscriptionSnapshot[delta.Op] {
	return ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "s", Mode: Singleton, Instances: []ServiceInstanceSnapshot[delta.Op]{{Members: members}}}
}
