package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol/cbor"
)

// Port of packages/protocol/test/protocol.test.ts at 64eeb82a4, case for case
// and in its order, against the v8 envelopes in protocol.go.
//
// pi's tests hand parseClientMessage a JS object. The Go equivalent is the
// tree cbor.DecodeRaw produces for a frame, because that is the only form an
// opaque payload reaches this layer in: wire values below are hand-built maps
// encoded and decoded back through cbor, and messages the port itself builds
// go through the real encoder and message decoder.

const testServerID = ServerID("00000000-0000-4000-8000-000000000001")

// The hex below was produced by upstream pi's own encoder
// (packages/protocol/src/cbor at 64eeb82a4) under node, never by Go — the same
// vectors protocol/cbor/raw_test.go carries, with their provenance there:
//
//	call          = { serviceId: "application.custom", instance: { key: "instance-1", generation: 2 },
//	                  member: "invoke", args: [{ arbitrary: true }, ["opaque"]] }
//	request       = { type: "request", id: "request-1", target: { serverId, sessionId: "session-1",
//	                  attachmentId: "attachment-1" }, call }
//	voidResponse  = { type: "response", id: "request-1", ok: true }
//	update        = { applicationDefined: true, zeta: [{ m: 1.5, b: null }], alpha: "x" }
//	serviceUpdate = { type: "service_update", subscriptionId: "subscription-1", update }
var upstreamV8 = map[string]string{
	"call":          "a469736572766963654964726170706c69636174696f6e2e637573746f6d68696e7374616e6365a2636b65796a696e7374616e63652d316a67656e65726174696f6e02666d656d62657266696e766f6b65646172677382a169617262697472617279f581666f7061717565",
	"request":       "a46474797065677265717565737462696469726571756573742d3166746172676574a3687365727665724964782430303030303030302d303030302d343030302d383030302d3030303030303030303030316973657373696f6e49646973657373696f6e2d316c6174746163686d656e7449646c6174746163686d656e742d316463616c6ca469736572766963654964726170706c69636174696f6e2e637573746f6d68696e7374616e6365a2636b65796a696e7374616e63652d316a67656e65726174696f6e02666d656d62657266696e766f6b65646172677382a169617262697472617279f581666f7061717565",
	"voidResponse":  "a3647479706568726573706f6e736562696469726571756573742d31626f6bf5",
	"update":        "a3726170706c69636174696f6e446566696e6564f5647a65746181a2616dfb3ff80000000000006162f665616c7068616178",
	"serviceUpdate": "a364747970656e736572766963655f7570646174656e737562736372697074696f6e49646e737562736372697074696f6e2d3166757064617465a3726170706c69636174696f6e446566696e6564f5647a65746181a2616dfb3ff80000000000006162f665616c7068616178",
}

func upstreamV8Bytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(upstreamV8[name])
	if err != nil || len(raw) == 0 {
		t.Fatalf("bad upstream vector %q: %v", name, err)
	}
	return raw
}

// rawJSON encodes an in-process value into the opaque form a payload travels in.
func rawJSON(t *testing.T, value any) cbor.RawItem {
	t.Helper()
	encoded, err := cbor.Encode(value, nil)
	if err != nil {
		t.Fatalf("cbor.Encode(%#v): %v", value, err)
	}
	return cbor.RawItem(encoded)
}

// wireV8 turns a hand-built wire form into the tree the v8 parsers take: what
// a peer's bytes decode to, opaque keys captured as spans.
func wireV8(t *testing.T, value any) any {
	t.Helper()
	encoded, err := cbor.Encode(value, nil)
	if err != nil {
		t.Fatalf("cbor.Encode(%#v): %v", value, err)
	}
	decoded, err := cbor.DecodeRaw(encoded, nil, "call", "result", "update")
	if err != nil {
		t.Fatalf("cbor.DecodeRaw: %v", err)
	}
	return decoded
}

func serverTargetWire(serverID string) map[string]any {
	return map[string]any{"serverId": serverID}
}

func sessionTargetWire() map[string]any {
	return map[string]any{
		"serverId":     string(testServerID),
		"sessionId":    "session-1",
		"attachmentId": "attachment-1",
	}
}

func listModelsCall() map[string]any {
	return map[string]any{"serviceId": "pi.models", "member": "list", "args": []any{}}
}

func assertValidationError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want a ValidationError", what)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: %T %q, want *ValidationError", what, err, err)
	}
}

func assertClientRejected(t *testing.T, what string, value any) {
	t.Helper()
	_, err := ParseClientMessageV8(value)
	assertValidationError(t, what, err)
}

func assertServerRejected(t *testing.T, what string, value any) {
	t.Helper()
	_, err := ParseServerMessageV8(value)
	assertValidationError(t, what, err)
}

func parseClient(t *testing.T, value any) ClientMessageV8 {
	t.Helper()
	message, err := ParseClientMessageV8(value)
	if err != nil {
		t.Fatalf("ParseClientMessageV8: %v", err)
	}
	return message
}

func parseServer(t *testing.T, value any) ServerMessageV8 {
	t.Helper()
	message, err := ParseServerMessageV8(value)
	if err != nil {
		t.Fatalf("ParseServerMessageV8: %v", err)
	}
	return message
}

// roundTripClient encodes a message the port built and decodes it through the
// v8 message decoder, the whole outbound-then-inbound path.
func roundTripClient(t *testing.T, message ClientMessageV8) ClientMessageV8 {
	t.Helper()
	frame, err := EncodeClientMessageV8(message, nil)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8: %v", err)
	}
	decoder, err := NewClientMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewClientMessageDecoderV8: %v", err)
	}
	messages, err := decoder.Push(frame)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	return messages[0]
}

func roundTripServer(t *testing.T, message ServerMessageV8) ServerMessageV8 {
	t.Helper()
	frame, err := EncodeServerMessageV8(message, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8: %v", err)
	}
	decoder, err := NewServerMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoderV8: %v", err)
	}
	messages, err := decoder.Push(frame)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if err := decoder.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	return messages[0]
}

