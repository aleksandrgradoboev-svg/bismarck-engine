package cbor

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// The hex below was produced by upstream pi's own encoder
// (packages/protocol/src/cbor at 64eeb82a4) under node, never by Go:
//
//	import { encodeCbor } from "<upstream>/packages/protocol/src/cbor/index.ts";
//	const call = { serviceId: "application.custom", instance: { key: "instance-1", generation: 2 },
//	               member: "invoke", args: [{ arbitrary: true }, ["opaque"]] };
//	const request = { type: "request", id: "request-1",
//	                  target: { serverId: "00000000-0000-4000-8000-000000000001",
//	                            sessionId: "session-1", attachmentId: "attachment-1" }, call };
//	const result = { z: 1, a: 2, m: 3 };
//	const response = { type: "response", id: "request-1", ok: true, result };
//	const voidResponse = { type: "response", id: "request-1", ok: true };
//	const update = { applicationDefined: true, zeta: [{ m: 1.5, b: null }], alpha: "x" };
//	const serviceUpdate = { type: "service_update", subscriptionId: "subscription-1", update };
//	// hex(encodeCbor(x)) for each
//
// `call` is the opaque payload from upstream's protocol.test.ts ("keeps routed
// request and event payloads opaque"); `result` is the map_key_order vector,
// the one object the Go map encoder is known to re-order. Neither has sorted
// keys, which is the whole point: a peer relaying these must reproduce the
// authored order, and a Go map cannot.
var upstreamRaw = map[string]string{
	"call":          "a469736572766963654964726170706c69636174696f6e2e637573746f6d68696e7374616e6365a2636b65796a696e7374616e63652d316a67656e65726174696f6e02666d656d62657266696e766f6b65646172677382a169617262697472617279f581666f7061717565",
	"request":       "a46474797065677265717565737462696469726571756573742d3166746172676574a3687365727665724964782430303030303030302d303030302d343030302d383030302d3030303030303030303030316973657373696f6e49646973657373696f6e2d316c6174746163686d656e7449646c6174746163686d656e742d316463616c6ca469736572766963654964726170706c69636174696f6e2e637573746f6d68696e7374616e6365a2636b65796a696e7374616e63652d316a67656e65726174696f6e02666d656d62657266696e766f6b65646172677382a169617262697472617279f581666f7061717565",
	"result":        "a3617a01616102616d03",
	"response":      "a4647479706568726573706f6e736562696469726571756573742d31626f6bf566726573756c74a3617a01616102616d03",
	"voidResponse":  "a3647479706568726573706f6e736562696469726571756573742d31626f6bf5",
	"update":        "a3726170706c69636174696f6e446566696e6564f5647a65746181a2616dfb3ff80000000000006162f665616c7068616178",
	"serviceUpdate": "a364747970656e736572766963655f7570646174656e737562736372697074696f6e49646e737562736372697074696f6e2d3166757064617465a3726170706c69636174696f6e446566696e6564f5647a65746181a2616dfb3ff80000000000006162f665616c7068616178",
}

func upstreamBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(upstreamRaw[name])
	if err != nil || len(raw) == 0 {
		t.Fatalf("bad upstream vector %q: %v", name, err)
	}
	return raw
}

// Test-local models of upstream's envelopes, in upstream's property order.
type rawSessionTarget struct {
	ServerID     string `cbor:"serverId"`
	SessionID    string `cbor:"sessionId"`
	AttachmentID string `cbor:"attachmentId"`
}

type rawRequest struct {
	Type   string           `cbor:"type"`
	ID     string           `cbor:"id"`
	Target rawSessionTarget `cbor:"target"`
	Call   RawItem          `cbor:"call"`
}

type rawResponse struct {
	Type   string  `cbor:"type"`
	ID     string  `cbor:"id"`
	OK     bool    `cbor:"ok"`
	Result RawItem `cbor:"result,omitempty"`
}

type rawServiceUpdate struct {
	Type           string  `cbor:"type"`
	SubscriptionID string  `cbor:"subscriptionId"`
	Update         RawItem `cbor:"update"`
}

