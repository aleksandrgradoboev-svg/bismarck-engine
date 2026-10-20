package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-inbox.test.ts: the cases
// that run tools.

// holdTool installs a hold tool whose calls wait for gate and then return
// result; a nil gate does not wait.
func holdTool(t *testing.T, setup *chatSetup, gate <-chan struct{}, result ToolExecutionResult) {
	t.Helper()
	installTool(t, setup.registry, namedWaitingTool("hold", gate, result))
}

func namedWaitingTool(name string, gate <-chan struct{}, result ToolExecutionResult) *ToolRegistration {
	if result.Output == nil {
		result.Output = ai.ContentList{}
	}
	return &ToolRegistration{
		Tool: ai.Tool{Name: name, Description: "Waits for the test", Parameters: objectSchema()},
		Execute: func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return ToolExecutionResult{}, context.Cause(ctx)
				}
			}
			return result, nil
		},
	}
}

func holdCall() providers.FauxResponseStep { return fauxStep(toolCalls(call("hold", nil, "c1"))) }

// toolRunning waits until the first slot runs.
func toolRunning(t *testing.T, h *Harness, root *Conversation) {
	t.Helper()
	eventually(t, func() bool {
		slots := liveSlots(t, h, root)
		return len(slots) > 0 && slots[0].Status == slotRunning
	})
}

func TestInboxAddsSteersToTheRunAtThePostToolsBoundaryAndHoldsFollowUpsForTheFinalBoundary(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, ToolExecutionResult{})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("after tools"), fauxText("follow-up")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	s := submit(t, root, steer("s"))
	f := submit(t, root, inputDraft("f"))
	close(gate)
	waitFor(t, f)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant", "pi.tool-result", "pi.user:s", "pi.assistant:after tools", "pi.user:f", "pi.assistant:follow-up"})
	answers := assistantIDs(t, root)
	expectDone(t, "input", submissionStatus(t, input), &answers[1])
	expectDone(t, "steer", submissionStatus(t, s), &answers[1])
	expectDone(t, "follow-up", submissionStatus(t, f), &answers[2])
}

func TestInboxEndsTheRunAtAQueuedResetAfterToolsAndRunsEarlierFollowUpsInTheNewContext(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, ToolExecutionResult{})
	var requests [][]string
	record := func(_ context.Context, request ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		var messages []string
		for _, message := range request.Messages {
			messages = append(messages, string(message.MessageRole())+":"+textOf(durable.EntryRecord{Model: []ai.Message{message}}))
		}
		requests = append(requests, messages)
		return fauxAnswer("fresh")
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), record})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	f := submit(t, root, inputDraft("f"))
	reset(t, root, nil)
	close(gate)
	waitFor(t, f)
	expectUnanswered(t, "input", submissionStatus(t, input), "reset")
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant", "pi.tool-result", "pi.reset", "pi.user:f", "pi.assistant:fresh"})
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == durable.ResetEntry.Kind && (entry.Head == nil || *entry.Head != entry.ID) {
			t.Fatalf("reset head = %v, want its own ID %d", entry.Head, entry.ID)
		}
	}
	// The follow-up's request starts at the reset: the complete system
	// baseline after the cut leads the follow-up.
	if len(requests) != 1 {
		t.Fatalf("requests = %q", requests)
	}
	expectStrings(t, "request", requests[0], []string{"system:", "user:f"})
	if calls := callCount(setup); calls != 2 {
		t.Fatalf("model calls = %d", calls)
	}
}

func TestInboxEndsTheRunWithAPiResetEntryWhenAToolRequestsAHandoff(t *testing.T) {
	setup := newChatSetup(t)
	handoff := "continue here"
	holdTool(t, setup, nil, ToolExecutionResult{Control: &ToolControl{Handoff: &handoff}})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall()})
	h, root := openChat(t, memory.New(), setup)
	settled := waitFor(t, submit(t, root, inputDraft("a")))
	entries := chatEntries(t, root)
	answers := assistantIDs(t, root)
	expectDone(t, "input", settled, &answers[0])
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant", "pi.tool-result", "pi.reset:continue here"})
	if last := entries[len(entries)-1]; last.Head == nil || *last.Head != last.ID {
		t.Fatalf("reset head = %v, want its own ID %d", last.Head, last.ID)
	}
	if calls := callCount(setup); calls != 1 {
		t.Fatalf("model calls = %d", calls)
	}
	expectIdleLive(t, h, root)
}