func assertSame(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

// "negotiates protocol version 8"
func TestV8NegotiatesProtocolVersion8(t *testing.T) {
	if ProtocolVersionV8 != 8 {
		t.Fatalf("ProtocolVersionV8 = %d, want 8", ProtocolVersionV8)
	}
	if !IsSupportedVersionV8(8) {
		t.Error("IsSupportedVersionV8(8) = false")
	}
	if IsSupportedVersionV8(7) {
		t.Error("IsSupportedVersionV8(7) = true")
	}
	// pi's isSupportedProtocolVersion(8.5) is false; a Go int64 cannot carry
	// 8.5, so the equivalent check is that a fractional version never
	// decodes into a hello at all.
	assertClientRejected(t, "hello version 8.5", wireV8(t, map[string]any{"type": "hello", "version": 8.5}))
	// The v1 constant is untouched by the additive phase.
	if ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d; the v1 constant must stay 1 until the cutover", ProtocolVersion)
	}
}

// "accepts integer client hello version %s for negotiation"
func TestV8AcceptsIntegerClientHelloVersions(t *testing.T) {
	for _, version := range []int64{0, ProtocolVersionV8, ProtocolVersionV8 + 1} {
		want := &ClientHelloV8{Type: "hello", Version: version}
		got := parseClient(t, wireV8(t, map[string]any{"type": "hello", "version": version}))
		assertSame(t, "parsed hello", got, want)
		assertSame(t, "round-tripped hello", roundTripClient(t, want), want)
	}
	assertSame(t, "NewClientHelloV8", NewClientHelloV8(), &ClientHelloV8{Type: "hello", Version: 8})
}

// "rejects an invalid client hello"
func TestV8RejectsInvalidClientHello(t *testing.T) {
	for _, test := range []struct {
		name  string
		value map[string]any
	}{
		{"string version", map[string]any{"type": "hello", "version": "8"}},
		{"fractional version", map[string]any{"type": "hello", "version": 8.5}},
		{"extra field", map[string]any{"type": "hello", "version": 8, "extra": true}},
	} {
		assertClientRejected(t, test.name, wireV8(t, test.value))
	}
	// The outbound path applies the same constraints to a hand-built hello.
	_, err := EncodeClientMessageV8(&ClientHelloV8{Type: "hello", Version: -1}, nil)
	assertValidationError(t, "negative version", err)
}

// "rejects non-canonical UUIDv4 server ID %j"
func TestV8RejectsNonCanonicalServerID(t *testing.T) {
	if !IsServerID(string(testServerID)) {
		t.Fatalf("IsServerID(%q) = false", testServerID)
	}
	for _, serverID := range []string{
		"",
		"server-1",
		"00000000-0000-7000-8000-000000000001", // version 7
		"00000000-0000-4000-7000-000000000001", // variant 7
		"00000000-0000-4000-8000-00000000000A", // uppercase
	} {
		if IsServerID(serverID) {
			t.Errorf("IsServerID(%q) = true", serverID)
		}
		assertClientRejected(t, "request to "+serverID, wireV8(t, map[string]any{
			"type":   "request",
			"id":     "request-1",
			"target": serverTargetWire(serverID),
			"call":   listModelsCall(),
		}))
		_, err := EncodeClientMessageV8(NewRequestV8("request-1", NewServerTarget(ServerID(serverID)), rawJSON(t, listModelsCall())), nil)
		assertValidationError(t, "encoding a request to "+serverID, err)
	}
	// The scan is exactly 36 bytes: a byte on either side of the length, and
	// a separator in the wrong place, are refused without a regexp to fall
	// back on.
	for _, serverID := range []string{
		"00000000-0000-4000-8000-00000000001",
		"00000000-0000-4000-8000-0000000000011",
		"00000000-00000-4000-8000-00000000001",
		"00000000-0000-4000-8000-00000000000g",
	} {
		if IsServerID(serverID) {
			t.Errorf("IsServerID(%q) = true", serverID)
		}
	}
}

// piServerIDPattern is pi's ServerIdSchema pattern, verbatim (protocol.ts at
// 49681e1b7). ASCII classes between anchors mean the same to RE2 as to V8.
var piServerIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// IsServerID is a hand-rolled scan of pi's pattern, so every byte position is
// checked against every byte value: the version nibble admits only 4, the
// variant nibble only 8, 9, a and b. Under node, pi's isServerId accepts
// exactly 489 of these 9,216 single-byte substitutions, and the digest is of
// that accepted set. (Its bytes above 0x7f are Latin-1 characters, not invalid
// UTF-8. Neither side accepts one, so the set is the same.)
func TestV8IsServerIDMatchesPiAtEveryPosition(t *testing.T) {
	base := string(testServerID)
	var accepted []string
	for position := range len(base) {
		for code := range 256 {
			candidate := base[:position] + string([]byte{byte(code)}) + base[position+1:]
			got := IsServerID(candidate)
			if want := piServerIDPattern.MatchString(candidate); got != want {
				t.Errorf("IsServerID(%q) = %v; pi's pattern says %v (byte %#02x at %d)", candidate, got, want, code, position)
			}
			if got {
				accepted = append(accepted, fmt.Sprintf("%d:%d", position, code))
			}
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(accepted, ",")))
	if len(accepted) != 489 || hex.EncodeToString(digest[:]) != "6541ad353b90b5182a3e9aaa68e1f7bc55c7dfc09403c93312b4d8d07aeaa0e8" {
		t.Errorf("accepted %d substitutions (sha256 %x); pi accepts 489 (sha256 6541ad35...)", len(accepted), digest)
	}
}

