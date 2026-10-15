package chord

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Port of packages/chord/test/service-wire.test.ts at 64eeb82a4, "service
// wire protocol". Its seven cases, in order:
//
//  1. "encodes control calls and validates service values"
//  2. "rejects malformed service values"
//  3. "validates decoded and wire snapshots and updates"
//  4. "keeps one operation codec pair for one subscription state"
//  5. "isolates operation dictionaries between states and subscriptions"
//  6. "creates and removes keyed instance codecs with their lifecycle"
//  7. "remote service endpoints publish and clean up provider subscriptions"
//
// Cases 1-3 are this file's subject and are ported whole. Cases 4-7 drive the
// state codec and the provider endpoint, which are later slices; what they
// establish about the WIRE — the exact JSON a codec emits on a path's first
// and second use, the keyed lifecycle updates, the endpoint's catalogue and
// state update — is pinned here as parse and marshal goldens, so those
// slices inherit the shapes rather than re-deriving them.
//
// Trees come from JSON literals, as they do from an envelope decoder: numbers
// are float64, and a Go int in a hand-built map is not a wire number.

func tree(t *testing.T, literal string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(literal), &v); err != nil {
		t.Fatalf("bad test literal %s: %v", literal, err)
	}
	return v
}

// encode is the value's own MarshalJSON: the bytes an Encoder with
// SetEscapeHTML(false) passes through verbatim.
func encode(t *testing.T, v json.Marshaler) string {
	t.Helper()
	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("%#v: MarshalJSON: %v", v, err)
	}
	return string(data)
}

func wantServiceValueError(t *testing.T, err error, what string, contains ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want invalid %s", what)
	}
	var sve *ServiceValueError
	if !errors.As(err, &sve) {
		t.Fatalf("error %T %q is not a *ServiceValueError", err, err)
	}
	if sve.What != what {
		t.Errorf("What = %q, want %q", sve.What, what)
	}
	// The rule that failed is the error's cause, for errors.Is and errors.As.
	// (A nested op's own error reaches it flattened to text: ledger D47.)
	if cause := errors.Unwrap(sve); cause == nil || cause != sve.Err {
		t.Errorf("errors.Unwrap(%q) = %v, want Err, the rule that failed", err, cause)
	}
	for _, s := range append([]string{"invalid " + what}, contains...) {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}
}

// Case 1: control calls round-trip through their ServiceCall form, and the
// catalogue and call parsers accept well-formed values. The three control
// call encodings are pinned as bytes: they are what pi's client sends.
func TestEncodesControlCallsAndValidatesServiceValues(t *testing.T) {
	calls := []struct {
		control ServiceControlCall
		want    string
	}{
		{CatalogueCall{}, `{"serviceId":"$chord.service","member":"catalogue","args":[]}`},
		{SubscribeCall{SubscriptionID: "subscription-1", ServiceID: "pi.models", Mode: Singleton},
			`{"serviceId":"$chord.service","member":"subscribe","args":["subscription-1","pi.models","singleton"]}`},
		{UnsubscribeCall{SubscriptionID: "subscription-1"},
			`{"serviceId":"$chord.service","member":"unsubscribe","args":["subscription-1"]}`},
	}
	for _, tc := range calls {
		call := tc.control.Call()
		if got := encode(t, call); got != tc.want {
			t.Errorf("%#v.Call() marshals to %s, want %s", tc.control, got, tc.want)
		}
		decoded, ok := DecodeServiceControlCall(call)
		if !ok || !reflect.DeepEqual(decoded, tc.control) {
			t.Errorf("DecodeServiceControlCall(%#v.Call()) = %#v, %v; want %#v, true", tc.control, decoded, ok, tc.control)
		}
		// A call that has crossed the wire is the same call.
		reparsed, err := ParseServiceCall(tree(t, tc.want))
		if err != nil {
			t.Fatalf("ParseServiceCall(%s): %v", tc.want, err)
		}
		if !reflect.DeepEqual(reparsed, call) {
			t.Errorf("ParseServiceCall(%s) = %#v, want %#v", tc.want, reparsed, call)
		}
	}

	catalogue, err := ParseServiceCatalogue(tree(t, `[{"serviceId":"pi.models","mode":"singleton"},{"serviceId":"pi.dialogs","mode":"keyed"}]`))
	if err != nil {
		t.Fatalf("ParseServiceCatalogue: %v", err)
	}
	wantCatalogue := []ServiceCatalogueEntry{{ServiceID: "pi.models", Mode: Singleton}, {ServiceID: "pi.dialogs", Mode: Keyed}}
	if !reflect.DeepEqual(catalogue, wantCatalogue) {
		t.Errorf("catalogue = %#v, want %#v", catalogue, wantCatalogue)
	}

	call, err := ParseServiceCall(tree(t, `{"serviceId":"pi.question-dialog","instance":{"key":"invocation-1","generation":2},"member":"submit","args":[{"outcome":"selected","index":0}]}`))
	if err != nil {
		t.Fatalf("ParseServiceCall: %v", err)
	}
	wantCall := ServiceCall{
		ServiceID: "pi.question-dialog",
		Instance:  &ServiceInstanceAddress{Key: "invocation-1", Generation: 2},
		Member:    "submit",
		Args:      []Value{map[string]any{"outcome": "selected", "index": float64(0)}},
	}
	if !reflect.DeepEqual(call, wantCall) {
		t.Errorf("call = %#v, want %#v", call, wantCall)
	}
	// An addressed call is not a control call.
	if control, ok := DecodeServiceControlCall(call); ok {
		t.Errorf("DecodeServiceControlCall(addressed method call) = %#v, true; want false", control)
	}
}

