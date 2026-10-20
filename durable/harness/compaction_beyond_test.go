package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Compaction cases beyond pi: mechanisms its tests pass without.

// The summarizer reads the context at the pinned tail: an edit committed
// after selection, here by the hook itself, is not part of the summary
// (spec §12).
func TestCompactionSummarizesTheContextAtTheSelectedTailNotOneEditedAfterSelection(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	u1 := chatEntries(t, chat.root)[0]
	addCompactionHooks(t, chat.setup.registry, "redact", &CompactionHooks{BeforeCompact: func(ctx context.Context, _ CompactionRequest, _ *TaskRuntime) (*CompactionDecision, error) {
		_, err := chat.root.Submit(ctx, redactDraft(u1.ID))
		return nil, err
	}})
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	prompt := msgText(chat.faux.summaryRequest(t, 0).messages[1])
	if !strings.Contains(prompt, "[User]: u1 ") || strings.Contains(prompt, "REDACTED") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// A steer placed between a call and its result is summarized after the
// result, in model-context order.
func TestSerializeConversationOrdersAResultBeforeASteerThatPrecededIt(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser("u1")),
		c.entry("pi.assistant", cutAssistant("calling", []string{"c"}, "")),
		c.entry("pi.user", cutUser("steer")),
		c.entry("pi.tool-result", cutResult("c", "r")),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	serialized := serializeConversation(summarizedMessages(cutView(entries, nil), 4))
	if want := "[Assistant tool calls]: read()\n\n[Tool result]: r\n\n[User]: steer"; !strings.HasSuffix(serialized, want) {
		t.Fatalf("serialized = %q, want it to end with %q", serialized, want)
	}
}

// An answer with text and a tool call is no summary: pi's case sends the
// call without text, which also fails as empty text.
func TestCompactionFailsAnAnswerWithTextAndAToolCall(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	both := providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("SUMMARY"), providers.FauxToolCall("read", map[string]any{})}, ai.StopStop)
	chat.faux.pushSummary(providers.FauxStatic(both))
	outcome := chat.outcome(t, chat.compact(t, nil))
	if outcome.Status != durable.OutcomeFailed || outcome.Error == nil || outcome.Error.Message != "Summarization attempted to call a tool" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a tool-calling answer was placed as a summary")
	}
}
