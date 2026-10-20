package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-events.test.ts: the cases
// that run a tool.

// toolEvents are the tool_execution_* events among events.
func toolEvents(events []AgentEvent) []AgentEvent {
	return slices.DeleteFunc(slices.Clone(events), func(event AgentEvent) bool {
		return !strings.HasPrefix(event.EventType(), "tool_execution")
	})
}

// rebuiltOutput is a slot's retained window as its output events rebuild it.
func rebuiltOutput(events []AgentEvent) string {
	text := ""
	for _, event := range events {
		update, ok := event.(ToolExecutionUpdateEvent)
		if !ok || update.Output == nil {
			continue
		}
		if update.Output.Set != nil {
			text = *update.Output.Set
		} else {
			text = text[update.Output.TrimStart:] + update.Output.Append
		}
	}
	return text
}

func TestEventsReportToolStartOutputAppendsAndTheResultEntry(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	var schema ai.Schema
	must(t, json.Unmarshal([]byte(`{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`), &schema))
	installTool(t, setup.registry, &ToolRegistration{
		Tool: ai.Tool{Name: "print", Description: "Prints", Parameters: &schema},
		Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			if err := api.Output([]byte("one\n"), nil); err != nil {
				return ToolExecutionResult{}, err
			}
			time.Sleep(150 * time.Millisecond)
			if err := api.Output([]byte("two\n"), nil); err != nil {
				return ToolExecutionResult{}, err
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return ToolExecutionResult{}, context.Cause(ctx)
			}
			return ToolExecutionResult{}, nil
		},
	})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("print", map[string]any{"n": 1}, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	_, log := listen(t, root)
	submission := submit(t, root, inputDraft("go"))
	// The output events rebuild the slot's retained window.
	eventually(t, func() bool { return rebuiltOutput(log.events()) == "one\ntwo\n" })
	close(gate)
	waitFor(t, submission)
	log.quiet(t)
	all := log.events()
	tool := toolEvents(all)
	start, ok := tool[0].(ToolExecutionStartEvent)
	if !ok || start.ToolCallID != "c1" || start.ToolName != "print" || start.TaskID == nil || jsonOf(t, start.Args) != `{"n":1}` {
		t.Fatalf("first tool event = %s", jsonOf(t, tool[0]))
	}
	end, ok := tool[len(tool)-1].(ToolExecutionEndEvent)
	if !ok || end.ToolCallID != "c1" || end.Entry == nil || end.Entry.Kind != durable.ToolResultEntry.Kind {
		t.Fatalf("last tool event = %s", jsonOf(t, tool[len(tool)-1]))
	}
	// As in the coding agent, the tool ends directly before its result
	// message.
	index := slices.IndexFunc(all, func(event AgentEvent) bool {
		e, ok := event.(ToolExecutionEndEvent)
		return ok && e.ToolCallID == "c1"
	})
	if got := eventTypes(all[index : index+3]); !slices.Equal(got, []string{"tool_execution_end", "message_start", "message_end"}) {
		t.Fatalf("after the end = %v", got)
	}
	if result, ok := all[index+1].(MessageStartEvent).Message.(ai.ToolResultMessage); !ok || result.ToolCallID != "c1" {
		t.Fatalf("result message = %s", jsonOf(t, all[index+1]))
	}
	// Two turns: the tool round, and the answer.
	types := eventTypes(all)
	if starts, ends := countOf(types, "turn_start"), countOf(types, "turn_end"); starts != 2 || ends != 2 {
		t.Fatalf("turn starts = %d, ends = %d", starts, ends)
	}
}

func countOf(types []string, kind string) int {
	n := 0
	for _, t := range types {
		if t == kind {
			n++
		}
	}
	return n
}

