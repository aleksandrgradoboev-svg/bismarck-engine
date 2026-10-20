package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Compaction test support (pi harness-compaction.test.ts helpers).

// compactionRequest is one request the scripted faux model received.
type compactionRequest struct {
	messages []ai.Message
	options  *ai.SimpleStreamOptions
	model    string
}

// compactionScript is a scripted faux model that answers agent requests and
// summarization requests from separate queues.
type compactionScript struct {
	mu              sync.Mutex
	agent           []providers.FauxResponseStep
	summaries       []providers.FauxResponseStep
	agentRequests   []compactionRequest
	summaryRequests []compactionRequest
}

func (s *compactionScript) pushAgent(steps ...providers.FauxResponseStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agent = append(s.agent, steps...)
}

func (s *compactionScript) pushSummary(steps ...providers.FauxResponseStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summaries = append(s.summaries, steps...)
}

func (s *compactionScript) summaryRequest(t *testing.T, index int) compactionRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.summaryRequests) {
		t.Fatalf("summary requests = %d, want more than %d", len(s.summaryRequests), index)
	}
	return s.summaryRequests[index]
}

func (s *compactionScript) summaryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.summaryRequests)
}

func (s *compactionScript) lastAgentRequest(t *testing.T) compactionRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agentRequests) == 0 {
		t.Fatal("no agent request")
	}
	return s.agentRequests[len(s.agentRequests)-1]
}

// isSummaryRequest tells a summarization request by its system prompt.
func isSummaryRequest(messages []ai.Message) bool {
	if len(messages) == 0 {
		return false
	}
	system, ok := messages[0].(ai.SystemMessage)
	if !ok {
		return false
	}
	text, ok := system.StringContent()
	return ok && strings.HasPrefix(text, "You are a context summarization assistant")
}

func scriptCompaction(setup *chatSetup) *compactionScript {
	script := &compactionScript{}
	steps := make([]providers.FauxResponseStep, 500)
	for i := range steps {
		steps[i] = func(ctx context.Context, transcript ai.TranscriptContext, options *ai.SimpleStreamOptions, state *providers.FauxState, model *ai.Model) *ai.AssistantMessage {
			request := compactionRequest{messages: append([]ai.Message(nil), transcript.Messages...), options: options, model: model.ID}
			summary := isSummaryRequest(request.messages)
			script.mu.Lock()
			var queue *[]providers.FauxResponseStep
			if summary {
				script.summaryRequests = append(script.summaryRequests, request)
				queue = &script.summaries
			} else {
				script.agentRequests = append(script.agentRequests, request)
				queue = &script.agent
			}
			if len(*queue) == 0 {
				script.mu.Unlock()
				kind := "agent"
				if summary {
					kind = "summary"
				}
				return fauxError("No scripted " + kind + " response")
			}
			step := (*queue)[0]
			*queue = (*queue)[1:]
			script.mu.Unlock()
			return step(ctx, transcript, options, state, model)
		}
	}
	setup.faux.SetResponses(steps)
	return script
}

// manualPolicy is small thresholds: no automatic compaction unless a test
// enables it.
func manualPolicy(keepRecent int) func(*HarnessSettings) {
	return func(s *HarnessSettings) {
		s.Compaction.Enabled = ptr(false)
		s.Compaction.ReserveTokens = ptr(1000)
		s.Compaction.KeepRecentTokens = ptr(keepRecent)
		s.Compaction.BackgroundTokens = ptr(0)
	}
}

type compactionChat struct {
	h       *Harness
	root    *Conversation
	setup   *chatSetup
	faux    *compactionScript
	storage durable.Storage
}

type compactionOpen struct {
	contextWindow int
	storage       durable.Storage
	policy        func(*HarnessSettings)
}

func newCompactionSetup(t *testing.T, contextWindow int) *chatSetup {
	t.Helper()
	if contextWindow == 0 {
		contextWindow = 100_000
	}
	return newChatSetupWith(t, providers.RegisterFauxProviderOptions{Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: contextWindow, MaxTokens: 900}}})
}

func openCompaction(t *testing.T, options compactionOpen) *compactionChat {
	t.Helper()
	setup := newCompactionSetup(t, options.contextWindow)
	faux := scriptCompaction(setup)
	return openCompactionWith(t, options, setup, faux)
}

