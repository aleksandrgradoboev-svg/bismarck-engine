package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Compaction outcomes (pi harness-compaction.test.ts, "compaction outcomes").

func addCompactionHooks(t *testing.T, registry Registry, name string, hooks *CompactionHooks) {
	t.Helper()
	must(t, registry.Install(&Extension{Name: "compaction-hooks:" + name, Hooks: []HookRegistration{Hook(CompactionTask, hooks)}}))
}

func decide(decision *CompactionDecision) *CompactionHooks {
	return &CompactionHooks{BeforeCompact: func(context.Context, CompactionRequest, *TaskRuntime) (*CompactionDecision, error) {
		return decision, nil
	}}
}

// compactionInput is the input tokens run added to the ledger.
func (c *compactionChat) compactionInput(t *testing.T, run func()) int {
	t.Helper()
	before := c.modelUsage(t).Input
	run()
	return c.modelUsage(t).Input - before
}

func TestCompactionCompletesWithoutASummaryWhenThereIsNothingToCompact(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.turn(t, "hi", "hello")
	id := chat.compact(t, nil)
	chat.outcome(t, id)
	if got := outcomeJSON(t, *getTask(t, chat.h, id)); got != `{"status":"completed","result":{}}` {
		t.Fatalf("outcome = %s", got)
	}
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
}

func TestCompactionAsksBeforeCompactTheFirstDecisionWinsAThrowIsReportedAndSkipped(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	var seen []CompactionRequest
	addCompactionHooks(t, chat.setup.registry, "broken", &CompactionHooks{BeforeCompact: func(context.Context, CompactionRequest, *TaskRuntime) (*CompactionDecision, error) {
		return nil, errors.New("hook broke")
	}})
	addCompactionHooks(t, chat.setup.registry, "summary", &CompactionHooks{BeforeCompact: func(_ context.Context, request CompactionRequest, _ *TaskRuntime) (*CompactionDecision, error) {
		seen = append(seen, request)
		return &CompactionDecision{Summary: "FROM HOOK"}, nil
	}})
	addCompactionHooks(t, chat.setup.registry, "decline", decide(&CompactionDecision{Decline: true}))
	chat.history(t)
	chat.result(t, chat.compact(t, ptr("why")))
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "<summary>\nFROM HOOK\n</summary>") {
		t.Fatalf("first user = %q", got)
	}
	if reports := chat.setup.reports.all(); len(reports) != 1 || reports[0].Error() != "hook broke" {
		t.Fatalf("reports = %v", reports)
	}
	if len(seen) != 1 {
		t.Fatalf("seen = %d", len(seen))
	}
	compaction := seen[0]
	if compaction.Reason != CompactionManual || compaction.Instructions == nil || *compaction.Instructions != "why" {
		t.Fatalf("compaction = %+v", compaction)
	}
	var kinds []string
	for _, record := range compaction.Entries {
		kinds = append(kinds, record.Kind)
	}
	if want := []string{"pi.user", "pi.system", "pi.assistant", "pi.user", "pi.assistant"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("entries = %v", kinds)
	}
	var texts []string
	for _, message := range compaction.Messages {
		if message.MessageRole() != ai.RoleSystem {
			texts = append(texts, msgText(message))
		}
	}
	if want := []string{sizedText("u1", 100), sizedText("a1", 100), sizedText("u2", 100), sizedText("a2", 100)}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("messages = %q", texts)
	}
}

func TestCompactionCompletesWithoutASummaryWhenAHookDeclines(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	addCompactionHooks(t, chat.setup.registry, "decline", decide(&CompactionDecision{Decline: true}))
	chat.history(t)
	id := chat.compact(t, nil)
	chat.outcome(t, id)
	if got := outcomeJSON(t, *getTask(t, chat.h, id)); got != `{"status":"completed","result":{}}` {
		t.Fatalf("outcome = %s", got)
	}
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
}

func TestCompactionFailsWithNoModelWithoutAConfiguredModel(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	must(t, chat.root.Configure(bg, AgentChange{Model: Clear[ModelRef]()}))
	outcome := chat.outcome(t, chat.compact(t, nil))
	if outcome.Status != durable.OutcomeFailed || outcome.Error == nil || string(outcome.Error.Detail) != `{"reason":"no_model"}` {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
}

func TestCompactionRetriesARetryableErrorWithThePinnedRequestAndCountsEveryAttemptOnce(t *testing.T) {
	single := openCompaction(t, compactionOpen{})
	single.history(t)
	must(t, single.root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}))
	single.faux.pushSummary(summaryStep("SUMMARY"))
	once := single.compactionInput(t, func() { single.result(t, single.compact(t, nil)) })
	if once <= 0 {
		t.Fatalf("one attempt added %d input tokens", once)
	}

	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	must(t, chat.root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}))
	chat.setup.setSettings(func(s *HarnessSettings) {
		s.Stream = ConversationStreamOptions{TimeoutMs: ptr(1234), Deferred: &StreamDeferral{}}
	})
	chat.faux.pushSummary(func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		// Changed during the attempt: the retry still uses the pinned request.
		must(t, chat.root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("low")}))
		chat.setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(1)} })
		return fauxError("overloaded")
	}, summaryStep("SUMMARY"))
	twice := chat.compactionInput(t, func() { chat.result(t, chat.compact(t, nil)) })
	if n := chat.faux.summaryCount(); n != 2 {
		t.Fatalf("summary requests = %d", n)
	}
	if twice != 2*once {
		t.Fatalf("two attempts added %d input tokens, one %d", twice, once)
	}
	provider, err := chat.h.Snapshot(bg, ProviderDoc.Conversation(chat.root.ID))
	must(t, err)
	for i := range 2 {
		options := chat.faux.summaryRequest(t, i).options
		if options.Reasoning != "high" || options.TimeoutMs != 1234 || options.CacheRetention != "none" || options.SessionID != provider["sessionId"] || options.Deferred != nil {
			t.Fatalf("request %d options = reasoning %q, timeout %d, cache %q, session %q, deferred %v", i, options.Reasoning, options.TimeoutMs, options.CacheRetention, options.SessionID, options.Deferred)
		}
	}
}

