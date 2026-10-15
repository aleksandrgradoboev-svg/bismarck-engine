package server_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
)

// testdata/response-model-48bfdbaff.json is pi's own toProtocolAssistantMessage
// and encodeServerMessage run over each case's message by
// testdata/capture-response-model.mts, at the last upstream commit carrying
// server/src/protocol.ts.
const responseModelCaptureFile = "testdata/response-model-48bfdbaff.json"

// TestAssistantResponseModelReachesTheWire pins the responseModel arm of
// ToProtocolAssistantMessage against pi. Since upstream 1283afd0d the Anthropic
// adapter keeps the requested id as the message's model and records a relay's
// relabel or a refusal fallback only as responseModel, so this arm is the one
// path either reaches a protocol client by.
func TestAssistantResponseModelReachesTheWire(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(responseModelCaptureFile)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var capture struct {
		Cases []struct {
			Name     string          `json:"name"`
			Progress string          `json:"progress"`
			Message  json.RawMessage `json:"message"`
			Item     struct {
				ResponseModel *string `json:"responseModel"`
			} `json:"item"`
			Frame string `json:"frame"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatalf("parse capture: %v", err)
	}
	if len(capture.Cases) == 0 {
		t.Fatalf("%s has no cases; rerun capture-response-model.mts", responseModelCaptureFile)
	}
	for _, c := range capture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			decoded, err := ai.UnmarshalMessage(c.Message)
			if err != nil {
				t.Fatalf("decode message: %v", err)
			}
			message, ok := decoded.(ai.AssistantMessage)
			if !ok {
				t.Fatalf("message decoded as %T, want ai.AssistantMessage", decoded)
			}
			item, err := server.ToProtocolAssistantMessage(message, "a1")
			if err != nil {
				t.Fatalf("ToProtocolAssistantMessage: %v", err)
			}

			switch want := c.Item.ResponseModel; {
			case want == nil && item.ResponseModel != nil:
				t.Errorf("responseModel = %q, want it omitted", *item.ResponseModel)
			case want != nil && (item.ResponseModel == nil || *item.ResponseModel != *want):
				t.Errorf("responseModel = %v, want %q", item.ResponseModel, *want)
			}
			if got := progressFrame(t, c.Progress, item); got != c.Frame {
				t.Errorf("frame diverges from pi\n got %s\nwant %s", got, c.Frame)
			}
			assertServerPayload(t, item)
		})
	}
}

// progressFrame encodes item inside the session_progress event pi's capture
// used: item_updated or item_finished.
func progressFrame(t *testing.T, progress string, item protocol.TranscriptItem) string {
	t.Helper()
	switch progress {
	case "item_finished":
		return itemFinishedFrame(t, item)
	case "item_updated":
		frame, err := protocol.EncodeServerMessage(&protocol.EventEnvelope{
			Type: "event",
			Event: &protocol.SessionProgressEvent{
				Type: "session_progress", SessionID: "s1",
				Progress: &protocol.ItemUpdatedProgress{Type: "item_updated", Item: item},
			},
		}, nil)
		if err != nil {
			t.Fatalf("EncodeServerMessage: %v", err)
		}
		return hex.EncodeToString(frame)
	default:
		t.Fatalf("unknown progress %q in %s", progress, responseModelCaptureFile)
		return ""
	}
}