// Case 2: each parser refuses its malformed value with upstream's description
// of the value, and the failure names the rule so a peer author can fix the
// encoder.
func TestRejectsMalformedServiceValues(t *testing.T) {
	_, err := ParseServiceCall(tree(t, `{"serviceId":"pi.models","member":"list","args":[],"extra":true}`))
	wantServiceValueError(t, err, "service call", `"extra"`)

	_, err = ParseServiceCatalogue(tree(t, `[{"serviceId":"pi.models","mode":"unknown"}]`))
	wantServiceValueError(t, err, "service catalogue", "mode", "unknown")

	_, err = ParseServiceProviderUpdate(tree(t, `{"type":"state","member":"state","sequence":0,"ops":[]}`))
	wantServiceValueError(t, err, "service provider update", "state update", "sequence", "at least 1")

	_, err = ParseWireServiceProviderUpdate(tree(t, `{"type":"state","member":"state","sequence":1,"ops":[["?",0]]}`))
	wantServiceValueError(t, err, "service provider update", "ops[0]", `"?"`)
}

// Case 3: a decoded snapshot and update parse to their typed forms and marshal
// back to the same bytes; the wire parsers accept the wire forms a codec
// makes of them. For a snapshot's single Replace and an update's first use of
// a path, the wire form IS the decoded form (Replace encodes to itself and a
// first path use is inline), which is why one literal serves both grammars.
func TestValidatesDecodedAndWireSnapshotsAndUpdates(t *testing.T) {
	snapshot := `{"serviceId":"pi.models","mode":"singleton","instances":[{"members":[{"name":"state","kind":"state","sequence":0,"ops":[["r",{"revision":1}]]}]}]}`
	decoded, err := ParseServiceSubscriptionSnapshot(tree(t, snapshot))
	if err != nil {
		t.Fatalf("ParseServiceSubscriptionSnapshot: %v", err)
	}
	wantDecoded := ServiceSubscriptionSnapshot[delta.Op]{
		ServiceID: "pi.models",
		Mode:      Singleton,
		Instances: []ServiceInstanceSnapshot[delta.Op]{{
			Members: []ServiceMemberSnapshot[delta.Op]{
				StateSnapshot[delta.Op]{Name: "state", Sequence: 0, Ops: []delta.Op{delta.Replace{Value: map[string]any{"revision": float64(1)}}}},
			},
		}},
	}
	if !reflect.DeepEqual(decoded, wantDecoded) {
		t.Errorf("decoded snapshot = %#v, want %#v", decoded, wantDecoded)
	}
	if got := encode(t, decoded); got != snapshot {
		t.Errorf("decoded snapshot marshals to %s, want %s", got, snapshot)
	}
	wire, err := ParseWireServiceSubscriptionSnapshot(tree(t, snapshot))
	if err != nil {
		t.Fatalf("ParseWireServiceSubscriptionSnapshot: %v", err)
	}
	wantWire := WireServiceSubscriptionSnapshot{
		ServiceID: "pi.models",
		Mode:      Singleton,
		Instances: []WireServiceInstanceSnapshot{{
			Members: []WireServiceMemberSnapshot{
				StateSnapshot[delta.WireOp]{Name: "state", Sequence: 0, Ops: []delta.WireOp{delta.Replace{Value: map[string]any{"revision": float64(1)}}}},
			},
		}},
	}
	if !reflect.DeepEqual(wire, wantWire) {
		t.Errorf("wire snapshot = %#v, want %#v", wire, wantWire)
	}
	if got := encode(t, wire); got != snapshot {
		t.Errorf("wire snapshot marshals to %s, want %s", got, snapshot)
	}

	update := `{"type":"state","member":"state","sequence":1,"ops":[["s",["revision"],2]]}`
	decodedUpdate, err := ParseServiceProviderUpdate(tree(t, update))
	if err != nil {
		t.Fatalf("ParseServiceProviderUpdate: %v", err)
	}
	wantUpdate := StateUpdate[delta.Op]{Member: "state", Sequence: 1, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("revision")}, Value: float64(2)}}}
	if !reflect.DeepEqual(decodedUpdate, wantUpdate) {
		t.Errorf("decoded update = %#v, want %#v", decodedUpdate, wantUpdate)
	}
	if got := encode(t, decodedUpdate); got != update {
		t.Errorf("decoded update marshals to %s, want %s", got, update)
	}
	wireUpdate, err := ParseWireServiceProviderUpdate(tree(t, update))
	if err != nil {
		t.Fatalf("ParseWireServiceProviderUpdate: %v", err)
	}
	wantWireUpdate := StateUpdate[delta.WireOp]{Member: "state", Sequence: 1, Ops: []delta.WireOp{delta.WireSet{Ref: delta.Path{delta.Key("revision")}, Value: float64(2)}}}
	if !reflect.DeepEqual(wireUpdate, wantWireUpdate) {
		t.Errorf("wire update = %#v, want %#v", wireUpdate, wantWireUpdate)
	}
	if got := encode(t, wireUpdate); got != update {
		t.Errorf("wire update marshals to %s, want %s", got, update)
	}
}

