package coding

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The runner and recorder tests are ported from pi v1.2.0
// packages/coding-agent/test/nested-tool-calls.test.ts; the session tests
// cover AgentSession's wiring and the Go-only serialization of hooks.

func nestedTestUsage(input int, cost float64) *ai.Usage {
	return &ai.Usage{Input: input, TotalTokens: input, Cost: ai.CostBreakdown{Input: cost, Total: cost}}
}

func objectSchema() *ai.Schema {
	var s ai.Schema
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{}}`), &s)
	return &s
}

func testTool(name string, execute func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error)) agent.AgentTool {
	return agent.AgentTool{Name: name, Label: name, Description: name, Parameters: objectSchema(), Execute: execute}
}

// runnerOver is pi's createRunner: a host that runs tools directly.
func runnerOver(tools *[]agent.AgentTool, sequential bool) (*nestedCallRunner, *[]agent.AgentEvent) {
	var mu sync.Mutex
	var events []agent.AgentEvent
	return newNestedCallRunner(nestedHost{
		tools:      func() []agent.AgentTool { return *tools },
		sequential: func() bool { return sequential },
		run: func(ctx context.Context, call ai.ToolCall, _ string, onUpdate func(agent.AgentToolResult)) agent.AgentToolCallOutcome {
			for _, tool := range *tools {
				if tool.Name == call.Name {
					result, err := tool.Execute(ctx, call.ID, call.Arguments, func(p agent.AgentToolResult) { onUpdate(p) })
					if err != nil {
						return agent.AgentToolCallOutcome{ToolCall: call, Result: agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: err.Error()}}}, IsError: true}
					}
					return agent.AgentToolCallOutcome{ToolCall: call, Result: result, IsError: result.IsError}
				}
			}
			return agent.AgentToolCallOutcome{ToolCall: call, Result: agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "Tool " + call.Name + " not found"}}, Details: map[string]any{}}, IsError: true}
		},
		emit: func(_ context.Context, e agent.AgentEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}), &events
}

func recordJSON(t *testing.T, calls *ai.NestedToolCalls) string {
	t.Helper()
	for i := range calls.Calls {
		if calls.Calls[i].DurationMs == nil && calls.Calls[i].Status != ai.NestedToolCallUnfinished {
			t.Fatalf("call %d has no duration", i)
		}
		calls.Calls[i].DurationMs = nil
	}
	data, _ := json.Marshal(calls)
	return string(data)
}

func TestNestedRunnerIDsEventsAndRecord(t *testing.T) {
	echo := testTool("echo", func(_ context.Context, _ string, _ map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		onUpdate(agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "partial"}}})
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}}, nil
	})
	tools := []agent.AgentTool{echo}
	runner, events := runnerOver(&tools, false)
	var updates int
	first := runner.execute(context.Background(), "call", "echo", map[string]any{"a": 1}, func(agent.AgentToolResult) { updates++ })
	missing := runner.execute(context.Background(), "call", "missing", map[string]any{}, nil)
	if first.ToolCall.ID != "call/1" || missing.ToolCall.ID != "call/2" || !missing.IsError || updates != 1 {
		t.Fatalf("first %s missing %s %v updates %d", first.ToolCall.ID, missing.ToolCall.ID, missing.IsError, updates)
	}
	var got []string
	for _, e := range *events {
		got = append(got, string(e.Type)+" "+e.ToolCallID+" "+e.ParentToolCallID)
	}
	want := []string{
		"tool_execution_start call/1 call", "tool_execution_update call/1 call", "tool_execution_end call/1 call",
		"tool_execution_start call/2 call", "tool_execution_end call/2 call",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %v", got)
	}
	record := runner.takeRecord("call")
	if got := recordJSON(t, record.Calls); got != `{"calls":[{"id":"call/1","name":"echo","status":"ok","arguments":{"a":1}},{"id":"call/2","name":"missing","status":"error","arguments":{},"error":"Tool missing not found"}],"complete":true}` {
		t.Fatalf("record %s", got)
	}
	// The record is taken once.
	if runner.takeRecord("call") != nil || runner.takeRecord("other") != nil {
		t.Fatal("record taken twice")
	}
}

func TestNestedRunnerRecordsDeeperCallsOnTheModelCall(t *testing.T) {
	leaf := testTool("leaf", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	})
	tools := []agent.AgentTool{leaf}
	runner, _ := runnerOver(&tools, false)
	tools = append(tools, testTool("middle", func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		runner.execute(ctx, id, "leaf", nil, nil)
		return agent.AgentToolResult{}, nil
	}))
	runner.execute(context.Background(), "call", "middle", nil, nil)
	var ids []string
	for _, c := range runner.takeRecord("call").Calls.Calls {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "call/1,call/1/1" {
		t.Fatalf("ids %v", ids)
	}
}

func TestNestedRunnerSumsUsageAtEveryDepth(t *testing.T) {
	leaf := testTool("leaf", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{Usage: nestedTestUsage(10, 0.01)}, nil
	})
	plain := testTool("plain", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	})
	tools := []agent.AgentTool{leaf, plain}
	runner, _ := runnerOver(&tools, false)
	tools = append(tools, testTool("middle", func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		runner.execute(ctx, id, "leaf", nil, nil)
		// Its own usage only: the leaf's is counted once, by the recorder.
		return agent.AgentToolResult{Usage: nestedTestUsage(5, 0.005)}, nil
	}))
	ctx := context.Background()
	runner.execute(ctx, "call", "middle", nil, nil)
	runner.execute(ctx, "call", "leaf", nil, nil)
	runner.execute(ctx, "call", "plain", nil, nil)
	runner.execute(ctx, "free", "plain", nil, nil)
	summary := runner.takeRecord("call")
	if summary.Usage.Input != 25 || math.Abs(summary.Usage.Cost.Total-0.025) > 1e-10 {
		t.Fatalf("usage %+v", summary.Usage)
	}
	if free := runner.takeRecord("free"); !free.Calls.Complete || free.Usage != nil {
		t.Fatalf("free %+v", free)
	}
}

func TestNestedRunnerSerializesSequentialTools(t *testing.T) {
	var active, maxSequential, maxParallel atomic.Int32
	makeTool := func(name string, max *atomic.Int32, mode agent.ToolExecutionMode) agent.AgentTool {
		tool := testTool(name, func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			n := active.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			return agent.AgentToolResult{}, nil
		})
		tool.ExecutionMode = mode
		return tool
	}
	tools := []agent.AgentTool{makeTool("sequential", &maxSequential, agent.ToolSequential), makeTool("parallel", &maxParallel, "")}
	runner, _ := runnerOver(&tools, false)
	for _, name := range []string{"sequential", "parallel"} {
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() { defer wg.Done(); runner.execute(context.Background(), "call", name, nil, nil) }()
		}
		wg.Wait()
	}
	if maxSequential.Load() != 1 || maxParallel.Load() != 3 {
		t.Fatalf("sequential %d parallel %d", maxSequential.Load(), maxParallel.Load())
	}
}

func recorderCall(id string, args map[string]any) ai.ToolCall {
	return ai.ToolCall{ID: id, Name: "t", Arguments: args}
}

func TestNestedRecorderLimits(t *testing.T) {
	r := newNestedCallRecorder()
	if r.snapshot() != nil {
		t.Fatal("empty snapshot")
	}
	r.finish(r.start(recorderCall("a", map[string]any{"x": 1})), false, "")
	if got := recordJSON(t, r.snapshot()); got != `{"calls":[{"id":"a","name":"t","status":"ok","arguments":{"x":1}}],"complete":true}` {
		t.Fatalf("snapshot %s", got)
	}
	big := r.start(recorderCall("b", map[string]any{"text": strings.Repeat("x", NestedMaxArgumentBytesPerCall)}))
	r.finish(big, true, strings.Repeat("e", 1000))
	s := r.snapshot()
	b := s.Calls[1]
	if s.Complete || b.ID != "b" || b.Status != ai.NestedToolCallError || b.Arguments != nil || b.ArgumentsBytes <= NestedMaxArgumentBytesPerCall || len(b.Error) != NestedMaxErrorChars {
		t.Fatalf("big %+v complete %v", b, s.Complete)
	}
	for i := range NestedMaxCalls {
		r.finish(r.start(recorderCall("c"+strconv.Itoa(i), map[string]any{})), false, "")
	}
	if len(r.snapshot().Calls) != NestedMaxCalls {
		t.Fatalf("calls %d", len(r.snapshot().Calls))
	}
}

func TestNestedRecorderTotalSizeAndUnfinished(t *testing.T) {
	r := newNestedCallRecorder()
	chunk := map[string]any{"text": strings.Repeat("x", 7000)}
	for i := range 6 {
		r.start(recorderCall("c"+strconv.Itoa(i), chunk))
	}
	s := r.snapshot()
	with := 0
	for _, c := range s.Calls {
		if c.Arguments != nil {
			with++
		}
		if c.Status != ai.NestedToolCallUnfinished {
			t.Fatalf("status %s", c.Status)
		}
	}
	// 32 KiB fits four 7000-byte argument objects.
	if with != 4 || s.Complete || len(s.Calls) != 6 {
		t.Fatalf("with %d complete %v calls %d", with, s.Complete, len(s.Calls))
	}
}

func chatModel() *ai.Model {
	return &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: 100000, MaxTokens: 8192}
}

// toolCallStream answers with one assistant message calling the named tools,
// then "done".
func toolCallStream(calls ...ai.ToolCall) agent.StreamFn {
	var mu sync.Mutex
	turn := 0
	return func(_ context.Context, model *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		mu.Lock()
		turn++
		first := turn == 1
		mu.Unlock()
		msg := &ai.AssistantMessage{Api: model.Api, Provider: model.Provider, Model: model.ID, StopReason: ai.StopStop,
			Content: ai.ContentList{ai.TextContent{Text: "done"}}}
		if first {
			msg.StopReason = ai.StopToolUse
			msg.Content = nil
			for _, c := range calls {
				msg.Content = append(msg.Content, c)
			}
		}
		s := ai.NewAssistantMessageEventStream()
		s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: msg.StopReason, Message: msg})
		s.End()
		return s
	}
}

// TestSessionExecuteToolRecordsNestedCalls: a tool's nested calls run with
// the session's hooks, which see the parent, emit events with the parent, and
// land on the tool's result message with their usage.
func TestSessionExecuteToolRecordsNestedCalls(t *testing.T) {
	var sess *Session
	leaf := testTool("leaf", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "leaf"}}, Usage: nestedTestUsage(7, 0.007)}, nil
	})
	caller := testTool("caller", func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		out := sess.ExecuteTool(ctx, id, "leaf", map[string]any{"q": "x"}, nil)
		return agent.AgentToolResult{Content: out.Result.Content, Usage: nestedTestUsage(1, 0.001)}, nil
	})
	var hookParents []string
	sess = NewSession(SessionOptions{Model: chatModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{caller, leaf}, NoContextFiles: true, NoSkills: true,
		StreamFn: toolCallStream(ai.ToolCall{ID: "t1", Name: "caller", Arguments: map[string]any{}}),
		BeforeToolCall: func(_ context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			hookParents = append(hookParents, c.ToolCall.ID+"<"+c.ParentToolCallID)
			return nil
		},
	})
	var events []string
	sess.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvToolExecutionStart {
			events = append(events, e.ToolCallID+"<"+e.ParentToolCallID)
		}
		return nil
	})
	if _, err := sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(hookParents, ",") != "t1<,t1/1<t1" || strings.Join(events, ",") != "t1<,t1/1<t1" {
		t.Fatalf("hooks %v events %v", hookParents, events)
	}
	var result *ai.ToolResultMessage
	for _, m := range sess.History() {
		if r, ok := messageAsToolResult(m); ok {
			result = r
		}
	}
	if result == nil || result.NestedCalls == nil || len(result.NestedCalls.Calls) != 1 || result.NestedCalls.Calls[0].ID != "t1/1" || !result.NestedCalls.Complete {
		t.Fatalf("tool result %+v", result)
	}
	if result.Usage == nil || result.Usage.Input != 8 || math.Abs(result.Usage.Cost.Total-0.008) > 1e-10 {
		t.Fatalf("usage %+v", result.Usage)
	}
}

// TestSessionNestedHooksAreSerializedWithTheLoop is the review's repro: two
// tools in one parallel batch make nested calls while the loop finalizes the
// other's result. The hooks write shared state without locking; under -race
// they must not race.
func TestSessionNestedHooksAreSerializedWithTheLoop(t *testing.T) {
	var sess *Session
	leaf := testTool("leaf", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	})
	busy := func(name string) agent.AgentTool {
		return testTool(name, func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			var wg sync.WaitGroup
			for range 20 {
				wg.Add(1)
				go func() { defer wg.Done(); sess.ExecuteTool(ctx, id, "leaf", nil, nil) }()
			}
			wg.Wait()
			return agent.AgentToolResult{}, nil
		})
	}
	hookCalls := 0 // written by every hook, deliberately without a lock
	sess = NewSession(SessionOptions{Model: chatModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{busy("a"), busy("b"), leaf}, NoContextFiles: true, NoSkills: true,
		StreamFn: toolCallStream(ai.ToolCall{ID: "a1", Name: "a", Arguments: map[string]any{}}, ai.ToolCall{ID: "b1", Name: "b", Arguments: map[string]any{}}),
		BeforeToolCall: func(context.Context, agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			hookCalls++
			return nil
		},
		AfterToolCall: func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult {
			hookCalls++
			return nil
		},
	})
	eventCount := 0 // likewise
	sess.Subscribe(func(context.Context, agent.AgentEvent) error { eventCount++; return nil })
	if _, err := sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// 2 model calls and 40 nested ones, each through both hooks.
	if hookCalls != 84 {
		t.Fatalf("hook calls %d", hookCalls)
	}
	if eventCount == 0 {
		t.Fatal("no events")
	}
}