// "keeps routed request and event payloads opaque"
func TestV8KeepsRoutedPayloadsOpaque(t *testing.T) {
	call := cbor.RawItem(upstreamV8Bytes(t, "call"))
	request := &RequestEnvelopeV8{
		Type: "request",
		ID:   "request-1",
		Target: &SessionTarget{
			ServerID:     testServerID,
			SessionID:    "session-1",
			AttachmentID: "attachment-1",
		},
		Call: call,
	}
	assertSame(t, "round-tripped request", roundTripClient(t, request), request)

	// The payload leaves as the bytes it arrived in: the port's frame for
	// this request is byte for byte upstream's, authored key order included.
	frame, err := EncodeClientMessageV8(request, nil)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8: %v", err)
	}
	if got, want := frame[frameHeaderLength:], upstreamV8Bytes(t, "request"); !bytes.Equal(got, want) {
		t.Errorf("request payload diverges from upstream's bytes\n got %x\nwant %x", got, want)
	}

	// The call's service meaning belongs to Chord: any strict JSON passes.
	arbitrary := parseClient(t, wireV8(t, map[string]any{
		"type":   "request",
		"id":     "request-1",
		"target": sessionTargetWire(),
		"call":   map[string]any{"arbitrary": "strict JSON whose service meaning belongs to Chord"},
	})).(*RequestEnvelopeV8)
	decodedCall, err := cbor.Decode(arbitrary.Call, nil)
	if err != nil {
		t.Fatalf("cbor.Decode(call): %v", err)
	}
	if _, ok := decodedCall.(map[string]any)["arbitrary"].(string); !ok {
		t.Errorf("call decoded to %#v, want an object with a string \"arbitrary\"", decodedCall)
	}

	update := parseServer(t, wireV8(t, map[string]any{
		"type":           "service_update",
		"subscriptionId": "subscription-1",
		"update":         map[string]any{"applicationDefined": true},
	})).(*ServiceEventEnvelope)
	decodedUpdate, err := cbor.Decode(update.Update, nil)
	if err != nil {
		t.Fatalf("cbor.Decode(update): %v", err)
	}
	assertSame(t, "update", decodedUpdate, map[string]any{"applicationDefined": true})

	// And the same byte-exactness for a server-side opaque payload.
	event := &ServiceEventEnvelope{
		Type:           "service_update",
		SubscriptionID: "subscription-1",
		Update:         cbor.RawItem(upstreamV8Bytes(t, "update")),
	}
	assertSame(t, "round-tripped service update", roundTripServer(t, event), event)
	frame, err = EncodeServerMessageV8(event, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8: %v", err)
	}
	if got, want := frame[frameHeaderLength:], upstreamV8Bytes(t, "serviceUpdate"); !bytes.Equal(got, want) {
		t.Errorf("service_update payload diverges from upstream's bytes\n got %x\nwant %x", got, want)
	}
}

// "rejects non-JSON opaque payloads"
//
// pi feeds a Uint8Array, NaN, an undefined property and a cycle through
// isJsonValue. On the CBOR wire only the first can exist — the decoder refuses
// non-finite floats and the undefined simple value, and a cycle has no
// encoding — so the wire cases are byte strings, and the other three are
// refused where a Go peer would have to build the payload: cbor.Encode.
func TestV8RejectsNonJSONOpaquePayloads(t *testing.T) {
	byteArray := []byte{1}
	callWith := func(arg any) map[string]any {
		return map[string]any{"serviceId": "application.custom", "member": "invoke", "args": []any{arg}}
	}

	assertClientRejected(t, "byte array in call", wireV8(t, map[string]any{
		"type":   "request",
		"id":     "request-1",
		"target": serverTargetWire(string(testServerID)),
		"call":   callWith(byteArray),
	}))
	assertServerRejected(t, "byte array in result", wireV8(t, map[string]any{
		"type": "response", "id": "request-1", "ok": true, "result": byteArray,
	}))
	assertServerRejected(t, "byte array in update", wireV8(t, map[string]any{
		"type": "service_update", "subscriptionId": "subscription-1", "update": byteArray,
	}))

	// A byte string is a legal CBOR item, so it can be held in a RawItem; the
	// envelope refuses it on the way out too.
	_, err := EncodeClientMessageV8(NewRequestV8("request-1", NewServerTarget(testServerID), rawJSON(t, callWith(byteArray))), nil)
	assertValidationError(t, "encoding a byte array in call", err)
	_, err = EncodeServerMessageV8(&ResponseEnvelopeV8{Type: "response", ID: "request-1", OK: true, Result: rawJSON(t, byteArray)}, nil)
	assertValidationError(t, "encoding a byte array in result", err)

	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	for _, test := range []struct {
		name  string
		value any
	}{
		{"non-finite number", math.NaN()},
		{"cycle", cyclic},
	} {
		if _, err := cbor.Encode(callWith(test.value), nil); err == nil {
			t.Errorf("%s: cbor.Encode accepted it, so it could reach a RawItem", test.name)
		}
	}

	// A RawItem that is not one complete item is refused before its content
	// is judged: the zero value, and one item followed by another.
	for _, test := range []struct {
		name string
		raw  cbor.RawItem
	}{
		{"empty", cbor.RawItem{}},
		{"two items", append(rawJSON(t, 1), rawJSON(t, 2)...)},
	} {
		_, err := EncodeClientMessageV8(NewRequestV8("request-1", NewServerTarget(testServerID), test.raw), nil)
		assertValidationError(t, test.name+" RawItem", err)
	}

	// The parsers take a DecodeRaw tree; a payload that arrived as a decoded
	// tree instead is refused with the fix named, never silently re-encoded.
	plain, err := cbor.Decode(rawJSON(t, map[string]any{
		"type":   "request",
		"id":     "request-1",
		"target": serverTargetWire(string(testServerID)),
		"call":   listModelsCall(),
	}), nil)
	if err != nil {
		t.Fatalf("cbor.Decode: %v", err)
	}
	_, err = ParseClientMessageV8(plain)
	assertValidationError(t, "decoded-tree call", err)
	if !strings.Contains(err.Error(), "cbor.DecodeRaw") {
		t.Errorf("error does not say how to decode the frame: %q", err)
	}
}