// Cases 4 and 5: the wire updates a codec emits on a path's second use — a
// definition followed by an id reference — and on a later reuse of the id.
// They are wire grammar only: the wire parser accepts them and marshals them
// back byte for byte, and the decoded parser refuses them, because a "#" is
// not a decoded op and an id is not a path.
func TestCodecWireUpdatesAreWireGrammarOnly(t *testing.T) {
	cases := []struct {
		literal string
		want    WireServiceProviderUpdate
	}{
		{`{"type":"state","member":"state","sequence":1,"ops":[["s",["revision"],1]]}`,
			StateUpdate[delta.WireOp]{Member: "state", Sequence: 1, Ops: []delta.WireOp{delta.WireSet{Ref: delta.Path{delta.Key("revision")}, Value: float64(1)}}}},
		{`{"type":"state","member":"state","sequence":2,"ops":[["#",0,["revision"]],["s",0,2]]}`,
			StateUpdate[delta.WireOp]{Member: "state", Sequence: 2, Ops: []delta.WireOp{
				delta.Define{ID: 0, Path: delta.Path{delta.Key("revision")}},
				delta.WireSet{Ref: delta.PathID(0), Value: float64(2)},
			}}},
		{`{"type":"state","member":"right","sequence":3,"ops":[["s",0,3]]}`,
			StateUpdate[delta.WireOp]{Member: "right", Sequence: 3, Ops: []delta.WireOp{delta.WireSet{Ref: delta.PathID(0), Value: float64(3)}}}},
	}
	for _, tc := range cases {
		got, err := ParseWireServiceProviderUpdate(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseWireServiceProviderUpdate(%s): %v", tc.literal, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseWireServiceProviderUpdate(%s) = %#v, want %#v", tc.literal, got, tc.want)
		}
		if enc := encode(t, got); enc != tc.literal {
			t.Errorf("marshals to %s, want %s", enc, tc.literal)
		}
	}
	for _, literal := range []string{cases[1].literal, cases[2].literal} {
		if got, err := ParseServiceProviderUpdate(tree(t, literal)); err == nil {
			t.Errorf("ParseServiceProviderUpdate(%s) = %#v, want an error: interned paths are wire grammar", literal, got)
		} else {
			wantServiceValueError(t, err, "service provider update", "ops[")
		}
	}
	// A base batch resets the dictionary; its Replace is the same in both grammars.
	base := `{"type":"state","member":"left","sequence":3,"ops":[["r",{"revision":3}]]}`
	if got, err := ParseServiceProviderUpdate(tree(t, base)); err != nil || encode(t, got) != base {
		t.Errorf("ParseServiceProviderUpdate(%s) = %#v, %v", base, got, err)
	}
	if got, err := ParseWireServiceProviderUpdate(tree(t, base)); err != nil || encode(t, got) != base {
		t.Errorf("ParseWireServiceProviderUpdate(%s) = %#v, %v", base, got, err)
	}
}

