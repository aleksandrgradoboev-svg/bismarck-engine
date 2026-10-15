package server_test

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
)

// The model authored these keys in this order, and no key is in sorted position
// at any depth — top level, inside a nested object, and inside an object in an
// array. Sorting anywhere shows up as different bytes.
const orderedArgumentsJSON = `{"path":"/tmp","depth":1,` +
	`"filters":[{"name":"go","enabled":true}],"nested":{"zeta":1,"alpha":2}}`

func orderedToolCall(t *testing.T) ai.ToolCall {
	t.Helper()
	arguments, order, err := ai.DecodeOrderedObject([]byte(orderedArgumentsJSON))
	if err != nil {
		t.Fatalf("DecodeOrderedObject: %v", err)
	}
	return ai.ToolCall{ID: "tc3", Name: "bash", Arguments: arguments, ArgumentsOrder: order}
}

// upstreamOrderedFrame is the frame pi itself produced for name, by running its
// own toProtocolAssistantMessage / toProtocolToolResultMessage and codec over
// the same arguments (see testdata/gen-messages.ts, orderedToolCalls).
func upstreamOrderedFrame(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../protocol/testdata/upstream_messages.json")
	if err != nil {
		t.Fatalf("read message vectors: %v", err)
	}
	var vectors struct {
		OrderedToolCalls []struct {
			Name  string `json:"name"`
			Frame string `json:"frame"`
			Error string `json:"error"`
		} `json:"orderedToolCalls"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse message vectors: %v", err)
	}
	for _, vector := range vectors.OrderedToolCalls {
		if vector.Name != name {
			continue
		}
		if vector.Error != "" {
			t.Fatalf("upstream failed to encode %q: %s", name, vector.Error)
		}
		return vector.Frame
	}
	t.Fatalf("no orderedToolCalls vector named %q", name)
	return ""
}

func itemFinishedFrame(t *testing.T, item protocol.TranscriptItem) string {
	t.Helper()
	frame, err := protocol.EncodeServerMessage(&protocol.EventEnvelope{
		Type: "event",
		Event: &protocol.SessionProgressEvent{
			Type: "session_progress", SessionID: "s1",
			Progress: &protocol.ItemFinishedProgress{Type: "item_finished", Item: item},
		},
	}, nil)
	if err != nil {
		t.Fatalf("EncodeServerMessage: %v", err)
	}
	return hex.EncodeToString(frame)
}

// TestAssistantToolCallArgumentOrderMatchesUpstream is the interop contract for
// the key order of a tool call's input: a Node peer reading this frame must see
// the keys the model wrote, in the order it wrote them.
func TestAssistantToolCallArgumentOrderMatchesUpstream(t *testing.T) {
	t.Parallel()
	message := ai.AssistantMessage{
		Content:    ai.ContentList{orderedToolCall(t)},
		Api:        "anthropic-messages",
		Provider:   "anthropic",
		Model:      "claude-opus-5",
		Usage:      ai.Usage{Input: 10, Output: 20, TotalTokens: 30, Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, Total: 0.3}},
		StopReason: ai.StopToolUse,
		Timestamp:  1007,
	}
	item, err := server.ToProtocolAssistantMessage(message, "a5")
	if err != nil {
		t.Fatalf("ToProtocolAssistantMessage: %v", err)
	}
	if got, want := itemFinishedFrame(t, item), upstreamOrderedFrame(t, "evt_item_finished_assistant"); got != want {
		t.Errorf("frame diverges from upstream\n got %s\nwant %s", got, want)
	}
}

// TestToolResultArgumentOrderMatchesUpstream pins the same order on the tool
// transcript item, which echoes the originating call's arguments.
func TestToolResultArgumentOrderMatchesUpstream(t *testing.T) {
	t.Parallel()
	call := orderedToolCall(t)
	result := ai.ToolResultMessage{
		ToolCallID: "tc3",
		ToolName:   "bash",
		Content:    ai.ContentList{ai.TextContent{Text: "out"}},
		Timestamp:  1008,
	}
	item, err := server.ToProtocolToolResultMessage(result, "t3", call)
	if err != nil {
		t.Fatalf("ToProtocolToolResultMessage: %v", err)
	}
	if got, want := itemFinishedFrame(t, item), upstreamOrderedFrame(t, "evt_item_finished_tool"); got != want {
		t.Errorf("frame diverges from upstream\n got %s\nwant %s", got, want)
	}
}

// TestOrderedJSONValueKeepsValidationSemantics: an ordered object is a new
// shape reaching a converter whose whole job is refusing what the protocol
// cannot carry, so it has to be walked by the same rules as a map.
func TestOrderedJSONValueKeepsValidationSemantics(t *testing.T) {
	t.Parallel()
	cycle := make(ai.OrderedObject, 1)
	cycle[0] = ai.OrderedField{Key: "self", Value: cycle}
	if _, err := server.ToProtocolJSONValue(cycle); err == nil ||
		err.Error() != "Protocol JSON values must not contain circular references" {
		t.Fatalf("err = %v, want the circular-reference rejection", err)
	}

	infinite := ai.OrderedObject{{Key: "n", Value: math.Inf(1)}}
	if _, err := server.ToProtocolJSONValue(infinite); err == nil ||
		err.Error() != "Protocol JSON numbers must be finite" {
		t.Fatalf("err = %v, want the finite-number rejection", err)
	}
}

// TestToolCallArgumentsWithoutOrderStillEncode covers the other arm of
// OrderedArguments: a call whose recorded order no longer matches its
// arguments falls back to the map, which the bridge still has to accept.
func TestToolCallArgumentsWithoutOrderStillEncode(t *testing.T) {
	t.Parallel()
	call := orderedToolCall(t)
	call.Arguments = map[string]any{"replaced": true}
	item, err := server.ToProtocolToolResultMessage(ai.ToolResultMessage{
		ToolCallID: "tc3", ToolName: "bash",
		Content:   ai.ContentList{ai.TextContent{Text: "out"}},
		Timestamp: 1008,
	}, "t3", call)
	if err != nil {
		t.Fatalf("ToProtocolToolResultMessage: %v", err)
	}
	input, ok := item.Input.(map[string]any)
	if !ok || input["replaced"] != true {
		t.Fatalf("input = %#v, want the replacement arguments", item.Input)
	}
}