// "validates request cancellation envelopes"
func TestV8ValidatesCancelEnvelopes(t *testing.T) {
	cancel := &CancelEnvelope{Type: "cancel", ID: "request-1", Target: NewServerTarget(testServerID)}
	wire := map[string]any{"type": "cancel", "id": "request-1", "target": serverTargetWire(string(testServerID))}
	assertSame(t, "parsed cancel", parseClient(t, wireV8(t, wire)), cancel)
	assertSame(t, "round-tripped cancel", roundTripClient(t, cancel), cancel)
	assertSame(t, "NewCancel", NewCancel("request-1", NewServerTarget(testServerID)), cancel)

	empty := map[string]any{"type": "cancel", "id": "", "target": serverTargetWire(string(testServerID))}
	assertClientRejected(t, "empty id", wireV8(t, empty))
	extra := map[string]any{"type": "cancel", "id": "request-1", "target": serverTargetWire(string(testServerID)), "extra": true}
	assertClientRejected(t, "extra field", wireV8(t, extra))

	_, err := EncodeClientMessageV8(&CancelEnvelope{Type: "cancel", ID: "", Target: NewServerTarget(testServerID)}, nil)
	assertValidationError(t, "encoding an empty id", err)
	_, err = EncodeClientMessageV8(&CancelEnvelope{Type: "cancel", ID: "request-1"}, nil)
	assertValidationError(t, "encoding without a target", err)
}

// "validates attachment route updates"
func TestV8ValidatesAttachmentRouteUpdates(t *testing.T) {
	attached := &AttachmentEnvelope{
		Type: "attachment",
		Attachment: &SessionTarget{
			ServerID:     testServerID,
			SessionID:    "session-1",
			AttachmentID: "attachment-1",
		},
	}
	detached := &AttachmentEnvelope{Type: "attachment", Attachment: nil}

	assertSame(t, "parsed attached", parseServer(t, wireV8(t, map[string]any{
		"type": "attachment", "attachment": sessionTargetWire(),
	})), attached)
	assertSame(t, "parsed detached", parseServer(t, wireV8(t, map[string]any{
		"type": "attachment", "attachment": nil,
	})), detached)
	assertSame(t, "round-tripped attached", roundTripServer(t, attached), attached)
	assertSame(t, "round-tripped detached", roundTripServer(t, detached), detached)

	// null is a value on the wire, not an omitted property.
	frame, err := EncodeServerMessageV8(detached, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8: %v", err)
	}
	if want := []byte{0xa2, 0x64, 't', 'y', 'p', 'e', 0x6a}; !bytes.HasPrefix(frame[frameHeaderLength:], want) || !bytes.HasSuffix(frame, []byte("attachment\xf6")) {
		t.Errorf("detached attachment is not a two-entry map ending in null: %x", frame[frameHeaderLength:])
	}

	assertServerRejected(t, "attachment with only sessionId", wireV8(t, map[string]any{
		"type": "attachment", "attachment": map[string]any{"sessionId": "session-1"},
	}))
	assertServerRejected(t, "attachment missing", wireV8(t, map[string]any{"type": "attachment"}))
	assertServerRejected(t, "attachment of the wrong type", wireV8(t, map[string]any{
		"type": "attachment", "attachment": "session-1",
	}))

	// A route the envelope carries is held to the session target's own
	// constraints in both directions. pi at 49681e1b7 refuses each of these
	// from encodeServerMessage and from its message decoder alike.
	for _, test := range []struct {
		name   string
		target SessionTarget
	}{
		{"invalid serverId", SessionTarget{ServerID: "server-1", SessionID: "session-1", AttachmentID: "attachment-1"}},
		{"version-7 serverId", SessionTarget{ServerID: "00000000-0000-7000-8000-000000000001", SessionID: "session-1", AttachmentID: "attachment-1"}},
		{"empty sessionId", SessionTarget{ServerID: testServerID, AttachmentID: "attachment-1"}},
		{"empty attachmentId", SessionTarget{ServerID: testServerID, SessionID: "session-1"}},
	} {
		assertServerRejected(t, "attachment with "+test.name, wireV8(t, map[string]any{
			"type": "attachment",
			"attachment": map[string]any{
				"serverId":     string(test.target.ServerID),
				"sessionId":    test.target.SessionID,
				"attachmentId": test.target.AttachmentID,
			},
		}))
		_, err := EncodeServerMessageV8(&AttachmentEnvelope{Type: "attachment", Attachment: &test.target}, nil)
		assertValidationError(t, "encoding an attachment with "+test.name, err)
	}
}

// "rejects malformed request boundaries: %s"
func TestV8RejectsMalformedRequestBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		value map[string]any
	}{
		{"empty request id", map[string]any{
			"type": "request", "id": "", "target": serverTargetWire(string(testServerID)), "call": listModelsCall(),
		}},
		{"extra envelope field", map[string]any{
			"type": "request", "id": "request-1", "target": serverTargetWire(string(testServerID)), "call": listModelsCall(), "extra": true,
		}},
		{"missing call", map[string]any{
			"type": "request", "id": "request-1", "target": serverTargetWire(string(testServerID)),
		}},
		{"target with a session field but no session", map[string]any{
			"type": "request", "id": "request-1", "call": listModelsCall(),
			"target": map[string]any{"serverId": string(testServerID), "attachmentId": "attachment-1"},
		}},
	} {
		assertClientRejected(t, test.name, wireV8(t, test.value))
	}
}