func TestInboxAddsEverySteerToTheRunAtThePostToolsBoundaryWithSteeringModeAll(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, ToolExecutionResult{})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("after tools"), fauxText("follow-up")})
	h, root := openChat(t, memory.New(), setup)
	setup.setSettings(func(s *HarnessSettings) { s.SteeringMode = QueueAll })
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	s1 := submit(t, root, steer("s1"))
	f := submit(t, root, inputDraft("f"))
	s2 := submit(t, root, steer("s2"))
	close(gate)
	waitFor(t, f)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant", "pi.tool-result", "pi.user:s1", "pi.user:s2", "pi.assistant:after tools", "pi.user:f", "pi.assistant:follow-up"})
	answers := assistantIDs(t, root)
	for name, submission := range map[string]*Submission{"input": input, "s1": s1, "s2": s2} {
		expectDone(t, name, submissionStatus(t, submission), &answers[1])
	}
}

func TestInboxStartsAQueuedFollowUpAfterATerminatingRound(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, ToolExecutionResult{Control: &ToolControl{Terminate: true}})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("follow-up")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	f := submit(t, root, inputDraft("f"))
	close(gate)
	waitFor(t, f)
	calling := assistantIDs(t, root)[0]
	expectDone(t, "input", submissionStatus(t, input), &calling)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant", "pi.tool-result", "pi.user:f", "pi.assistant:follow-up"})
}

func TestInboxWritesTheLastHandoffInCallOrderAndThenRunsQueuedFollowUpsInTheNewContext(t *testing.T) {
	setup := newChatSetup(t)
	firstGate, secondGate := make(chan struct{}), make(chan struct{})
	one, two := "one", "two"
	holdTool(t, setup, firstGate, ToolExecutionResult{Control: &ToolControl{Handoff: &one}})
	installTool(t, setup.registry, namedWaitingTool("later", secondGate, ToolExecutionResult{Control: &ToolControl{Handoff: &two}}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("hold", nil, "c1"), call("later", nil, "c2"))), fauxText("follow-up")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	f := submit(t, root, inputDraft("f"))
	close(secondGate)
	eventually(t, func() bool {
		slots := liveSlots(t, h, root)
		return len(slots) > 1 && slots[1].Status == slotDone
	})
	close(firstGate)
	waitFor(t, f)
	expectDone(t, "input", submissionStatus(t, input), nil)
	got := transcript(t, root)
	expectStrings(t, "transcript tail", got[len(got)-4:], []string{"pi.tool-result", "pi.reset:two", "pi.user:f", "pi.assistant:follow-up"})
}

