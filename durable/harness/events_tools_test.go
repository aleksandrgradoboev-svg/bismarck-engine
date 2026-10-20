package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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
