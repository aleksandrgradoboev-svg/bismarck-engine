package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Overflow compaction (pi harness-compaction.test.ts, "overflow compaction").

const overflowText = "prompt is too long: 250000 tokens > 200000 maximum"

// overflowEnabled is the manual thresholds with compaction enabled.
func overflowEnabled(s *HarnessSettings) {
	manualPolicy(150)(s)
	s.Compaction.Enabled = ptr(true)
}

func expectModelError(t *testing.T, settled durable.SubmissionRecord, detail string) {
	t.Helper()
	var got string
	_ = json.Unmarshal(settled.Detail, &got)
	if settled.Status != durable.SubmissionUnanswered || settled.Reason != "model_error" || (detail != "" && got != detail) {
		t.Fatalf("settled = %+v (%s), want model_error %q", settled, settled.Detail, detail)
	}
}

func TestCompactionOverflowCompactsAndRetriesWithTheSameAttemptLeavingTheErrorOutOfTheRetry(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	var attempt any
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)), func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		if generation, ok := chat.live(t)["generation"].(map[string]any); ok {
			attempt = generation["attempt"]
		}
		return fauxAnswer("fits")
	})
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if attempt != float64(1) {
		t.Fatalf("attempt = %v, want 1", attempt)
	}
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-5:]; !reflect.DeepEqual(got, []string{"pi.user", "pi.assistant", "pi.compaction", "pi.system", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
	for _, record := range chatEntries(t, chat.root) {
		if record.Kind == "pi.compaction" && string(record.Data) != `{"reason":"overflow"}` {
			t.Fatalf("summary data = %s", record.Data)
		}
	}
	retry := chat.faux.lastAgentRequest(t).messages
	if retry[0].MessageRole() != ai.RoleSystem || !strings.Contains(msgText(retry[1]), "SUMMARY") {
		t.Fatalf("retry = %v", retry[:2])
	}
	for _, message := range retry {
		if assistant, ok := message.(ai.AssistantMessage); ok && assistant.StopReason == ai.StopError {
			t.Fatal("the retry carries the error")
		}
	}
}

func TestCompactionOverflowFailsASecondOverflowWithItsErrorEntry(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)), fauxStep(fauxError(overflowText)))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	expectModelError(t, submissionWait(t, input), overflowText)
	count := 0
	kinds := chat.kinds(t)
	for _, kind := range kinds {
		if kind == "pi.compaction" {
			count++
		}
	}
	if count != 1 || kinds[len(kinds)-1] != "pi.assistant" {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestCompactionOverflowFailsWithoutCompactingWhenCompactionIsDisabledEvenForARetryableLookingOverflow(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.faux.pushAgent(fauxStep(fauxError("overloaded: " + overflowText)))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	expectModelError(t, submissionWait(t, input), "")
	if n := chat.agentRequestCount(); n != 4 {
		t.Fatalf("agent requests = %d, want 4", n)
	}
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
}

func TestCompactionOverflowFailsAfterABlockingThresholdCompactionInTheSameGeneration(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	expectModelError(t, submissionWait(t, input), "")
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
}

func TestCompactionOverflowFailsWithTheOverflowText(t *testing.T) {
	for _, c := range []struct {
		name     string
		prepare  func(t *testing.T, chat *compactionChat)
		requests int
	}{
		{"when compaction declines", func(t *testing.T, chat *compactionChat) {
			addCompactionHooks(t, chat.setup.registry, "decline", decide(&CompactionDecision{Decline: true}))
		}, 0},
		{"when compaction fails", func(t *testing.T, chat *compactionChat) {
			chat.faux.pushSummary(fauxStep(fauxError("bad request")))
		}, 1},
		// Classification finds no cut, so no compaction starts and the
		// ordinary failure carries the text.
		{"when compaction cannot cut", func(t *testing.T, chat *compactionChat) {
			chat.setup.setSettings(func(s *HarnessSettings) { s.Compaction.KeepRecentTokens = ptr(100_000) })
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{})
			chat.turn(t, sizedText("u1", 100), sizedText("a1", 100))
			chat.setup.setSettings(overflowEnabled)
			c.prepare(t, chat)
			chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
			input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
			must(t, err)
			expectModelError(t, submissionWait(t, input), overflowText)
			if n := chat.faux.summaryCount(); n != c.requests {
				t.Fatalf("summary requests = %d, want %d", n, c.requests)
			}
		})
	}
}

// A request-size 413 is an overflow too (pi 73f8de80): Copilot's carries no
// token counts, and the generation still compacts and retries. pi's case for
// it is in the coding agent (regressions/copilot-request-size-overflow); this
// is the harness's path for the same answer.
func TestCompactionOverflowCompactsAndRetriesOnARequestSize413(t *testing.T) {
	const copilot413 = `github-copilot API error (413): {"message":"failed to parse request","code":""}`
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxStep(fauxError(copilot413)), fauxStep(fauxAnswer("fits")))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	compacted := false
	for _, record := range chatEntries(t, chat.root) {
		if record.Kind == "pi.compaction" {
			compacted = string(record.Data) == `{"reason":"overflow"}`
		}
	}
	if !compacted {
		t.Fatalf("no overflow compaction: kinds = %v", chat.kinds(t))
	}
}
