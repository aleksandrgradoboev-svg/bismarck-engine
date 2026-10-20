package harness

import (
	"context"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Generation compaction cases beyond pi: mechanisms its tests pass without.

// everCompactions are every compaction task the Session stored, terminal
// ones included.
func (c *compactionChat) everCompactions(t *testing.T) []durable.TaskRecord {
	t.Helper()
	var records []durable.TaskRecord
	must(t, c.h.ReadOnLine(func() error {
		page, err := c.h.Storage().ScanTasks(bg, durable.TaskQuery{Kind: compactionKind}, 100, nil)
		records = page.Items
		return err
	}))
	return records
}

// Without a cut neither a threshold nor an overflow starts a compaction at
// all, not even one that finds nothing.
func TestCompactionStartsNoCompactionWithoutACut(t *testing.T) {
	t.Run("background threshold", func(t *testing.T) {
		chat := openCompaction(t, compactionOpen{contextWindow: 2000})
		chat.history(t)
		chat.setup.setSettings(compactionPolicyOf(true, 500, 100_000, 1000))
		chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
		if records := chat.everCompactions(t); len(records) != 0 {
			t.Fatalf("compactions = %+v", records)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		chat := openCompaction(t, compactionOpen{})
		chat.turn(t, sizedText("u1", 100), sizedText("a1", 100))
		chat.setup.setSettings(func(s *HarnessSettings) {
			overflowEnabled(s)
			s.Compaction.KeepRecentTokens = ptr(100_000)
		})
		chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
		input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
		must(t, err)
		expectModelError(t, submissionWait(t, input), overflowText)
		if records := chat.everCompactions(t); len(records) != 0 {
			t.Fatalf("compactions = %+v", records)
		}
	})
}

// A second overflow after an overflow compaction fails without compacting
// again.
func TestCompactionOverflowSendsOneSummaryRequestForTwoOverflows(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	chat.faux.pushSummary(summaryStep("SUMMARY"), summaryStep("AGAIN"))
	// The kept part fits the budget that selected it, so the second overflow
	// would find no cut under the same policy; a smaller budget leaves one.
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)), func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		chat.setup.setSettings(func(s *HarnessSettings) { s.Compaction.KeepRecentTokens = ptr(1) })
		return fauxError(overflowText)
	})
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	expectModelError(t, submissionWait(t, input), overflowText)
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d, want 1", n)
	}
	if records := chat.everCompactions(t); len(records) != 1 {
		t.Fatalf("compactions = %d, want 1", len(records))
	}
}

func measuredAssistant(text string, input int) ai.Message {
	message := cutAssistant(text, nil, "").(ai.AssistantMessage)
	message.Usage = ai.Usage{Input: input}
	return message
}

// estimateContext measures from the newest answer with usage, skipping one
// without; adds the planned messages; and thresholdCompaction blocks only
// strictly above the threshold, counting the planned system entries.
func TestCompactionEstimateContextAndThresholds(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", measuredAssistant(sizedText("a1", 100), 1000)),
		c.entry("pi.user", cutUser(sizedText("u2", 100))),
		c.entry("pi.assistant", measuredAssistant(sizedText("a2", 100), 0)),
	}
	view := cutView(entries, nil)
	all := 0
	for _, message := range view.Messages {
		all += ai.EstimateMessageTokens(message)
	}
	// A hand-built view has no origins, so the measured answer is not found
	// in the context and every message is estimated after its usage.
	if got := estimateContext(view, nil); got != 1000+all {
		t.Fatalf("estimate = %d, want %d (the usage of a1, not of a2 without usage)", got, 1000+all)
	}
	extra := []ai.Message{cutUser(sizedText("planned", 100))}
	tokens := estimateContext(view, nil)
	if got := estimateContext(view, extra); got != tokens+ai.EstimateMessageTokens(extra[0]) {
		t.Fatalf("estimate with planned = %d, want %d", got, tokens+ai.EstimateMessageTokens(extra[0]))
	}
	policy := CompactionPolicy{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 1}
	if got := thresholdCompaction(view, nil, tokens+100, policy); got != "" {
		t.Fatalf("at the threshold = %q, want none", got)
	}
	if got := thresholdCompaction(view, nil, tokens+99, policy); got != "blocking" {
		t.Fatalf("above the threshold = %q, want blocking", got)
	}
	planned := []durable.EntryDraft{{Kind: durable.SystemEntry.Kind, Model: extra}}
	if got := thresholdCompaction(view, planned, tokens+100, policy); got != "blocking" {
		t.Fatalf("with planned entries = %q, want blocking", got)
	}
}

// A generation waiting for its overflow compaction shows no attempt in
// pi.live.
func TestCompactionOverflowWaitShowsNoGeneration(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
	_, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	generation := runTaskOf(t, chat.h, chat.root)
	if status := getTask(t, chat.h, generation).State.Status; status != durable.TaskWaiting {
		t.Fatalf("generation = %s", status)
	}
	if value, ok := chat.live(t)["generation"]; ok {
		t.Fatalf("pi.live generation = %v", value)
	}
	must(t, chat.root.Abort(bg, false))
}

// The end of a compaction that did not fail carries no error, even while
// another's failure is recorded.
func TestCompactionEndOfAnotherCompactionCarriesNoRecordedFailure(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	_, log := listen(t, chat.root)
	chat.faux.pushSummary(fauxStep(fauxError("bad request")))
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	eventually(t, func() bool { return chat.liveFailure(t) != nil })
	// Nothing to compact: it completes without a summary, so the failure
	// stays recorded.
	chat.setup.setSettings(manualPolicy(100_000))
	manual := chat.compact(t, nil)
	chat.result(t, manual)
	if chat.liveFailure(t) == nil {
		t.Fatal("the failure was removed")
	}
	var end *CompactionEndEvent
	eventually(t, func() bool {
		for _, event := range log.events() {
			if e, ok := event.(CompactionEndEvent); ok && e.TaskID == manual {
				end = &e
				return true
			}
		}
		return false
	})
	if end.Error != nil {
		t.Fatalf("compaction_end = %+v, want no error", end)
	}
}
