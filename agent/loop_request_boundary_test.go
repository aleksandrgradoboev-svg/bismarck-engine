package agent

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The request and turn boundaries of upstream 466db0fec: FinishTurn decides
// what follows a completed turn, and PrepareRequest may replace the runtime
// state immediately before every provider request.

// requestUserTexts returns the text of each user message in a provider request.
func requestUserTexts(req ai.TranscriptContext) []string {
	var out []string
	for _, m := range req.Messages {
		if _, ok := m.(ai.UserMessage); ok {
			out = append(out, userText(m))
		}
	}
	return out
}

// runBoundaryLoop drives runAgentLoop from a "run" prompt with the noop tool,
// counting provider requests and handing each one to onRequest (when set)
// before replying with the next scripted message.
func runBoundaryLoop(cfg AgentLoopConfig, emit EventSink, onRequest func(n int, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions), msgs ...*ai.AssistantMessage) int {
	scripted := scriptedStream(msgs...)
	if cfg.Model == nil {
		cfg.Model = testModel
	}
	if emit == nil {
		emit = func(AgentEvent) error { return nil }
	}
	requests := 0
	runAgentLoop(context.Background(), []AgentMessage{ai.NewUserText("run", 0)},
		AgentContext{Tools: []AgentTool{noopTool()}}, cfg, emit,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests++
			if onRequest != nil {
				onRequest(requests, model, req, opts)
			}
			return scripted(ctx, model, req, opts)
		})
	return requests
}

// agent-loop.test.ts "runs finishTurn after tool-result messages and before
// turn_end".
func TestFinishTurnRunsAfterToolResultsBeforeTurnEnd(t *testing.T) {
	var ordering []string
	terminating := AgentTool{
		Name:        "echo",
		Description: "Echo tool",
		Parameters:  ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: params["value"].(string)}}, Terminate: true}, nil
		},
	}
	cfg := AgentLoopConfig{
		Model: testModel,
		FinishTurn: func(_ context.Context, c AgentTurnContext) AgentTurnDecision {
			ordering = append(ordering, "finishTurn")
			if len(c.ToolResults) != 1 {
				t.Errorf("finishTurn saw %d tool results, want 1", len(c.ToolResults))
			}
			if last := c.Context.Messages[len(c.Context.Messages)-1]; last.MessageRole() != ai.RoleToolResult {
				t.Errorf("finishTurn context ends in %q, want toolResult", last.MessageRole())
			}
			return ""
		},
	}
	scripted := scriptedStream(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
	runAgentLoop(context.Background(), []AgentMessage{ai.NewUserText("echo", 0)},
		AgentContext{Tools: []AgentTool{terminating}}, cfg,
		func(e AgentEvent) error {
			switch e.Type {
			case EvMessageEnd:
				ordering = append(ordering, "message_end:"+string(e.Message.MessageRole()))
			case EvTurnEnd:
				ordering = append(ordering, "turn_end")
			}
			return nil
		}, scripted)

	want := []string{"message_end:toolResult", "finishTurn", "turn_end"}
	if len(ordering) < 3 || !slices.Equal(ordering[len(ordering)-3:], want) {
		t.Fatalf("ordering = %v, want it to end with %v", ordering, want)
	}
}

// agent-loop.test.ts "runs finishTurn for a %s assistant before turn_end
// without changing the hard exit": the hook sees the failed turn, but even a
// continuation request cannot schedule another request or poll the queues.
func TestFinishTurnOnFailedResponseKeepsTheHardExit(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopError, ai.StopAborted} {
		t.Run(string(reason), func(t *testing.T) {
			var ordering []string
			var steeringPolls, followUpPolls int
			requests := runBoundaryLoop(AgentLoopConfig{
				FinishTurn: func(_ context.Context, c AgentTurnContext) AgentTurnDecision {
					if c.Message.StopReason != reason {
						t.Errorf("finishTurn saw stop reason %q, want %q", c.Message.StopReason, reason)
					}
					if c.ToolResults == nil || len(c.ToolResults) != 0 {
						t.Errorf("finishTurn tool results = %#v, want empty", c.ToolResults)
					}
					ordering = append(ordering, "finishTurn")
					return TurnContinue
				},
				GetSteeringMessages: func() []AgentMessage {
					steeringPolls++
					return nil
				},
				GetFollowUpMessages: func() []AgentMessage {
					followUpPolls++
					return []AgentMessage{ai.NewUserText("queued", 0)}
				},
			}, func(e AgentEvent) error {
				if e.Type == EvTurnEnd {
					ordering = append(ordering, "turn_end")
				}
				return nil
			}, nil, &ai.AssistantMessage{StopReason: reason, ErrorMessage: string(reason)})

			if !slices.Equal(ordering, []string{"finishTurn", "turn_end"}) {
				t.Fatalf("ordering = %v, want [finishTurn turn_end]", ordering)
			}
			if requests != 1 || steeringPolls != 1 || followUpPolls != 0 {
				t.Fatalf("requests=%d steeringPolls=%d followUpPolls=%d, want 1 1 0", requests, steeringPolls, followUpPolls)
			}
		})
	}
}