// "accepts a successful void response without a result field"
func TestV8AcceptsVoidResponseWithoutResult(t *testing.T) {
	void := &ResponseEnvelopeV8{Type: "response", ID: "request-1", OK: true}
	got := parseServer(t, wireV8(t, map[string]any{"type": "response", "id": "request-1", "ok": true}))
	assertSame(t, "parsed void response", got, void)
	if got.(*ResponseEnvelopeV8).Result != nil {
		t.Errorf("void response decoded with a result: %x", got.(*ResponseEnvelopeV8).Result)
	}
	assertSame(t, "round-tripped void response", roundTripServer(t, void), void)

	// Absence round-trips as absence: the outbound frame carries no result
	// field, byte for byte as upstream writes it.
	frame, err := EncodeServerMessageV8(void, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8: %v", err)
	}
	if got, want := frame[frameHeaderLength:], upstreamV8Bytes(t, "voidResponse"); !bytes.Equal(got, want) {
		t.Errorf("void response diverges from upstream's bytes\n got %x\nwant %x", got, want)
	}

	// Whereas a null result is a present JSON value, kept distinct from none.
	withNull := parseServer(t, wireV8(t, map[string]any{
		"type": "response", "id": "request-1", "ok": true, "result": nil,
	})).(*ResponseEnvelopeV8)
	if !bytes.Equal(withNull.Result, []byte{0xf6}) {
		t.Errorf("null result decoded as %x, want the null item f6", withNull.Result)
	}
	assertSame(t, "round-tripped null result", roundTripServer(t, withNull), withNull)
}

// "rejects malformed server boundaries: %s"
func TestV8RejectsMalformedServerBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		value map[string]any
	}{
		{"invalid server id", map[string]any{"type": "hello", "version": 8, "serverId": "server-1"}},
		{"wrong hello version", map[string]any{"type": "hello", "version": 7, "serverId": string(testServerID)}},
		{"extra response field", map[string]any{"type": "response", "id": "request-1", "ok": true, "result": []any{}, "extra": true}},
		{"empty error code", map[string]any{"type": "response", "id": "request-1", "ok": false, "error": map[string]any{"code": "", "message": "bad"}}},
		{"failed response without error", map[string]any{"type": "response", "id": "request-1", "ok": false}},
		{"failed response with result", map[string]any{"type": "response", "id": "request-1", "ok": false, "result": []any{}, "error": map[string]any{"code": "cancelled", "message": ""}}},
		{"successful response with error", map[string]any{"type": "response", "id": "request-1", "ok": true, "error": map[string]any{"code": "cancelled", "message": ""}}},
		{"error with details", map[string]any{"type": "hello_error", "error": map[string]any{"code": "version", "message": "bad", "details": map[string]any{}}}},
		{"empty subscription id", map[string]any{"type": "service_update", "subscriptionId": "", "update": map[string]any{}}},
	} {
		assertServerRejected(t, test.name, wireV8(t, test.value))
	}

	for _, test := range []struct {
		name    string
		message ServerMessageV8
	}{
		{"invalid server id", &ServerHelloV8{Type: "hello", Version: 8, ServerID: "server-1"}},
		{"wrong hello version", &ServerHelloV8{Type: "hello", Version: 7, ServerID: testServerID}},
		{"empty error code", &ResponseEnvelopeV8{Type: "response", ID: "request-1", Error: &ProtocolErrorV8{Message: "bad"}}},
		{"failed response with result", &ResponseEnvelopeV8{Type: "response", ID: "request-1", Result: rawJSON(t, []any{}), Error: &ProtocolErrorV8{Code: "cancelled"}}},
		{"successful response with error", &ResponseEnvelopeV8{Type: "response", ID: "request-1", OK: true, Error: &ProtocolErrorV8{Code: "cancelled"}}},
	} {
		_, err := EncodeServerMessageV8(test.message, nil)
		assertValidationError(t, "encoding "+test.name, err)
	}
	assertSame(t, "NewServerHelloV8", NewServerHelloV8(testServerID), &ServerHelloV8{Type: "hello", Version: 8, ServerID: testServerID})
}

// "accepts the opaque %s error code"
func TestV8AcceptsOpaqueErrorCodes(t *testing.T) {
	for _, code := range []string{"wrong_server", "cancelled", "service_not_found", "application_error", "anything_a_service_defines"} {
		message := &ResponseEnvelopeV8{
			Type: "response", ID: "request-1", OK: false,
			Error: &ProtocolErrorV8{Code: code, Message: "safe"},
		}
		assertSame(t, "parsed "+code, parseServer(t, wireV8(t, map[string]any{
			"type": "response", "id": "request-1", "ok": false,
			"error": map[string]any{"code": code, "message": "safe"},
		})), message)
		assertSame(t, "round-tripped "+code, roundTripServer(t, message), message)

		helloError := &ServerHelloErrorV8{Type: "hello_error", Error: ProtocolErrorV8{Code: code, Message: "safe"}}
		assertSame(t, "round-tripped hello_error "+code, roundTripServer(t, helloError), helloError)
	}
	if got := (&ProtocolErrorV8{Code: "wrong_server", Message: "fenced"}).Error(); got != "wrong_server: fenced" {
		t.Errorf("Error() = %q", got)
	}
}

// "rejects unknown messages and fields"
func TestV8RejectsUnknownMessagesAndFields(t *testing.T) {
	assertServerRejected(t, "hello with snapshot", wireV8(t, map[string]any{
		"type": "hello", "version": 8, "serverId": string(testServerID), "snapshot": map[string]any{},
	}))
	assertServerRejected(t, "unknown type", wireV8(t, map[string]any{"type": "unknown", "event": map[string]any{}}))
	// v1 shapes are not v8 shapes: the old event envelope and the old hello
	// with a connectionId are unknown here.
	assertServerRejected(t, "v1 event envelope", wireV8(t, map[string]any{"type": "event", "event": map[string]any{}}))
	assertClientRejected(t, "unknown client type", wireV8(t, map[string]any{"type": "steer"}))
	assertClientRejected(t, "missing type", wireV8(t, map[string]any{"version": 8}))
}