func TestInboxTotalsAssistantUsagePerModelAndToolUsagePerToolAndSumsTheSession(t *testing.T) {
	setup := newChatSetup(t)
	spent := ai.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 10, Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1}}
	holdTool(t, setup, nil, ToolExecutionResult{Usage: &spent})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("done"), fauxText("other")})
	h, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("a")))
	entries := chatEntries(t, root)
	var input, output, tokens int
	for _, entry := range entries {
		if entry.Kind == durable.AssistantEntry.Kind {
			message := entryAssistant(t, entry)
			input += message.Usage.Input
			output += message.Usage.Output
			tokens += message.Usage.TotalTokens
		}
	}
	var usage UsageState
	value, err := h.Snapshot(bg, UsageDoc.Conversation(root.ID))
	must(t, err)
	must(t, fromValue(value, &usage))
	if model := usage.Models["faux/faux-1"]; model.Input != input || model.Output != output || model.TotalTokens != tokens {
		t.Fatalf("model usage = %+v, want %d/%d/%d", model, input, output, tokens)
	}
	if jsonOf(t, usage.Tools) != jsonOf(t, map[string]ai.Usage{"hold": spent}) {
		t.Fatalf("tool usage = %s", jsonOf(t, usage.Tools))
	}
	results := toolResults(entries)
	if len(results) != 1 || results[0].Usage == nil || jsonOf(t, *results[0].Usage) != jsonOf(t, spent) {
		t.Fatalf("result usage = %+v", results)
	}

	// A fork starts at zero; the Session total adds every conversation
	// once.
	fork, err := root.Fork(bg, entries[len(entries)-1].ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	forkValue, err := h.Snapshot(bg, UsageDoc.Conversation(fork.ID))
	must(t, err)
	if got := jsonOf(t, forkValue); got != `{"models":{},"tools":{}}` {
		t.Fatalf("fork usage = %s", got)
	}
	waitFor(t, submit(t, fork, inputDraft("b")))
	forkValue, err = h.Snapshot(bg, UsageDoc.Conversation(fork.ID))
	must(t, err)
	var forkUsage UsageState
	must(t, fromValue(forkValue, &forkUsage))
	total, err := h.Usage(bg)
	must(t, err)
	if got, want := total.Models["faux/faux-1"].Output, output+forkUsage.Models["faux/faux-1"].Output; got != want {
		t.Fatalf("session output = %d, want %d", got, want)
	}
	if jsonOf(t, total.Tools) != jsonOf(t, map[string]ai.Usage{"hold": spent}) {
		t.Fatalf("session tool usage = %s", jsonOf(t, total.Tools))
	}
}

func TestInboxCountsFailedAttemptsConvertedPartialsAndToolUsageReplacedByAfterTool(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 200, MinTokenSize: 1, MaxTokenSize: 1})
	spent := ai.Usage{Input: 5, TotalTokens: 5}
	holdTool(t, setup, nil, ToolExecutionResult{})
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{AfterTool: func(_ context.Context, _ ToolHookCall, result ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
		result.Usage = &spent
		return &result, nil
	}})
	retryable := providers.FauxAssistantMessage(ai.ContentList{}, ai.StopError)
	retryable.ErrorMessage = "503 Service Unavailable"
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(retryable), holdCall(), fauxText("done"), fauxText(strings.Repeat("x", 400))})
	h, root := openChat(t, memory.New(), setup)
	setup.setSettings(retrySettings(true, 1, 1))
	waitFor(t, submit(t, root, inputDraft("a")))
	var usage UsageState
	value, err := h.Snapshot(bg, UsageDoc.Conversation(root.ID))
	must(t, err)
	must(t, fromValue(value, &usage))
	if jsonOf(t, usage.Tools) != jsonOf(t, map[string]ai.Usage{"hold": spent}) {
		t.Fatalf("tool usage = %s", jsonOf(t, usage.Tools))
	}

	// A partial committed while streaming becomes an aborted entry on abort;
	// its usage counts too.
	input := submit(t, root, inputDraft("b"))
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		return generation != nil && generation.Message != nil
	})
	_, err = h.AbortTask(bg, decodeLive(t, liveOf(t, h, root)).Run.TaskID)
	must(t, err)
	waitFor(t, input)
	var reasons []ai.StopReason
	output := 0
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == durable.AssistantEntry.Kind {
			message := entryAssistant(t, entry)
			reasons = append(reasons, message.StopReason)
			output += message.Usage.Output
		}
	}
	if want := []ai.StopReason{ai.StopError, ai.StopToolUse, ai.StopStop, ai.StopAborted}; jsonOf(t, reasons) != jsonOf(t, want) {
		t.Fatalf("stop reasons = %v, want %v", reasons, want)
	}
	value, err = h.Snapshot(bg, UsageDoc.Conversation(root.ID))
	must(t, err)
	must(t, fromValue(value, &usage))
	if got := usage.Models["faux/faux-1"].Output; got != output {
		t.Fatalf("model output = %d, want %d", got, output)
	}
}