func TestCompactionAddsNoUsageWhenAHookDeclinesOrSuppliesTheSummary(t *testing.T) {
	for _, decision := range []*CompactionDecision{{Decline: true}, {Summary: "HOOK"}} {
		chat := openCompaction(t, compactionOpen{})
		addCompactionHooks(t, chat.setup.registry, "decide", decide(decision))
		chat.history(t)
		if added := chat.compactionInput(t, func() { chat.result(t, chat.compact(t, nil)) }); added != 0 {
			t.Fatalf("decision %+v added %d input tokens", decision, added)
		}
	}
}

func TestCompactionCapsMaxTokensAtTheModelsOutputLimitAndSendsNoTools(t *testing.T) {
	setup := newCompactionSetup(t, 0)
	faux := scriptCompaction(setup)
	read := testTool("read")
	addTool(t, setup.registry, read)
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	must(t, chat.root.Configure(bg, AgentChange{Tools: Set(Exactly(read))}))
	chat.history(t)
	chat.setup.setSettings(func(s *HarnessSettings) { s.Compaction.ReserveTokens = ptr(2000) })
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	request := chat.faux.summaryRequest(t, 0)
	// 0.8 * 2000 = 1600, above the model's 900.
	if request.options.MaxTokens == nil || *request.options.MaxTokens != 900 {
		t.Fatalf("maxTokens = %v", request.options.MaxTokens)
	}
	if len(request.messages) != 2 {
		t.Fatalf("summary request has %d messages", len(request.messages))
	}
	for _, message := range request.messages {
		if system, ok := message.(ai.SystemMessage); ok && system.ToolsAdded != nil {
			t.Fatal("the summary request adds tools")
		}
	}
}

func TestCompactionFailsWithModelError(t *testing.T) {
	toolCall := providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("read", map[string]any{})}, ai.StopStop)
	for _, c := range []struct {
		name     string
		response *ai.AssistantMessage
		message  string
	}{
		{"retries run out", fauxError("overloaded"), "Summarization failed: overloaded"},
		{"a non-retryable error", fauxError("bad request"), "Summarization failed: bad request"},
		{"a length stop", providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("partial")}, ai.StopLength), "Summarization hit the token limit; the summary is incomplete"},
		{"a tool call", toolCall, "Summarization attempted to call a tool"},
		{"empty text", fauxAnswer("  "), "Summarization produced no text"},
	} {
		t.Run(c.name, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{})
			chat.history(t)
			for range 3 {
				chat.faux.pushSummary(providers.FauxStatic(c.response))
			}
			outcome := chat.outcome(t, chat.compact(t, nil))
			if outcome.Status != durable.OutcomeFailed || outcome.Error == nil || outcome.Error.Message != c.message || string(outcome.Error.Detail) != `{"reason":"model_error"}` {
				t.Fatalf("outcome = %+v", outcome)
			}
			// The retry policy allows two retries after the first attempt.
			want := 1
			if c.name == "retries run out" {
				want = 3
			}
			if n := chat.faux.summaryCount(); n != want {
				t.Fatalf("summary requests = %d, want %d", n, want)
			}
			if _, ok := chat.live(t)["compactions"]; ok {
				t.Fatal("compactions are still listed")
			}
			if hasKind(chat.kinds(t), "pi.compaction") {
				t.Fatal("a failed compaction wrote an entry")
			}
		})
	}
}

// Per-model compaction overrides (the manual case; the threshold and
// overflow cases come with generation's compaction).
func TestCompactionAppliesTheModelsOverrideWhenACompactionSelectsAndSizesItsSummary(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(func(s *HarnessSettings) {
		manualPolicy(100_000)(s)
		s.Compaction.ModelOverrides = map[string]CompactionOverride{"faux/faux-1": {KeepRecentTokens: ptr(150), ReserveTokens: ptr(500)}}
	})
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	// 0.8 * 500 from the override, below the model's 900.
	if max := chat.faux.summaryRequest(t, 0).options.MaxTokens; max == nil || *max != 400 {
		t.Fatalf("maxTokens = %v", max)
	}
	var kept []string
	for _, message := range chat.context(t).Messages[1:] {
		kept = append(kept, msgText(message))
	}
	if want := []string{sizedText("u3", 100), sizedText("a3", 100)}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %q", kept)
	}
}