func openCompactionWith(t *testing.T, options compactionOpen, setup *chatSetup, faux *compactionScript) *compactionChat {
	t.Helper()
	addSection(t, setup.registry, &PromptSection{Key: "preamble", Untagged: true, Render: constSection("preamble", "You are helpful.").Render})
	storage := options.storage
	if storage == nil {
		storage = memory.New()
	}
	h, root := openChat(t, storage, setup)
	setup.setSettings(func(s *HarnessSettings) {
		// These tests exercise compaction accounting and thresholds, not the
		// faux provider's cache simulation.
		s.Stream = ConversationStreamOptions{CacheRetention: "none"}
		policy := options.policy
		if policy == nil {
			policy = manualPolicy(150)
		}
		policy(s)
		s.Retry.Enabled = ptr(true)
		s.Retry.MaxRetries = ptr(2)
		s.Retry.BaseDelay = ptr(time.Millisecond)
	})
	must(t, h.Resume())
	return &compactionChat{h: h, root: root, setup: setup, faux: faux, storage: storage}
}

// turn runs one turn: user answered by reply.
func (c *compactionChat) turn(t *testing.T, user, reply string) {
	t.Helper()
	c.faux.pushAgent(fauxText(reply))
	submission, err := c.root.Submit(bg, inputDraft(user))
	must(t, err)
	settled, err := submission.Wait(bg)
	must(t, err)
	if settled.Status != durable.SubmissionDone {
		t.Fatalf("turn %q settled %+v", user, settled)
	}
}

// history is three turns of about 100-token messages.
func (c *compactionChat) history(t *testing.T) {
	t.Helper()
	c.turn(t, sizedText("u1", 100), sizedText("a1", 100))
	c.turn(t, sizedText("u2", 100), sizedText("a2", 100))
	c.turn(t, sizedText("u3", 100), sizedText("a3", 100))
}

func (c *compactionChat) compact(t *testing.T, instructions *string) durable.TaskID {
	t.Helper()
	id, err := c.root.Compact(bg, instructions)
	must(t, err)
	return id
}

// outcome waits for a task's outcome.
func (c *compactionChat) outcome(t *testing.T, id durable.TaskID) durable.TaskOutcome {
	t.Helper()
	record, err := c.h.WaitForTask(bg, id)
	must(t, err)
	if record.State.Outcome == nil {
		t.Fatalf("task %d has no outcome", id)
	}
	return *record.State.Outcome
}

// result waits for a compaction's result; it must complete.
func (c *compactionChat) result(t *testing.T, id durable.TaskID) CompactionResult {
	t.Helper()
	outcome := c.outcome(t, id)
	if outcome.Status != durable.OutcomeCompleted {
		t.Fatalf("compaction %d = %+v, want completed", id, outcome)
	}
	var result CompactionResult
	must(t, json.Unmarshal(outcome.Result, &result))
	return result
}

// summarySubmission is the write submission of a conversation-owned
// compaction's summary.
func (c *compactionChat) summarySubmission(t *testing.T, id durable.TaskID) *Submission {
	t.Helper()
	result := c.result(t, id)
	if result.SubmissionID == nil {
		t.Fatalf("compaction %d result %+v has no submission", id, result)
	}
	submission, err := c.h.Submission(bg, *result.SubmissionID)
	must(t, err)
	return submission
}

func submissionWait(t *testing.T, submission *Submission) durable.SubmissionRecord {
	t.Helper()
	record, err := submission.Wait(bg)
	must(t, err)
	return record
}

func (c *compactionChat) kinds(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, entry := range chatEntries(t, c.root) {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}

func (c *compactionChat) live(t *testing.T) map[string]any {
	t.Helper()
	value, err := c.h.Snapshot(bg, LiveDoc.Conversation(c.root.ID))
	must(t, err)
	if value == nil {
		return map[string]any{}
	}
	return value
}

func (c *compactionChat) modelUsage(t *testing.T) ai.Usage {
	t.Helper()
	value, err := c.h.Snapshot(bg, UsageDoc.Conversation(c.root.ID))
	must(t, err)
	var usage UsageState
	must(t, fromValue(value, &usage))
	return usage.Models["faux/faux-1"]
}

func (c *compactionChat) context(t *testing.T) ContextView {
	t.Helper()
	view, err := c.root.Context(bg, nil)
	must(t, err)
	return view
}

// messageText is pi's textOf: a message's string content or first text block.
func msgText(message ai.Message) string {
	var content ai.ContentList
	switch m := message.(type) {
	case ai.UserMessage:
		content = m.Content
	case ai.AssistantMessage:
		content = m.Content
	case ai.ToolResultMessage:
		content = m.Content
	}
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

// firstUserText is the text of the first user message; a leading baseline
// system message comes before it.
func firstUserText(messages []ai.Message) string {
	for _, message := range messages {
		if message.MessageRole() == ai.RoleUser {
			return msgText(message)
		}
	}
	return ""
}

func summaryStep(text string) providers.FauxResponseStep { return fauxText(text) }

func hasKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
