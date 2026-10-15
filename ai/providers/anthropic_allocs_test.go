package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// anthropicTextDeltaStream is a stream of one text block with deltas
// text_delta events.
func anthropicTextDeltaStream(deltas int) string {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
	b.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for i := range deltas {
		fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"word %d \"}}\n\n", i)
	}
	b.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	b.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":200}}\n\n")
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

// TestAnthropicTextDeltaAllocations locks what a text_delta costs on the
// primary adapter's hot path with no observer. Each event is read once: one
// validating scan, then its members and its delta's split out of the event
// text without copying them (decodeRawObject), its type and the delta's type
// compared without allocating, the block index compared as text, the delta
// text parsed once — so the pushed event (a partial message clone and its
// content) is most of what a delta allocates. When this was written a delta
// cost 20.1 allocations (21.5 under -race); decoding members with
// encoding/json instead cost 37, and before that fix 44.1. If it fails,
// profile BenchmarkAnthropic-style streaming (go test -memprofile) for the
// read that came back.
func TestAnthropicTextDeltaAllocations(t *testing.T) {
	model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: "http://anthropic.invalid", MaxTokens: 4096}
	transcript := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}})
	stream := func(deltas int) float64 {
		body := anthropicTextDeltaStream(deltas)
		return testing.AllocsPerRun(20, func() {
			opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", HTTPClient: cannedDoer{status: http.StatusOK, body: body}}}
			s := StreamAnthropic(context.Background(), model, transcript, &AnthropicOptions{StreamOptions: opts})
			for range s.Events() {
			}
			if r := s.Result(); r.StopReason != ai.StopStop {
				t.Fatalf("stopReason = %s (%s)", r.StopReason, r.ErrorMessage)
			}
		})
	}
	const deltas = 100
	perDelta := (stream(deltas+10) - stream(10)) / deltas
	t.Logf("%.1f allocations per text_delta", perDelta)
	if perDelta > 24 {
		t.Fatalf("a text_delta allocates %.1f times, want at most 24: an event is being decoded, copied or parsed more than once", perDelta)
	}
}