// TestDecodeRawCapturesDesignatedSpans: the designated top-level entries come
// back as the exact bytes upstream wrote for that item, and those bytes decode
// to the same value the item decodes to in place.
func TestDecodeRawCapturesDesignatedSpans(t *testing.T) {
	tests := []struct {
		frame, key, item string
	}{
		{"request", "call", "call"},
		{"response", "result", "result"},
		{"serviceUpdate", "update", "update"},
	}
	for _, test := range tests {
		t.Run(test.frame, func(t *testing.T) {
			frame := upstreamBytes(t, test.frame)
			decoded, err := DecodeRaw(frame, nil, "call", "result", "update")
			if err != nil {
				t.Fatalf("DecodeRaw: %v", err)
			}
			object, ok := decoded.(map[string]any)
			if !ok {
				t.Fatalf("decoded %T, want map[string]any", decoded)
			}
			raw, ok := object[test.key].(RawItem)
			if !ok {
				t.Fatalf("%q decoded as %T, want RawItem", test.key, object[test.key])
			}
			if want := upstreamBytes(t, test.item); !bytes.Equal(raw, want) {
				t.Errorf("span diverges from upstream's item bytes\n got %x\nwant %x", []byte(raw), want)
			}

			// The span is itself one complete item.
			fromSpan, err := Decode(raw, nil)
			if err != nil {
				t.Fatalf("Decode(span): %v", err)
			}
			plain, err := Decode(frame, nil)
			if err != nil {
				t.Fatalf("Decode(frame): %v", err)
			}
			if inPlace := plain.(map[string]any)[test.key]; !reflect.DeepEqual(fromSpan, inPlace) {
				t.Errorf("span decodes to\n %#v\nbut the item decodes in place to\n %#v", fromSpan, inPlace)
			}

			// Everything that was not designated is decoded as before.
			for key, value := range object {
				if key == test.key {
					continue
				}
				if !reflect.DeepEqual(value, plain.(map[string]any)[key]) {
					t.Errorf("undesignated %q changed: %#v", key, value)
				}
			}
		})
	}
}

