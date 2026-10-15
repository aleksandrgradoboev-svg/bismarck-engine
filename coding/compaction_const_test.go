package coding

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// TestEstimateImageChars verifies the per-image token estimate uses pi's
// ESTIMATED_IMAGE_CHARS = 4800 (compaction.ts), exercised via contentChars
// and EstimateMessageTokens.
func TestEstimateImageChars(t *testing.T) {
	if estimatedImageChars != 4800 {
		t.Fatalf("estimatedImageChars = %d, want 4800", estimatedImageChars)
	}

	// contentChars: 4800 (image) + 5 (text "hello").
	content := ai.ContentList{
		ai.ImageContent{Data: "abc", MimeType: "image/png"},
		ai.TextContent{Text: "hello"},
	}
	if got := contentChars(content); got != 4805 {
		t.Fatalf("contentChars with one image+text = %d, want 4805", got)
	}

	// EstimateMessageTokens on a user message with a single image:
	// ceil(4800/4) = 1200 tokens.
	msg := ai.UserMessage{Content: ai.ContentList{ai.ImageContent{Data: "x", MimeType: "image/png"}}, Timestamp: 1}
	if got := EstimateMessageTokens(msg); got != 1200 {
		t.Fatalf("EstimateMessageTokens(image) = %d, want 1200 (ceil(4800/4))", got)
	}
}