// The encoder is the only guard a Go-built envelope passes: a decoder picks the
// arm by the type literal and validates each struct it fills, so a wrong
// literal or a nested fault can only get out through Encode. pi's
// encodeClientMessage and encodeServerMessage (49681e1b7, under node) refuse
// every row here, and a frame the port sent instead would poison the peer's
// decoder. The zero Type is the realistic mistake: an envelope written as a
// struct literal rather than built by its constructor.
func TestV8EncodersRefuseWhatPiRefuses(t *testing.T) {
	call := rawJSON(t, listModelsCall())
	server := NewServerTarget(testServerID)
	session := &SessionTarget{ServerID: testServerID, SessionID: "session-1", AttachmentID: "attachment-1"}
	for _, test := range []struct {
		name    string
		message ClientMessageV8
	}{
		{"hello without a type", &ClientHelloV8{Version: 8}},
		{"hello typed as a request", &ClientHelloV8{Type: "request", Version: 8}},
		{"request without a type", &RequestEnvelopeV8{ID: "request-1", Target: server, Call: call}},
		{"request typed as a cancel", &RequestEnvelopeV8{Type: "cancel", ID: "request-1", Target: server, Call: call}},
		{"cancel without a type", &CancelEnvelope{ID: "request-1", Target: server}},
		{"cancel typed as a request", &CancelEnvelope{Type: "request", ID: "request-1", Target: server}},
	} {
		frame, err := EncodeClientMessageV8(test.message, nil)
		assertValidationError(t, "encoding a client "+test.name+fmt.Sprintf(" (produced %x)", frame), err)
		if !strings.Contains(err.Error(), "type must be") {
			t.Errorf("encoding a client %s: %q does not name the type", test.name, err)
		}
	}
	for _, test := range []struct {
		name, mention string
		message       ServerMessageV8
	}{
		{"hello without a type", "type must be", &ServerHelloV8{Version: 8, ServerID: testServerID}},
		{"hello typed as a response", "type must be", &ServerHelloV8{Type: "response", Version: 8, ServerID: testServerID}},
		{"hello_error without a type", "type must be", &ServerHelloErrorV8{Error: ProtocolErrorV8{Code: "busy", Message: "m"}}},
		{"hello_error typed as a response", "type must be", &ServerHelloErrorV8{Type: "response", Error: ProtocolErrorV8{Code: "busy", Message: "m"}}},
		{"hello_error with an empty code", "code", &ServerHelloErrorV8{Type: "hello_error", Error: ProtocolErrorV8{Message: "m"}}},
		{"response without a type", "type must be", &ResponseEnvelopeV8{ID: "request-1", OK: true, Result: rawJSON(t, []any{})}},
		{"response typed as a service_update", "type must be", &ResponseEnvelopeV8{Type: "service_update", ID: "request-1", OK: true, Result: rawJSON(t, []any{})}},
		{"service_update without a type", "type must be", &ServiceEventEnvelope{SubscriptionID: "subscription-1", Update: rawJSON(t, nil)}},
		{"service_update typed as a response", "type must be", &ServiceEventEnvelope{Type: "response", SubscriptionID: "subscription-1", Update: rawJSON(t, nil)}},
		{"attachment without a type", "type must be", &AttachmentEnvelope{Attachment: session}},
		{"attachment typed as a response", "type must be", &AttachmentEnvelope{Type: "response", Attachment: session}},
	} {
		frame, err := EncodeServerMessageV8(test.message, nil)
		assertValidationError(t, "encoding a server "+test.name+fmt.Sprintf(" (produced %x)", frame), err)
		if !strings.Contains(err.Error(), test.mention) {
			t.Errorf("encoding a server %s: %q does not mention %q", test.name, err, test.mention)
		}
	}
}

// "does not parse JSON strings as messages"
func TestV8DoesNotParseJSONStringsAsMessages(t *testing.T) {
	assertClientRejected(t, "client JSON string", `{"type":"hello","version":8}`)
	assertServerRejected(t, "server JSON string", `{"type":"hello","version":8,"serverId":"`+string(testServerID)+`"}`)
	assertClientRejected(t, "nil", nil)
	assertServerRejected(t, "array", []any{})
}

// "encodes complete client and server frames"
func TestV8EncodesCompleteClientAndServerFrames(t *testing.T) {
	clientFrame, err := EncodeClientMessageV8(NewClientHelloV8(), nil)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8: %v", err)
	}
	frames, err := mustFrameDecoder(t).Push(clientFrame)
	if err != nil || len(frames) != 1 {
		t.Fatalf("FrameDecoder.Push: %d frames, %v", len(frames), err)
	}
	decoded, err := cbor.DecodeRaw(frames[0], nil, "call")
	if err != nil {
		t.Fatalf("cbor.DecodeRaw: %v", err)
	}
	assertSame(t, "client hello", parseClient(t, decoded), NewClientHelloV8())

	serverFrame, err := EncodeServerMessageV8(NewServerHelloV8(testServerID), nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8: %v", err)
	}
	frames, err = mustFrameDecoder(t).Push(serverFrame)
	if err != nil || len(frames) != 1 {
		t.Fatalf("FrameDecoder.Push: %d frames, %v", len(frames), err)
	}
	decoded, err = cbor.DecodeRaw(frames[0], nil, "result", "update")
	if err != nil {
		t.Fatalf("cbor.DecodeRaw: %v", err)
	}
	assertSame(t, "server hello", parseServer(t, decoded), NewServerHelloV8(testServerID))

	// A nil message is refused, not dereferenced.
	_, err = EncodeClientMessageV8(nil, nil)
	assertValidationError(t, "nil client message", err)
	_, err = EncodeServerMessageV8(nil, nil)
	assertValidationError(t, "nil server message", err)
}

func mustFrameDecoder(t *testing.T) *FrameDecoder {
	t.Helper()
	decoder, err := NewFrameDecoder(nil)
	if err != nil {
		t.Fatalf("NewFrameDecoder: %v", err)
	}
	return decoder
}