// TestEncodeRawItemReproducesUpstreamBytes: relaying a captured span through a
// Go struct yields upstream's frame byte for byte — including the authored key
// order a Go map would have sorted away.
func TestEncodeRawItemReproducesUpstreamBytes(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		got, err := Encode(rawRequest{
			Type: "request", ID: "request-1",
			Target: rawSessionTarget{
				ServerID:     "00000000-0000-4000-8000-000000000001",
				SessionID:    "session-1",
				AttachmentID: "attachment-1",
			},
			Call: RawItem(upstreamBytes(t, "call")),
		}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if want := upstreamBytes(t, "request"); !bytes.Equal(got, want) {
			t.Errorf("frame diverges from upstream\n got %x\nwant %x", got, want)
		}
	})

	t.Run("response", func(t *testing.T) {
		got, err := Encode(rawResponse{
			Type: "response", ID: "request-1", OK: true,
			Result: RawItem(upstreamBytes(t, "result")),
		}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if want := upstreamBytes(t, "response"); !bytes.Equal(got, want) {
			t.Errorf("frame diverges from upstream\n got %x\nwant %x", got, want)
		}
		// The same payload through a Go map is the known divergence; the
		// RawItem is what closes it.
		sorted, err := Encode(map[string]any{"z": int64(1), "a": int64(2), "m": int64(3)}, nil)
		if err != nil {
			t.Fatalf("Encode(map): %v", err)
		}
		if bytes.Equal(sorted, upstreamBytes(t, "result")) {
			t.Fatal("map encoding now matches upstream order; this test no longer proves RawItem is needed")
		}
	})

	t.Run("void_response_omits_nil_result", func(t *testing.T) {
		got, err := Encode(rawResponse{Type: "response", ID: "request-1", OK: true}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if want := upstreamBytes(t, "voidResponse"); !bytes.Equal(got, want) {
			t.Errorf("frame diverges from upstream\n got %x\nwant %x", got, want)
		}
	})

	t.Run("service_update", func(t *testing.T) {
		got, err := Encode(rawServiceUpdate{
			Type: "service_update", SubscriptionID: "subscription-1",
			Update: RawItem(upstreamBytes(t, "update")),
		}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if want := upstreamBytes(t, "serviceUpdate"); !bytes.Equal(got, want) {
			t.Errorf("frame diverges from upstream\n got %x\nwant %x", got, want)
		}
	})
}

// TestRelayRoundTripIsByteExact is the whole reason RawItem exists: decode a
// peer's frame, re-emit it from a Go struct, and the bytes are the peer's.
func TestRelayRoundTripIsByteExact(t *testing.T) {
	frame := upstreamBytes(t, "request")
	decoded, err := DecodeRaw(frame, nil, "call")
	if err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	object := decoded.(map[string]any)
	target := object["target"].(map[string]any)
	relayed, err := Encode(rawRequest{
		Type: object["type"].(string),
		ID:   object["id"].(string),
		Target: rawSessionTarget{
			ServerID:     target["serverId"].(string),
			SessionID:    target["sessionId"].(string),
			AttachmentID: target["attachmentId"].(string),
		},
		Call: object["call"].(RawItem),
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(relayed, frame) {
		t.Errorf("relay is not byte-exact\n got %x\nwant %x", relayed, frame)
	}
}

// TestEncodeRawItemPassesThroughInAnyContainer: the passthrough has to be
// recognised wherever a value can sit, not only as a struct field.
func TestEncodeRawItemPassesThroughInAnyContainer(t *testing.T) {
	item := RawItem(upstreamBytes(t, "result"))
	tests := []struct {
		name  string
		value any
		want  string // hex prefix before the item bytes
	}{
		{"array", []any{item}, "81"},
		{"map", map[string]any{"k": item}, "a1616b"},
		{"ordered_object", OrderedObject{{Key: "k", Value: item}}, "a1616b"},
		{"pointer", &item, ""},
		{"interface_in_array", []any{any(item)}, "81"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Encode(test.value, nil)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			want := test.want + upstreamRaw["result"]
			if hex.EncodeToString(got) != want {
				t.Errorf("encoding = %s, want %s", hex.EncodeToString(got), want)
			}
		})
	}
}

// TestDecodeRawLeavesUndesignatedShapesAlone: capture is by top-level key only,
// so a same-named nested key and a non-map top level decode exactly as Decode
// would.
func TestDecodeRawLeavesUndesignatedShapesAlone(t *testing.T) {
	t.Run("nested_same_name_key", func(t *testing.T) {
		frame, err := Encode(map[string]any{"target": map[string]any{"call": int64(1)}, "id": "x"}, nil)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, err := DecodeRaw(frame, nil, "call")
		if err != nil {
			t.Fatalf("DecodeRaw: %v", err)
		}
		nested := decoded.(map[string]any)["target"].(map[string]any)["call"]
		if _, isRaw := nested.(RawItem); isRaw {
			t.Fatal("a nested key was captured; capture is top-level only")
		}
		if nested != any(int64(1)) {
			t.Errorf("nested call = %#v, want int64(1)", nested)
		}
	})

	t.Run("array_top_level", func(t *testing.T) {
		// [{"call": 1}]
		frame, _ := hex.DecodeString("81a16463616c6c01")
		decoded, err := DecodeRaw(frame, nil, "call")
		if err != nil {
			t.Fatalf("DecodeRaw: %v", err)
		}
		want := []any{map[string]any{"call": int64(1)}}
		if !reflect.DeepEqual(decoded, want) {
			t.Errorf("decoded %#v, want %#v", decoded, want)
		}
	})

	t.Run("no_keys", func(t *testing.T) {
		frame := upstreamBytes(t, "request")
		a, err := DecodeRaw(frame, nil)
		if err != nil {
			t.Fatalf("DecodeRaw: %v", err)
		}
		b, err := Decode(frame, nil)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Error("DecodeRaw with no keys differs from Decode")
		}
	})
}

// TestDecodeRawStillValidatesTheSpan: a captured item is still read under the
// decoder's rules and limits, so malformed bytes inside a designated field are
// rejected exactly as they would be anywhere else. A span that is not
// readable must never be captured and relayed onward.
func TestDecodeRawStillValidatesTheSpan(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		opts *Options
		err  string
	}{
		{
			name: "duplicate_key_inside_span",
			hex:  "a16463616c6ca2616101616102", // {"call": {"a":1,"a":2}}
			err:  "CBOR map contains a duplicate key",
		},
		{
			name: "truncated_span",
			hex:  "a16463616c6c8201", // {"call": [1, <missing>]}
			err:  "Truncated CBOR payload",
		},
		{
			name: "invalid_utf8_inside_span",
			hex:  "a16463616c6c62c328",
			err:  "CBOR text string contains invalid UTF-8",
		},
		{
			name: "depth_counts_from_the_frame",
			// {"call": [[[1]]]} with maxDepth 3: the innermost array is at
			// depth 4 relative to the frame, though only 3 inside the span.
			hex:  "a16463616c6c83818101",
			opts: &Options{MaxDepth: ptr(3)},
			err:  "CBOR nesting depth exceeds configured limit of 3",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame, err := hex.DecodeString(test.hex)
			if err != nil {
				t.Fatalf("bad test hex: %v", err)
			}
			got, err := DecodeRaw(frame, test.opts, "call")
			if err == nil {
				t.Fatalf("DecodeRaw accepted %s and produced %#v", test.hex, got)
			}
			if err.Error() != test.err {
				t.Errorf("error text\n got %q\nwant %q", err.Error(), test.err)
			}
		})
	}
}