func TestEventsEndACallThatNeverRunsAndAToolAbortedWithItsGeneration(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, &ToolRegistration{
		Tool: ai.Tool{Name: "wait", Description: "Waits until aborted", Parameters: objectSchema()},
		Execute: func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
			<-ctx.Done()
			return ToolExecutionResult{}, context.Cause(ctx)
		},
	})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("ghost", nil, "c1"), call("wait", nil, "c2")))})
	h, root := openChat(t, memory.New(), setup)
	_, log := listen(t, root)
	submission := submit(t, root, inputDraft("go"))
	eventually(t, func() bool { return slices.Contains(eventTypes(log.events()), "tool_execution_start") })
	// Aborting the generation aborts its round: the tool ends with its
	// aborted result first (spec §8.5).
	_, err := h.AbortTask(bg, decodeLive(t, liveOf(t, h, root)).Run.TaskID)
	must(t, err)
	waitFor(t, submission)
	log.quiet(t)
	all := log.events()
	// The call not offered ends after its calling message and directly
	// before its result message.
	var round []string
	for _, event := range all {
		switch e := event.(type) {
		case MessageEndEvent:
			round = append(round, "end:"+e.Entry.Kind)
		case MessageStartEvent:
			round = append(round, "start:"+string(e.Message.MessageRole()))
		default:
			round = append(round, event.EventType())
		}
	}
	ghostEnd := slices.Index(round, "tool_execution_end")
	if ghostEnd < 1 || ghostEnd+3 > len(round) {
		t.Fatalf("round = %v", round)
	}
	if got := round[ghostEnd-1 : ghostEnd+3]; !slices.Equal(got, []string{"end:pi.assistant", "tool_execution_end", "start:toolResult", "end:pi.tool-result"}) {
		t.Fatalf("around the first end = %v", got)
	}
	var got []string
	for _, event := range toolEvents(all) {
		switch e := event.(type) {
		case ToolExecutionStartEvent:
			got = append(got, e.EventType()+" "+e.ToolCallID+" false")
		case ToolExecutionEndEvent:
			got = append(got, e.EventType()+" "+e.ToolCallID+" "+map[bool]string{true: "true", false: "false"}[e.Entry != nil])
		default:
			got = append(got, event.EventType())
		}
	}
	if want := []string{"tool_execution_end c1 true", "tool_execution_start c2 false", "tool_execution_end c2 true"}; !slices.Equal(got, want) {
		t.Fatalf("tool events = %v", got)
	}
}

// committedPartials collects the in-flight message of every pi.live commit
// that has one.
func committedPartials(t *testing.T, h *Harness) func() []ai.AssistantMessage {
	t.Helper()
	var mu sync.Mutex
	var partials []ai.AssistantMessage
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.Value != nil {
				if generation := decodeLive(t, document.Value).Generation; generation != nil && generation.Message != nil {
					mu.Lock()
					partials = append(partials, *generation.Message)
					mu.Unlock()
				}
			}
		}
	})
	must(t, err)
	t.Cleanup(unsubscribe)
	return func() []ai.AssistantMessage {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(partials)
	}
}

func TestEventsRebuildEveryCommittedPartialOfThinkingTextAndToolCallArgumentsFromMessageChanges(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 150, MinTokenSize: 1, MaxTokenSize: 1})
	message := providers.FauxAssistantMessage(ai.ContentList{
		providers.FauxThinking(strings.Repeat("thinking about it ", 10)),
		providers.FauxText(strings.Repeat("some text ", 10)),
		providers.FauxToolCall("missing", map[string]any{"path": strings.Repeat("a/long/path/", 10), "note": strings.Repeat("x", 60)}, "c1"),
	}, ai.StopToolUse)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(message), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	partials := committedPartials(t, h)
	_, log := listen(t, root)
	waitFor(t, submit(t, root, inputDraft("go")))
	log.quiet(t)
	var rebuilt []ai.AssistantMessage
	var current ai.AssistantMessage
	// The streamed tool-calling message, up to its end; the short final
	// answer commits no partial.
	for _, event := range log.events() {
		if end, ok := event.(MessageEndEvent); ok && end.Entry.Kind == durable.AssistantEntry.Kind {
			break
		}
		switch e := event.(type) {
		case MessageStartEvent:
			assistant, ok := e.Message.(ai.AssistantMessage)
			if !ok {
				continue
			}
			current = assistant
		case MessageUpdateEvent:
			current = applyChanges(current, e.Changes)
		default:
			continue
		}
		rebuilt = append(rebuilt, current)
	}
	committed := partials()
	if len(committed) <= 2 {
		t.Fatalf("committed partials = %d, want more than 2", len(committed))
	}
	contents := func(messages []ai.AssistantMessage) []string {
		return names(messages, func(m ai.AssistantMessage) string { return jsonOf(t, m.Content) })
	}
	if got, want := contents(rebuilt), contents(committed); !slices.Equal(got, want) {
		t.Fatalf("rebuilt %d partials, committed %d; first difference at %d", len(got), len(want), firstDifference(got, want))
	}
	kinds := map[string]bool{}
	for _, event := range log.events() {
		if update, ok := event.(MessageUpdateEvent); ok {
			for _, change := range update.Changes {
				kinds[change.ChangeType()] = true
			}
		}
	}
	if !kinds["thinking_delta"] && !kinds["text_delta"] {
		t.Fatalf("change kinds = %v, want a text or thinking delta", kinds)
	}
}