// "enforces outbound frame limits"
func TestV8EnforcesOutboundFrameLimits(t *testing.T) {
	limit := 8
	opts := &FrameOptions{MaxFrameLength: &limit}
	_, err := EncodeClientMessageV8(NewClientHelloV8(), opts)
	assertValidationError(t, "client hello over the limit", err)
	_, err = EncodeServerMessageV8(NewServerHelloV8(testServerID), opts)
	assertValidationError(t, "server hello over the limit", err)
}

// A maxFrameLength above cbor's 16MB default admits an opaque payload above it,
// in both directions. pi at 49681e1b7, under node with maxFrameLength 40MB,
// encodes each message below with encodeClientMessage or encodeServerMessage and
// reads the frame back through its message decoder; the digests are of those
// frames. The payload's strict-JSON check re-reads its span, which the frame's
// limits have already bounded, so the re-read must not apply a limit of its own.
func TestV8OpaquePayloadsLargerThanTheCBORDefault(t *testing.T) {
	const mb = 1024 * 1024
	limit := 40 * mb
	opts := &FrameOptions{MaxFrameLength: &limit}
	raw := func(value any) cbor.RawItem {
		t.Helper()
		encoded, err := cbor.Encode(value, &cbor.Options{MaxByteLength: &limit})
		if err != nil {
			t.Fatalf("cbor.Encode: %v", err)
		}
		return cbor.RawItem(encoded)
	}
	text := strings.Repeat("x", 17*mb)
	// Twenty 1MB strings: no single string is over the default, only the span.
	chunks := make([]any, 20)
	for i := range chunks {
		chunks[i] = strings.Repeat(string(rune('a'+i)), mb)
	}
	request := func(call any) ClientMessageV8 {
		return NewRequestV8("request-1", NewServerTarget(testServerID), raw(call))
	}

	for _, test := range []struct {
		name    string
		message any
		sha256  string
	}{
		{"call_17MB_text", request(text), "5ac657c3ffda6c00e7d9cdabb34501dab231cff105b2ce32be7818e80bd20d9d"},
		{"call_20x1MB_array", request(chunks), "3c64ab46d57f4a59beea3ea62ac5c30d6f7f517c946a7a51bbc32102977719e7"},
		{"result_17MB_text", &ResponseEnvelopeV8{Type: "response", ID: "request-1", OK: true, Result: raw(text)},
			"304046914f1a624141f78f9d8f55f362f83b0e2a3aeef3e46e644f2ecd88c047"},
		{"update_17MB_text", &ServiceEventEnvelope{Type: "service_update", SubscriptionID: "subscription-1", Update: raw(map[string]any{"text": text})},
			"23f4549e10b2455d59cec573c1ad6c7718afc4c5b93aca267cb8c7f25f816d35"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// pi's frame, built without the envelope encoder under test.
			payload, err := cbor.Encode(test.message, &cbor.Options{MaxByteLength: &limit})
			if err != nil {
				t.Fatalf("cbor.Encode: %v", err)
			}
			frame, err := EncodeFrame(payload)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if digest := sha256.Sum256(frame); hex.EncodeToString(digest[:]) != test.sha256 {
				t.Fatalf("the fixture is not pi's frame: sha256 %x, want %s", digest, test.sha256)
			}

			var decoded []any
			var encoded []byte
			var decodeErr, encodeErr error
			switch message := test.message.(type) {
			case ClientMessageV8:
				decoder, err := NewClientMessageDecoderV8(opts)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := decoder.Push(frame)
				for _, m := range messages {
					decoded = append(decoded, m)
				}
				decodeErr = errors.Join(err, decoder.End())
				encoded, encodeErr = EncodeClientMessageV8(message, opts)
			case ServerMessageV8:
				decoder, err := NewServerMessageDecoderV8(opts)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := decoder.Push(frame)
				for _, m := range messages {
					decoded = append(decoded, m)
				}
				decodeErr = errors.Join(err, decoder.End())
				encoded, encodeErr = EncodeServerMessageV8(message, opts)
			}

			if decodeErr != nil {
				t.Errorf("decode: the port refuses pi's frame: %v", decodeErr)
			} else if len(decoded) != 1 || !reflect.DeepEqual(decoded[0], test.message) {
				t.Errorf("decode: got %d messages, want the one pi encoded", len(decoded))
			}
			if encodeErr != nil {
				t.Errorf("encode: the port refuses what pi encodes: %v", encodeErr)
			} else if !bytes.Equal(encoded, frame) {
				t.Errorf("encode: the frame diverges from pi's (sha256 %x)", sha256.Sum256(encoded))
			}
		})
	}

	// The frame's limit still binds. Under the default limit pi refuses to
	// encode the same request ("CBOR text string length exceeds configured
	// limit of 16777216"), and a default decoder refuses the 40MB frame.
	big := request(text)
	_, err := EncodeClientMessageV8(big, nil)
	assertValidationError(t, "a 17MB call under the default frame limit", err)
	// pi's text, from encodeProtocolMessage around its encoder's refusal: the
	// payload is too big, not malformed, so nothing names the RawItem the
	// port holds it in.
	if want := "Unable to encode client protocol message: CBOR text string length exceeds configured limit of 16777216"; err.Error() != want {
		t.Errorf("error %q, pi's is %q", err, want)
	}
	frame, err := EncodeClientMessageV8(big, opts)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8 at 40MB: %v", err)
	}
	decoder, err := NewClientMessageDecoderV8(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decoder.Push(frame)
	assertValidationError(t, "a 17MB frame under the default frame limit", err)
}