// continueOnce returns a FinishTurn that asks for continuation on its first
// call only, counting every call.
func continueOnce(calls *int) FinishTurnFunc {
	return func(context.Context, AgentTurnContext) AgentTurnDecision {
		*calls++
		if *calls == 1 {
			return TurnContinue
		}
		return ""
	}
}

// agent-loop.test.ts "makes exactly one context-only request when no natural
// request satisfies continuation".
func TestFinishTurnContinueMakesOneContextOnlyRequest(t *testing.T) {
	var finishCalls int
	requests := runBoundaryLoop(AgentLoopConfig{FinishTurn: continueOnce(&finishCalls)}, nil, nil,
		textMessage("response 1"), textMessage("response 2"), textMessage("response 3"))

	if requests != 2 || finishCalls != 2 {
		t.Fatalf("requests=%d finishCalls=%d, want 2 2", requests, finishCalls)
	}
}

// agent-loop.test.ts "lets a natural tool-result request satisfy
// continuation".
func TestFinishTurnContinueIsSatisfiedByToolResults(t *testing.T) {
	var finishCalls int
	requests := runBoundaryLoop(AgentLoopConfig{FinishTurn: continueOnce(&finishCalls)}, nil, nil,
		assistantWithToolCall("tool-1", "noop", map[string]any{}), textMessage("done"), textMessage("extra"))

	if requests != 2 || finishCalls != 2 {
		t.Fatalf("requests=%d finishCalls=%d, want 2 2", requests, finishCalls)
	}
}

// agent-loop.test.ts "lets a natural %s request satisfy continuation": the
// queued message carries the continuation, so no context-only request follows
// it.
func TestFinishTurnContinueIsSatisfiedByQueuedMessages(t *testing.T) {
	for _, kind := range []string{"steering", "follow-up"} {
		t.Run(kind, func(t *testing.T) {
			queued := ai.NewUserText(kind, 0)
			var finishCalls, steeringPolls int
			followUpDelivered := false
			var secondRequestUsers []string
			requests := runBoundaryLoop(AgentLoopConfig{
				FinishTurn: continueOnce(&finishCalls),
				GetSteeringMessages: func() []AgentMessage {
					steeringPolls++
					if kind == "steering" && steeringPolls == 2 {
						return []AgentMessage{queued}
					}
					return nil
				},
				GetFollowUpMessages: func() []AgentMessage {
					if kind != "follow-up" || followUpDelivered {
						return nil
					}
					followUpDelivered = true
					return []AgentMessage{queued}
				},
			}, nil, func(n int, _ *ai.Model, req ai.TranscriptContext, _ *ai.SimpleStreamOptions) {
				if n == 2 {
					secondRequestUsers = requestUserTexts(req)
				}
			}, textMessage("done"), textMessage("done"), textMessage("extra"))

			if requests != 2 || finishCalls != 2 {
				t.Fatalf("requests=%d finishCalls=%d, want 2 2", requests, finishCalls)
			}
			if !slices.Contains(secondRequestUsers, kind) {
				t.Fatalf("second request user texts = %v, want %q among them", secondRequestUsers, kind)
			}
		})
	}
}

