package providers

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// piMessagesHoleStream is a stream that starts text blocks at each of starts,
// appends "a" to the last one and finishes.
func piMessagesHoleStream(starts ...int) string {
	frames := []string{`{"type":"start"}`}
	for _, at := range starts {
		frames = append(frames, fmt.Sprintf(`{"type":"text_start","contentIndex":%d}`, at))
	}
	last := starts[len(starts)-1]
	frames = append(frames,
		fmt.Sprintf(`{"type":"text_delta","contentIndex":%d,"delta":"a"}`, last),
		fmt.Sprintf(`{"type":"text_end","contentIndex":%d,"content":"a"}`, last),
		`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`)
	return piMessagesSSE(frames...)
}

// "rejects content indexes that are not the next or an existing block" (pi
// 89c921d8): the index comes from the wire and keys the content array, so a
// start may add only the next block and any other event needs an existing one.
func TestPiMessagesRejectsContentIndexesThatAreNotTheNextOrAnExistingBlock(t *testing.T) {
	for name, frames := range map[string][]string{
		"far start":             {`{"type":"text_start","contentIndex":4294967000}`},
		"__proto__ start":       {`{"type":"text_start","contentIndex":"__proto__"}`},
		"__proto__ end":         {`{"type":"text_start","contentIndex":0}`, `{"type":"toolcall_end","contentIndex":"__proto__","toolCall":{"polluted":true}}`},
		"delta without a block": {`{"type":"text_delta","contentIndex":0,"delta":"x"}`},
		// Beyond pi's suite: Number.isInteger rejects a fractional index.
		"fractional start": {`{"type":"text_start","contentIndex":0.5}`},
	} {
		t.Run(name, func(t *testing.T) {
			all := append([]string{`{"type":"start"}`}, frames...)
			all = append(all, `{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`)
			_, _, final := streamPiMessagesEvents(t, t.Context(), piMessagesSSE(all...), ai.StreamOptions{})
			if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "Invalid contentIndex") {
				t.Fatalf("stop = %s (%q), want an Invalid contentIndex error", final.StopReason, final.ErrorMessage)
			}
			if len(final.Content) > 1 {
				t.Fatalf("content = %v, want at most one block", final.Content)
			}
		})
	}
}

// TestPiMessagesFarContentIndexFailsFast requires a frame naming a slot far
// past the content to fail the stream at once, without growing the content
// toward it. The allocation bound is what proves it; a wall-clock bound would
// only add a flake under -race on a loaded machine.
func TestPiMessagesFarContentIndexFailsFast(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, final := streamPiMessagesEvents(t, t.Context(), piMessagesHoleStream(10_000_000), ai.StreamOptions{})
	runtime.ReadMemStats(&after)
	if final.StopReason != ai.StopError {
		t.Errorf("stopReason = %s, want error: slot 10000000 is past the content", final.StopReason)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Errorf("streaming allocated %d MB for one far contentIndex; the content must not grow toward it", allocated>>20)
	}
}

// pi-messages terminal events carry responseModel (pi 14c81184, #11265): done
// and error copy it onto the message, and a done without one leaves none.
func TestPiMessagesTerminalEventsCarryResponseModel(t *testing.T) {
	done := `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseId":"resp_1","responseModel":"claude-opus-5-5","providerThinkingLevel":"high"}`
	_, _, final := streamPiMessagesEvents(t, t.Context(), piMessagesSSE(`{"type":"start"}`, done), ai.StreamOptions{})
	if final.ResponseModel != "claude-opus-5-5" || final.Model != "auto" || final.ProviderThinkingLevel != "high" {
		t.Fatalf("done: responseModel %q model %q level %q", final.ResponseModel, final.Model, final.ProviderThinkingLevel)
	}

	plain := `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseId":"resp_1"}`
	_, _, final = streamPiMessagesEvents(t, t.Context(), piMessagesSSE(`{"type":"start"}`, plain), ai.StreamOptions{})
	if final.ResponseModel != "" {
		t.Fatalf("done without one: responseModel %q, want none", final.ResponseModel)
	}

	failed := `{"type":"error","reason":"error","usage":` + piMessagesUsageJSON + `,"errorMessage":"Upstream failed","responseModel":"claude-opus-5-5"}`
	_, _, final = streamPiMessagesEvents(t, t.Context(), piMessagesSSE(`{"type":"start"}`, failed), ai.StreamOptions{})
	if final.StopReason != ai.StopError || final.ErrorMessage != "Upstream failed" || final.ResponseModel != "claude-opus-5-5" {
		t.Fatalf("error: stop %s %q responseModel %q", final.StopReason, final.ErrorMessage, final.ResponseModel)
	}
}