// Case 6: the keyed lifecycle — an empty keyed snapshot, a spawn, an addressed
// state update and a close — as both parsers see it. Every literal is the
// codec's own output for the decoded value (a first path use is inline), so
// each parses in both grammars and marshals back to itself.
func TestKeyedInstanceLifecycleShapes(t *testing.T) {
	snapshot := `{"serviceId":"pi.dialogs","mode":"keyed","instances":[]}`
	decoded, err := ParseServiceSubscriptionSnapshot(tree(t, snapshot))
	if err != nil {
		t.Fatalf("ParseServiceSubscriptionSnapshot: %v", err)
	}
	wantSnapshot := ServiceSubscriptionSnapshot[delta.Op]{ServiceID: "pi.dialogs", Mode: Keyed, Instances: []ServiceInstanceSnapshot[delta.Op]{}}
	if !reflect.DeepEqual(decoded, wantSnapshot) {
		t.Errorf("snapshot = %#v, want %#v", decoded, wantSnapshot)
	}
	if got := encode(t, decoded); got != snapshot {
		t.Errorf("snapshot marshals to %s, want %s", got, snapshot)
	}

	address := ServiceInstanceAddress{Key: "dialog-1", Generation: 1}
	updates := []struct {
		literal string
		want    ServiceProviderUpdate[delta.Op]
	}{
		{`{"type":"spawned","instance":{"instance":{"key":"dialog-1","generation":1},"members":[{"name":"request","kind":"state","sequence":0,"ops":[["r",{"value":0}]]}]}}`,
			SpawnedUpdate[delta.Op]{Instance: ServiceInstanceSnapshot[delta.Op]{
				Instance: &address,
				Members: []ServiceMemberSnapshot[delta.Op]{
					StateSnapshot[delta.Op]{Name: "request", Sequence: 0, Ops: []delta.Op{delta.Replace{Value: map[string]any{"value": float64(0)}}}},
				},
			}}},
		{`{"type":"state","instance":{"key":"dialog-1","generation":1},"member":"request","sequence":1,"ops":[["s",["value"],1]]}`,
			StateUpdate[delta.Op]{Instance: &address, Member: "request", Sequence: 1, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("value")}, Value: float64(1)}}}},
		{`{"type":"closed","instance":{"key":"dialog-1","generation":1}}`,
			ClosedUpdate[delta.Op]{Instance: address}},
	}
	for _, tc := range updates {
		got, err := ParseServiceProviderUpdate(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseServiceProviderUpdate(%s): %v", tc.literal, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseServiceProviderUpdate(%s) = %#v, want %#v", tc.literal, got, tc.want)
		}
		if enc := encode(t, got); enc != tc.literal {
			t.Errorf("decoded marshals to %s, want %s", enc, tc.literal)
		}
		wire, err := ParseWireServiceProviderUpdate(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseWireServiceProviderUpdate(%s): %v", tc.literal, err)
			continue
		}
		if enc := encode(t, wire); enc != tc.literal {
			t.Errorf("wire marshals to %s, want %s", enc, tc.literal)
		}
	}
}