// "incrementally decodes fragmented and coalesced client messages"
func TestV8IncrementallyDecodesClientMessages(t *testing.T) {
	request := NewRequestV8("request-1", NewServerTarget(testServerID),
		rawJSON(t, map[string]any{"serviceId": "pi.session-directory", "member": "list", "args": []any{}}))
	first, err := EncodeClientMessageV8(NewClientHelloV8(), nil)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8(hello): %v", err)
	}
	second, err := EncodeClientMessageV8(request, nil)
	if err != nil {
		t.Fatalf("EncodeClientMessageV8(request): %v", err)
	}
	wire := append(append([]byte{}, first...), second...)
	want := []ClientMessageV8{NewClientHelloV8(), request}

	for split := 0; split <= len(wire); split++ {
		decoder, err := NewClientMessageDecoderV8(nil)
		if err != nil {
			t.Fatalf("NewClientMessageDecoderV8: %v", err)
		}
		head, err := decoder.Push(wire[:split])
		if err != nil {
			t.Fatalf("split %d: Push(head): %v", split, err)
		}
		tail, err := decoder.Push(wire[split:])
		if err != nil {
			t.Fatalf("split %d: Push(tail): %v", split, err)
		}
		if err := decoder.End(); err != nil {
			t.Fatalf("split %d: End: %v", split, err)
		}
		got := append(head, tail...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d:\n got %#v\nwant %#v", split, got, want)
		}
	}
}

// "incrementally decodes fragmented and coalesced server messages"
func TestV8IncrementallyDecodesServerMessages(t *testing.T) {
	response := &ResponseEnvelopeV8{Type: "response", ID: "request-1", OK: true, Result: rawJSON(t, []any{})}
	first, err := EncodeServerMessageV8(NewServerHelloV8(testServerID), nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8(hello): %v", err)
	}
	second, err := EncodeServerMessageV8(response, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessageV8(response): %v", err)
	}
	wire := append(append([]byte{}, first...), second...)

	split := len(first) + len(second)/2
	decoder, err := NewServerMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoderV8: %v", err)
	}
	head, err := decoder.Push(wire[:split])
	if err != nil {
		t.Fatalf("Push(head): %v", err)
	}
	assertSame(t, "head", head, []ServerMessageV8{NewServerHelloV8(testServerID)})
	tail, err := decoder.Push(wire[split:])
	if err != nil {
		t.Fatalf("Push(tail): %v", err)
	}
	assertSame(t, "tail", tail, []ServerMessageV8{response})
	if err := decoder.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
}

// "rejects invalid framed input: %s"
func TestV8RejectsInvalidFramedInput(t *testing.T) {
	mustEncodeFrame := func(payload []byte) []byte {
		t.Helper()
		frame, err := EncodeFrame(payload)
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		return frame
	}
	for _, test := range []struct {
		name string
		wire []byte
	}{
		{"empty CBOR payload", mustEncodeFrame(nil)},
		{"malformed CBOR", mustEncodeFrame([]byte{0xff})},
		{"schema-invalid CBOR", mustEncodeFrame(rawJSON(t, map[string]any{"type": "hello", "version": 1, "extra": true}))},
	} {
		decoder, err := NewClientMessageDecoderV8(nil)
		if err != nil {
			t.Fatalf("NewClientMessageDecoderV8: %v", err)
		}
		_, err = decoder.Push(test.wire)
		assertValidationError(t, test.name, err)

		valid, err := EncodeClientMessageV8(NewClientHelloV8(), nil)
		if err != nil {
			t.Fatalf("EncodeClientMessageV8: %v", err)
		}
		_, err = decoder.Push(valid)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "failed") {
			t.Errorf("%s: after failure Push returned %v, want a decoder-has-failed error", test.name, err)
		}
	}
}

// "rejects truncated and oversized framing"
func TestV8RejectsTruncatedAndOversizedFraming(t *testing.T) {
	truncated, err := NewServerMessageDecoderV8(nil)
	if err != nil {
		t.Fatalf("NewServerMessageDecoderV8: %v", err)
	}
	messages, err := truncated.Push([]byte{0, 0, 0, 2, 1})
	if err != nil || len(messages) != 0 {
		t.Fatalf("Push(partial): %d messages, %v", len(messages), err)
	}
	assertValidationError(t, "End on a partial frame", truncated.End())

	limit := 3
	oversized, err := NewClientMessageDecoderV8(&FrameOptions{MaxFrameLength: &limit})
	if err != nil {
		t.Fatalf("NewClientMessageDecoderV8: %v", err)
	}
	_, err = oversized.Push([]byte{0, 0, 0, 4})
	assertValidationError(t, "oversized frame header", err)

	// An invalid limit is the caller's bug, reported as a RangeError up front.
	negative := -1
	if _, err := NewClientMessageDecoderV8(&FrameOptions{MaxFrameLength: &negative}); !errors.As(err, new(*RangeError)) {
		t.Errorf("negative maxFrameLength: %v, want *RangeError", err)
	}
}

// The RpcTarget union is sealed: both arms report the server they are fenced
// to, and the server-wide arm is reachable only through NewServerTarget.
func TestV8RpcTargetArms(t *testing.T) {
	if got := NewServerTarget(testServerID).TargetServerID(); got != testServerID {
		t.Errorf("server target reports %q", got)
	}
	session := &SessionTarget{ServerID: testServerID, SessionID: "s", AttachmentID: "a"}
	if got := session.TargetServerID(); got != testServerID {
		t.Errorf("session target reports %q", got)
	}
	for _, test := range []struct {
		name   string
		target *SessionTarget
	}{
		{"empty sessionId", &SessionTarget{ServerID: testServerID, AttachmentID: "a"}},
		{"empty attachmentId", &SessionTarget{ServerID: testServerID, SessionID: "s"}},
		{"bad serverId", &SessionTarget{ServerID: "nope", SessionID: "s", AttachmentID: "a"}},
	} {
		_, err := EncodeClientMessageV8(NewCancel("request-1", test.target), nil)
		assertValidationError(t, test.name, err)
	}
	// A typed nil member is refused by name rather than dereferenced.
	_, err := EncodeClientMessageV8(NewCancel("request-1", (*SessionTarget)(nil)), nil)
	assertValidationError(t, "typed nil session target", err)
}