// TestDecodeRawSpanDoesNotAliasInput: the transport reuses its frame buffer,
// so a span that shared it would change under the caller's feet — the same
// rule the decoder already applies to byte strings.
func TestDecodeRawSpanDoesNotAliasInput(t *testing.T) {
	frame := upstreamBytes(t, "response")
	decoded, err := DecodeRaw(frame, nil, "result")
	if err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	raw := decoded.(map[string]any)["result"].(RawItem)
	before := bytes.Clone(raw)
	for i := range frame {
		frame[i] = 0xff
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("RawItem aliases the input buffer")
	}
}

// TestEncodeRejectsUnreadableRawItem: the encoder writes a RawItem verbatim, so
// it is the one place a corrupt frame could be produced. A frame the decoder
// would refuse is unreadable by every peer including us — and a peer's message
// decoder fails permanently on the first bad frame — so the encoder refuses
// first. The zero value is the realistic case: a required RawItem field left
// unset would otherwise vanish from the wire and shift every byte after it.
func TestEncodeRejectsUnreadableRawItem(t *testing.T) {
	tests := []struct {
		name  string
		value any
		opts  *Options
		want  string
	}{
		{
			name:  "nil",
			value: rawRequest{Type: "request", ID: "r", Call: nil},
			want:  "RawItem must not be empty",
		},
		{
			name:  "empty",
			value: RawItem{},
			want:  "RawItem must not be empty",
		},
		{
			name:  "trailing_data",
			value: RawItem{0x01, 0x01},
			want:  "CBOR payload contains trailing data",
		},
		{
			name:  "truncated",
			value: RawItem{0x82, 0x01},
			want:  "Truncated CBOR payload",
		},
		{
			name:  "indefinite_length",
			value: RawItem{0x9f, 0x01, 0xff},
			want:  "Indefinite-length CBOR arrays are not supported",
		},
		{
			name:  "duplicate_key",
			value: RawItem{0xa2, 0x61, 0x61, 0x01, 0x61, 0x61, 0x02},
			want:  "CBOR map contains a duplicate key",
		},
		{
			name: "too_deep_for_its_position",
			// [[1]] is fine alone under maxDepth 2 but not one level down.
			value: []any{RawItem{0x81, 0x81, 0x01}},
			opts:  &Options{MaxDepth: ptr(2)},
			want:  "CBOR nesting depth exceeds configured limit of 2",
		},
		{
			name:  "too_long_a_string",
			value: RawItem{0x63, 'a', 'b', 'c'},
			opts:  &Options{MaxByteLength: ptr(2)},
			want:  "CBOR text string length exceeds configured limit of 2",
		},
	}
	// A limit exceeded is not a malformed item: pi's encoder, encoding the
	// value the bytes hold, refuses it in the same words, so they come out
	// bare. Anything else is the RawItem's own fault and says so.
	limits := map[string]bool{"too_deep_for_its_position": true, "too_long_a_string": true}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Encode(test.value, test.opts)
			if err == nil {
				t.Fatalf("Encode accepted an unreadable RawItem and produced %x", got)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error text\n got %q\nwant it to contain %q", err.Error(), test.want)
			}
			switch {
			case limits[test.name] && err.Error() != test.want:
				t.Errorf("a limit exceeded reads %q, pi's encoder says %q", err, test.want)
			case !limits[test.name] && !strings.HasPrefix(test.want, "RawItem must not be empty") &&
				err.Error() != "RawItem must hold exactly one readable CBOR item: "+test.want:
				t.Errorf("a malformed RawItem reads %q, want the RawItem's own wording around %q", err, test.want)
			}
			if _, isCbor := err.(*Error); !isCbor {
				t.Errorf("error is %T, want *Error", err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