// agent-loop.test.ts "prepares the initial request after pending messages and
// can replace request state".
func TestPrepareRequestRunsBeforeTheFirstRequestAfterPendingMessages(t *testing.T) {
	replacementModel := &ai.Model{ID: "replacement", Name: "replacement", Api: "faux", Provider: "faux"}
	canonical := ai.NewUserText("canonical projection", 0)
	steering := ai.NewUserText("steering", 0)
	high := ThinkHigh
	steeringDelivered := false
	var completed []string
	var prepareCalls int
	var sawModel string
	var sawUsers []string
	var sawReasoning ai.ThinkingLevel

	cfg := AgentLoopConfig{
		GetSteeringMessages: func() []AgentMessage {
			if steeringDelivered {
				return nil
			}
			steeringDelivered = true
			return []AgentMessage{steering}
		},
		PrepareRequest: func(_ context.Context, r PrepareRequestContext) *AgentRequestUpdate {
			prepareCalls++
			if !slices.Contains(completed, "steering") {
				t.Errorf("prepareRequest ran before the steering message was emitted; completed = %v", completed)
			}
			if !slices.ContainsFunc(r.Context.Messages, func(m AgentMessage) bool {
				u, ok := m.(ai.UserMessage)
				return ok && textOfContent(u.Content) == "steering"
			}) {
				t.Errorf("prepareRequest context lacks the steering message")
			}
			next := *r.Context
			next.Messages = []AgentMessage{canonical}
			return &AgentRequestUpdate{Context: &next, Model: replacementModel, ThinkingLevel: &high}
		},
	}
	runBoundaryLoop(cfg, func(e AgentEvent) error {
		if e.Type == EvMessageEnd {
			if u, ok := e.Message.(ai.UserMessage); ok {
				completed = append(completed, textOfContent(u.Content))
			}
		}
		return nil
	}, func(_ int, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) {
		sawModel = model.ID
		sawUsers = requestUserTexts(req)
		sawReasoning = opts.Reasoning
	}, textMessage("done"))

	if prepareCalls != 1 {
		t.Fatalf("prepareRequest ran %d times, want 1", prepareCalls)
	}
	if sawModel != replacementModel.ID {
		t.Fatalf("request model = %q, want the replacement %q", sawModel, replacementModel.ID)
	}
	if !slices.Equal(sawUsers, []string{"canonical projection"}) {
		t.Fatalf("request user texts = %v, want only the replacement context", sawUsers)
	}
	if sawReasoning != ai.ThinkingLevel("high") {
		t.Fatalf("request reasoning = %q, want high", sawReasoning)
	}
}

// agent-loop.test.ts "does not poll steering after prepareRequest": steering
// queued by the hook waits for the next natural poll.
func TestPrepareRequestDoesNotPollSteering(t *testing.T) {
	late := ai.NewUserText("late steering", 0)
	var queued []AgentMessage
	var preparations, steeringPolls int
	var includedSteering []bool
	runBoundaryLoop(AgentLoopConfig{
		GetSteeringMessages: func() []AgentMessage {
			steeringPolls++
			out := queued
			queued = nil
			return out
		},
		PrepareRequest: func(context.Context, PrepareRequestContext) *AgentRequestUpdate {
			preparations++
			if preparations == 1 {
				queued = append(queued, late)
			}
			return nil
		},
	}, nil, func(_ int, _ *ai.Model, req ai.TranscriptContext, _ *ai.SimpleStreamOptions) {
		includedSteering = append(includedSteering, slices.Contains(requestUserTexts(req), "late steering"))
	}, textMessage("done"), textMessage("done"))

	if !slices.Equal(includedSteering, []bool{false, true}) {
		t.Fatalf("requests included the late steering = %v, want [false true]", includedSteering)
	}
	// Startup, post-turn delivery, then the final natural-stop check.
	if preparations != 2 || steeringPolls != 3 {
		t.Fatalf("preparations=%d steeringPolls=%d, want 2 3", preparations, steeringPolls)
	}
}

// TestPrepareRequestReplacementPersistsAndMapsOff locks the two halves of the
// thinking-level contract along with persistence: the hook is handed
// config.reasoning ?? "off", a returned "off" clears reasoning, and a
// replacement returned once stays in force for the later requests of the run.
func TestPrepareRequestReplacementPersistsAndMapsOff(t *testing.T) {
	replacementModel := &ai.Model{ID: "replacement", Name: "replacement", Api: "faux", Provider: "faux"}
	off := ThinkOff
	var handed []string
	var sent []string
	runBoundaryLoop(AgentLoopConfig{
		Reasoning: ThinkHigh,
		PrepareRequest: func(_ context.Context, r PrepareRequestContext) *AgentRequestUpdate {
			handed = append(handed, fmt.Sprintf("%s/%s", r.Model.ID, r.ThinkingLevel))
			if len(handed) == 1 {
				return &AgentRequestUpdate{Model: replacementModel, ThinkingLevel: &off}
			}
			return nil
		},
	}, nil, func(_ int, model *ai.Model, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions) {
		sent = append(sent, fmt.Sprintf("%s/%q", model.ID, opts.Reasoning))
	}, assistantWithToolCall("tool-1", "noop", map[string]any{}), textMessage("done"))

	if want := []string{"faux/high", "replacement/off"}; !slices.Equal(handed, want) {
		t.Fatalf("prepareRequest was handed %v, want %v", handed, want)
	}
	if want := []string{`replacement/""`, `replacement/""`}; !slices.Equal(sent, want) {
		t.Fatalf("requests sent %v, want %v", sent, want)
	}
}
