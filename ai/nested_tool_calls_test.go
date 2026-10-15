package ai

import (
	"encoding/json"
	"testing"
)

// Upstream 8562bcf66: a session pi wrote records nested calls on tool results,
// and the port carries them through byte for byte — nestedCalls last, where
// pi attaches it; a 0ms call's durationMs; arguments of every shape pi records
// (it records them before validating them), a null and {} included, with
// their keys in pi's order; and argumentsBytes for arguments omitted for size.
func TestNestedToolCallsRoundTrip(t *testing.T) {
	const written = `{"role":"toolResult","toolCallId":"codemode-1","toolName":"codemode","content":[],"isError":false,"timestamp":0,` +
		`"nestedCalls":{"calls":[` +
		`{"id":"codemode-1/1","name":"read","status":"ok","arguments":{"path":"a.ts","limit":5},"durationMs":0},` +
		`{"id":"codemode-1/2","name":"ls","status":"error","arguments":{},"durationMs":3,"error":"boom"},` +
		`{"id":"codemode-1/3","name":"bash","status":"error","arguments":"ls","durationMs":1},` +
		`{"id":"codemode-1/4","name":"bash","status":"error","arguments":[1,2],"durationMs":1},` +
		`{"id":"codemode-1/5","name":"bash","status":"error","arguments":7,"durationMs":1},` +
		`{"id":"codemode-1/6","name":"bash","status":"error","arguments":null,"durationMs":1},` +
		`{"id":"codemode-1/7","name":"write","status":"unfinished","argumentsBytes":40000}` +
		`],"complete":false}}`
	msg, err := UnmarshalMessage([]byte(written))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != written {
		t.Fatalf("round trip changed the message:\n got %s\nwant %s", out, written)
	}
}