// Case 7: what the endpoint answers a catalogue call with, and the update a
// provider publishes after `state.state.value = 1`, as bytes built from Go
// values — the producer side of the goldens above.
func TestEndpointCatalogueAndUpdateGoldens(t *testing.T) {
	catalogue, err := json.Marshal([]ServiceCatalogueEntry{{ServiceID: "test.counter", Mode: Singleton}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"serviceId":"test.counter","mode":"singleton"}]`; string(catalogue) != want {
		t.Errorf("catalogue marshals to %s, want %s", catalogue, want)
	}
	update := StateUpdate[delta.Op]{Member: "state", Sequence: 1, Ops: []delta.Op{delta.Set{Path: delta.Path{delta.Key("value")}, Value: 1}}}
	if got, want := encode(t, update), `{"type":"state","member":"state","sequence":1,"ops":[["s",["value"],1]]}`; got != want {
		t.Errorf("update marshals to %s, want %s", got, want)
	}
	unavailable := UnavailableUpdate[delta.Op]{}
	if got, want := encode(t, unavailable), `{"type":"unavailable"}`; got != want {
		t.Errorf("unavailable marshals to %s, want %s", got, want)
	}
}

// DecodeServiceControlCall is the $chord.service grammar: the control ID
// with no address, and one of three members with exactly its arguments.
// Anything else — including a malformed control call — is an ordinary call
// for the provider to refuse, which is what upstream's undefined means.
func TestDecodeServiceControlCallGrammar(t *testing.T) {
	control := "$chord.service"
	address := &ServiceInstanceAddress{Key: "k", Generation: 1}
	notControl := []ServiceCall{
		{ServiceID: "pi.models", Member: "catalogue", Args: []Value{}},
		{ServiceID: control, Instance: address, Member: "catalogue", Args: []Value{}},
		{ServiceID: control, Member: "catalogue", Args: []Value{"x"}},
		{ServiceID: control, Member: "list", Args: []Value{}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", "singleton", "extra"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{1, "pi.models", "singleton"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"", "pi.models", "singleton"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "", "singleton"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", "other"}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", nil}},
		// The empty mode is no mode, however it is spelled: pi's
		// decodeServiceControlCall returns undefined for it.
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", ""}},
		{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", ServiceMode("")}},
		{ServiceID: control, Member: "unsubscribe", Args: []Value{}},
		{ServiceID: control, Member: "unsubscribe", Args: []Value{""}},
		{ServiceID: control, Member: "unsubscribe", Args: []Value{"s", "t"}},
	}
	for _, call := range notControl {
		if got, ok := DecodeServiceControlCall(call); ok {
			t.Errorf("DecodeServiceControlCall(%#v) = %#v, true; want false", call, got)
		}
	}
	// A nil Args is an empty argument list, as it marshals.
	if got, ok := DecodeServiceControlCall(ServiceCall{ServiceID: control, Member: "catalogue"}); !ok || got != (CatalogueCall{}) {
		t.Errorf("nil-args catalogue = %#v, %v; want CatalogueCall{}, true", got, ok)
	}
	// A Go caller may spell the mode as a ServiceMode; it is the same string.
	typed := ServiceCall{ServiceID: control, Member: "subscribe", Args: []Value{"s", "pi.models", Keyed}}
	want := SubscribeCall{SubscriptionID: "s", ServiceID: "pi.models", Mode: Keyed}
	if got, ok := DecodeServiceControlCall(typed); !ok || got != want {
		t.Errorf("typed-mode subscribe = %#v, %v; want %#v, true", got, ok, want)
	}
}

// The parsers are a security boundary: an unknown key, a null where an object
// is optional, a wrong kind, an unknown discriminator or a value below its
// minimum is a rejection at every level, not something to ignore. Each
// literal is well-formed but for the one thing its name says.
func TestParsersRejectAtEveryLevel(t *testing.T) {
	updates := []struct{ name, literal, mention string }{
		{"not an object", `[]`, "object"},
		{"missing type", `{"member":"state","sequence":1,"ops":[]}`, `"type"`},
		{"unknown type", `{"type":"bogus"}`, `"bogus"`},
		{"state extra key", `{"type":"state","member":"state","sequence":1,"ops":[],"extra":1}`, `"extra"`},
		{"state null instance", `{"type":"state","instance":null,"member":"state","sequence":1,"ops":[]}`, "instance"},
		{"state missing ops", `{"type":"state","member":"state","sequence":1}`, `"ops"`},
		{"state ops not array", `{"type":"state","member":"state","sequence":1,"ops":{}}`, "ops"},
		{"state fractional sequence", `{"type":"state","member":"state","sequence":1.5,"ops":[]}`, "sequence"},
		{"state empty member", `{"type":"state","member":"","sequence":1,"ops":[]}`, "member"},
		{"state bad op", `{"type":"state","member":"state","sequence":1,"ops":[["s",["__proto__"],1]]}`, "ops[0]"},
		{"reset missing snapshot", `{"type":"reset"}`, `"snapshot"`},
		{"reset snapshot extra key", `{"type":"reset","snapshot":{"serviceId":"s","mode":"keyed","instances":[],"extra":1}}`, `"extra"`},
		{"reset non-root state", `{"type":"reset","snapshot":{"serviceId":"s","mode":"singleton","instances":[{"members":[{"name":"m","kind":"method"},{"name":"s","kind":"state","sequence":1,"ops":[]}]}]}}`, "instances[0]: members[1]: a reset must contain full root replacements"},
		{"unavailable extra key", `{"type":"unavailable","extra":true}`, `"extra"`},
		{"replaced missing snapshot", `{"type":"replaced"}`, `"snapshot"`},
		{"replaced snapshot extra key", `{"type":"replaced","snapshot":{"members":[],"extra":1}}`, `"extra"`},
		{"spawned method extra key", `{"type":"spawned","instance":{"members":[{"name":"m","kind":"method","sequence":0}]}}`, `"sequence"`},
		{"spawned unknown member kind", `{"type":"spawned","instance":{"members":[{"name":"m","kind":"other"}]}}`, `"other"`},
		{"spawned member missing kind", `{"type":"spawned","instance":{"members":[{"name":"m"}]}}`, `"kind"`},
		{"spawned state negative sequence", `{"type":"spawned","instance":{"members":[{"name":"m","kind":"state","sequence":-1,"ops":[]}]}}`, "sequence"},
		{"spawned null instance address", `{"type":"spawned","instance":{"instance":null,"members":[]}}`, "instance"},
		{"closed generation zero", `{"type":"closed","instance":{"key":"k","generation":0}}`, "generation"},
		{"closed empty key", `{"type":"closed","instance":{"key":"","generation":1}}`, "key"},
		{"closed address extra key", `{"type":"closed","instance":{"key":"k","generation":1,"extra":1}}`, `"extra"`},
		{"closed missing instance", `{"type":"closed"}`, `"instance"`},
	}
	for _, tc := range updates {
		t.Run("update "+tc.name, func(t *testing.T) {
			v := tree(t, tc.literal)
			if got, err := ParseServiceProviderUpdate(v); err == nil {
				t.Errorf("decoded: accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service provider update", tc.mention)
			}
			if got, err := ParseWireServiceProviderUpdate(v); err == nil {
				t.Errorf("wire: accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service provider update", tc.mention)
			}
		})
	}

	snapshots := []struct{ name, literal, mention string }{
		{"not an object", `"pi.models"`, "object"},
		{"extra key", `{"serviceId":"s","mode":"keyed","instances":[],"extra":1}`, `"extra"`},
		{"bad mode", `{"serviceId":"s","mode":"both","instances":[]}`, `"both"`},
		{"empty mode", `{"serviceId":"s","mode":"","instances":[]}`, "mode"},
		{"empty id", `{"serviceId":"","mode":"keyed","instances":[]}`, "serviceId"},
		{"instances not array", `{"serviceId":"s","mode":"keyed","instances":{}}`, "instances"},
		{"instance extra key", `{"serviceId":"s","mode":"keyed","instances":[{"members":[],"extra":1}]}`, `"extra"`},
		{"instance null address", `{"serviceId":"s","mode":"keyed","instances":[{"instance":null,"members":[]}]}`, "instance"},
		// A reserved segment is refused in both grammars; a two-element ["s", 1]
		// would be a legal wire short form.
		{"member bad op", `{"serviceId":"s","mode":"keyed","instances":[{"members":[{"name":"x","kind":"state","sequence":0,"ops":[["s",["__proto__"],1]]}]}]}`, "ops[0]"},
	}
	for _, tc := range snapshots {
		t.Run("snapshot "+tc.name, func(t *testing.T) {
			v := tree(t, tc.literal)
			if got, err := ParseServiceSubscriptionSnapshot(v); err == nil {
				t.Errorf("decoded: accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service subscription snapshot", tc.mention)
			}
			if got, err := ParseWireServiceSubscriptionSnapshot(v); err == nil {
				t.Errorf("wire: accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service subscription snapshot", tc.mention)
			}
		})
	}

	calls := []struct{ name, literal, mention string }{
		{"not an object", `null`, "object"},
		{"missing args", `{"serviceId":"s","member":"m"}`, `"args"`},
		{"args not array", `{"serviceId":"s","member":"m","args":{}}`, "args"},
		{"empty member", `{"serviceId":"s","member":"","args":[]}`, "member"},
		{"empty service", `{"serviceId":"","member":"m","args":[]}`, "serviceId"},
		{"null instance", `{"serviceId":"s","instance":null,"member":"m","args":[]}`, "instance"},
		{"instance extra key", `{"serviceId":"s","instance":{"key":"k","generation":1,"extra":1},"member":"m","args":[]}`, `"extra"`},
		{"generation zero", `{"serviceId":"s","instance":{"key":"k","generation":0},"member":"m","args":[]}`, "generation"},
	}
	for _, tc := range calls {
		t.Run("call "+tc.name, func(t *testing.T) {
			if got, err := ParseServiceCall(tree(t, tc.literal)); err == nil {
				t.Errorf("accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service call", tc.mention)
			}
		})
	}

	catalogues := []struct{ name, literal, mention string }{
		{"not an array", `{}`, "array"},
		{"entry extra key", `[{"serviceId":"a","mode":"keyed","extra":1}]`, `"extra"`},
		{"entry missing mode", `[{"serviceId":"a"}]`, `"mode"`},
		{"entry empty mode", `[{"serviceId":"a","mode":""}]`, "mode"},
		{"duplicate id", `[{"serviceId":"a","mode":"keyed"},{"serviceId":"a","mode":"singleton"}]`, `duplicate`},
	}
	for _, tc := range catalogues {
		t.Run("catalogue "+tc.name, func(t *testing.T) {
			if got, err := ParseServiceCatalogue(tree(t, tc.literal)); err == nil {
				t.Errorf("accepted as %#v", got)
			} else {
				wantServiceValueError(t, err, "service catalogue", tc.mention)
			}
		})
	}
}

// A parsed value is complete: an empty array is an empty slice, never nil,
// so it marshals back as [] and a consumer can range over it without a nil
// check — and a parsed call's args are the tree's own values, borrowed.
func TestParsedValuesAreComplete(t *testing.T) {
	call, err := ParseServiceCall(tree(t, `{"serviceId":"s","member":"m","args":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if call.Args == nil {
		t.Error("parsed args are nil, want an empty slice")
	}
	if call.Instance != nil {
		t.Errorf("parsed instance = %#v, want nil for an unaddressed call", call.Instance)
	}
	update, err := ParseServiceProviderUpdate(tree(t, `{"type":"state","member":"m","sequence":1,"ops":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := update.(StateUpdate[delta.Op]); !ok || state.Ops == nil {
		t.Errorf("parsed update = %#v, want a StateUpdate with empty non-nil ops", update)
	}
	catalogue, err := ParseServiceCatalogue(tree(t, `[]`))
	if err != nil {
		t.Fatal(err)
	}
	if catalogue == nil {
		t.Error("parsed empty catalogue is nil, want an empty slice")
	}
}

// Upstream 35180b9df (service-wire.test.ts "validates explicit resets and
// restarts path dictionaries at the new baseline"), its parser half: a
// "reset" update carries a full subscription snapshot that rebaselines every
// state, so each state member must be exactly one root replacement. Both
// grammars accept it — a replacement encodes to itself — and marshal it back
// byte for byte; the codec half (restarting path dictionaries) belongs to the
// state codec, which is not ported yet.
func TestValidatesExplicitResets(t *testing.T) {
	reset := `{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[{"members":[{"name":"state","kind":"state","sequence":103,"ops":[["r",{"after":103}]]}]}]}}`
	decoded, err := ParseServiceProviderUpdate(tree(t, reset))
	if err != nil {
		t.Fatalf("ParseServiceProviderUpdate: %v", err)
	}
	want := ResetUpdate[delta.Op]{Snapshot: ServiceSubscriptionSnapshot[delta.Op]{
		ServiceID: "pi.states",
		Mode:      Singleton,
		Instances: []ServiceInstanceSnapshot[delta.Op]{{
			Members: []ServiceMemberSnapshot[delta.Op]{
				StateSnapshot[delta.Op]{Name: "state", Sequence: 103, Ops: []delta.Op{delta.Replace{Value: map[string]any{"after": float64(103)}}}},
			},
		}},
	}}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("decoded reset = %#v, want %#v", decoded, want)
	}
	if got := encode(t, decoded); got != reset {
		t.Errorf("decoded reset marshals to %s, want %s", got, reset)
	}
	wire, err := ParseWireServiceProviderUpdate(tree(t, reset))
	if err != nil {
		t.Fatalf("ParseWireServiceProviderUpdate: %v", err)
	}
	if got := encode(t, wire); got != reset {
		t.Errorf("wire reset marshals to %s, want %s", got, reset)
	}

	for name, parse := range map[string]func(any) error{
		"decoded": func(v any) error { _, err := ParseServiceProviderUpdate(v); return err },
		"wire":    func(v any) error { _, err := ParseWireServiceProviderUpdate(v); return err },
	} {
		t.Run(name, func(t *testing.T) {
			withExtra := strings.Replace(reset, `{"type":"reset",`, `{"type":"reset","extra":true,`, 1)
			wantServiceValueError(t, parse(tree(t, withExtra)), "service provider update", `"extra"`)
			wantServiceValueError(t, parse(tree(t, `{"type":"reset","snapshot":{}}`)), "service provider update")
			notRoot := `{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[{"members":[{"name":"state","kind":"state","sequence":103,"ops":[["s",["before"],103]]}]}]}}`
			wantServiceValueError(t, parse(tree(t, notRoot)), "service provider update", "full root replacements")
			twoOps := `{"type":"reset","snapshot":{"serviceId":"pi.states","mode":"singleton","instances":[{"members":[{"name":"state","kind":"state","sequence":103,"ops":[["r",1],["r",2]]}]}]}}`
			wantServiceValueError(t, parse(tree(t, twoOps)), "service provider update", "full root replacements")
		})
	}
}

// Upstream 35180b9df, against pi itself. The oracle,
// testdata/reset/reset-35180b9df.json, was captured from pi's own
// packages/chord/src at that sha by testdata/reset/capture.mts (node v26.4.0;
// its header says how). "real" holds the resets pi's RemoteServiceProvider
// sends when a subscription's delivery buffer overflows — with method
// members, several states, keyed instances, and no instances at all — and
// "table" holds pi's two parsers' verdicts on 46 literals.
func TestResetUpdatesMatchPi(t *testing.T) {
	data, err := os.ReadFile("testdata/reset/reset-35180b9df.json")
	if err != nil {
		t.Fatalf("read oracle: %v (regenerate it with testdata/reset/capture.mts)", err)
	}
	type verdict struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	var oracle struct {
		Real []struct {
			Name    string `json:"name"`
			Decoded string `json:"decoded"`
			Wire    string `json:"wire"`
		} `json:"real"`
		Table []struct {
			Name    string  `json:"name"`
			Literal string  `json:"literal"`
			Decoded verdict `json:"decoded"`
			Wire    verdict `json:"wire"`
		} `json:"table"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if len(oracle.Real) != 4 || len(oracle.Table) != 46 {
		t.Fatalf("oracle holds %d resets and %d rows, want 4 and 46", len(oracle.Real), len(oracle.Table))
	}
	// canonical re-encodes JSON with sorted keys and no HTML escaping.
	canonical := func(s string) string {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("bad JSON %s: %v", s, err)
		}
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	parsers := []struct {
		grammar string
		parse   func(any) (json.Marshaler, error)
	}{
		{"decoded", func(v any) (json.Marshaler, error) { return ParseServiceProviderUpdate(v) }},
		{"wire", func(v any) (json.Marshaler, error) { return ParseWireServiceProviderUpdate(v) }},
	}

	for _, reset := range oracle.Real {
		for i, p := range parsers {
			want := reset.Decoded
			if i == 1 {
				want = reset.Wire
			}
			t.Run(reset.Name+" "+p.grammar, func(t *testing.T) {
				update, err := p.parse(tree(t, want))
				if err != nil {
					t.Fatalf("pi sent it, the port refuses it: %v", err)
				}
				if _, ok := update.(interface{ update(delta.Op) }); !ok && i == 0 {
					t.Fatalf("parsed as %T, want a decoded ResetUpdate", update)
				}
				got := encode(t, update)
				// The one difference allowed is the recorded ruling on decoded
				// key order (UPSTREAM.md: do NOT make the decoder
				// order-preserving): a replacement's object payload comes back
				// with its keys sorted, where pi keeps insertion order.
				if got != want && (canonical(got) != canonical(want) || canonical(want) == want) {
					t.Errorf("re-marshals to\n %s\nwant pi's\n %s", got, want)
				}
			})
		}
	}

	const rule = "full root replacements"
	for _, row := range oracle.Table {
		for i, p := range parsers {
			want := row.Decoded
			if i == 1 {
				want = row.Wire
			}
			t.Run(row.Name+" "+p.grammar, func(t *testing.T) {
				_, err := p.parse(tree(t, row.Literal))
				if want.OK {
					if err != nil {
						t.Fatalf("pi accepts it, the port refuses it: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("pi refuses it (%q), the port accepts it", want.Error)
				}
				// The texts are the port's own (ServiceValueError never
				// crosses the wire); what must agree is which check fires.
				if piRule, portRule := strings.Contains(want.Error, rule), strings.Contains(err.Error(), rule); piRule != portRule {
					t.Fatalf("pi refuses it with %q, the port with %q: they disagree on whether the reset rule fired", want.Error, err)
				}
			})
		}
	}
}