func firstDifference(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func TestEventsRebuildASlidingTailWindowFromOutputTrimsAndAppends(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	installTool(t, setup.registry, &ToolRegistration{
		Tool:         ai.Tool{Name: "tail", Description: "Prints lines", Parameters: objectSchema()},
		OutputLimits: &OutputLimits{MaxLines: 3, Retain: "tail"},
		Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			for line := range 6 {
				if err := api.Output([]byte(fmt.Sprintf("line %d\n", line)), nil); err != nil {
					return ToolExecutionResult{}, err
				}
				time.Sleep(120 * time.Millisecond)
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return ToolExecutionResult{}, context.Cause(ctx)
			}
			return ToolExecutionResult{}, nil
		},
	})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("tail", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	var mu sync.Mutex
	var outputs []string
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.Value != nil {
				tools, _ := document.Value["tools"].([]any)
				if len(tools) == 0 {
					continue
				}
				output, ok := tools[0].(map[string]any)["output"].(string)
				mu.Lock()
				if ok && (len(outputs) == 0 || outputs[len(outputs)-1] != output) {
					outputs = append(outputs, output)
				}
				mu.Unlock()
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	_, log := listen(t, root)
	submission := submit(t, root, inputDraft("go"))
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(outputs) > 0 && strings.HasSuffix(outputs[len(outputs)-1], "line 5\n")
	})
	close(gate)
	waitFor(t, submission)
	log.quiet(t)
	var rebuilt []string
	text, trimmed := "", false
	for _, event := range log.events() {
		update, ok := event.(ToolExecutionUpdateEvent)
		if !ok || update.Output == nil {
			continue
		}
		if update.Output.Set != nil {
			text = *update.Output.Set
		} else {
			text = text[update.Output.TrimStart:] + update.Output.Append
			trimmed = trimmed || update.Output.TrimStart > 0
		}
		rebuilt = append(rebuilt, text)
	}
	mu.Lock()
	defer mu.Unlock()
	expectStrings(t, "rebuilt windows", rebuilt, outputs)
	if !trimmed {
		t.Fatal("no update trimmed the window")
	}
}

func TestEventsReportASteerWithoutRunEventsAResetAsAnAppendedEntryAndNothingForOtherConversations(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, ToolExecutionResult{})
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	other, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	_, log := listen(t, root)
	input := submit(t, root, inputDraft("a"))
	eventually(t, func() bool { return slices.Contains(eventTypes(log.events()), "tool_execution_start") })
	submit(t, root, steer("s"))
	log.quiet(t)
	before := len(log.all())
	commitNote(t, other)
	log.quiet(t)
	if after := len(log.all()); after != before {
		t.Fatalf("batches = %d after another conversation's commit, want %d", after, before)
	}
	close(gate)
	waitFor(t, input)
	reset(t, root, nil)
	log.quiet(t)
	types := eventTypes(log.events())
	if starts, ends := countOf(types, "run_start"), countOf(types, "run_end"); starts != 1 || ends != 1 {
		t.Fatalf("run starts = %d, ends = %d", starts, ends)
	}
	batches := log.all()
	expectStrings(t, "last batch", eventTypes(batches[len(batches)-1]), []string{"entry_appended", "submission"})
}
