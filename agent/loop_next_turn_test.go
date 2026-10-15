package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// noopToolFor builds a tool that records nothing and always succeeds.
func noopTool() AgentTool {
	return AgentTool{
		Name:        "noop",
		Description: "Noop tool",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}}, nil
		},
	}
}

// nextTurnProbe records the interleaving of provider requests, hook calls and
// turn_start events for one run.
type nextTurnProbe struct {
	mu    sync.Mutex
	trace []string
}

func (p *nextTurnProbe) log(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trace = append(p.trace, s)
}

func (p *nextTurnProbe) got() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.trace, " ")
}

// runProbeLoop drives runAgentLoop with a scripted stream, recording the trace.
func runProbeLoop(t *testing.T, p *nextTurnProbe, cfg AgentLoopConfig, msgs ...*ai.AssistantMessage) {
	t.Helper()
	scripted := scriptedStream(msgs...)
	cfg.Model = testModel
	agentCtx := AgentContext{Tools: []AgentTool{noopTool()}}
	runAgentLoop(context.Background(), []AgentMessage{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "start"}}}},
		agentCtx, cfg,
		func(e AgentEvent) error {
			if e.Type == EvTurnStart {
				p.log("turn_start")
			}
			return nil
		},
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			p.log("request")
			return scripted(ctx, model, req, opts)
		})
}

// TestPrepareNextTurnRunsOnlyBeforeAnActualNextRequest pins upstream 56700d42e:
// preparation moved out of the post-turn_end block and to the head of the next
// inner-loop iteration, so it runs immediately before the provider request it
// prepares for — and NOT at all after the final turn, which no longer has a next
// request to prepare. Under the previous shape this hook fired twice, the second
// time for a turn that never happened.
func TestPrepareNextTurnRunsOnlyBeforeAnActualNextRequest(t *testing.T) {
	p := &nextTurnProbe{}
	runProbeLoop(t, p, AgentLoopConfig{
		PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
			p.log("prepare")
			return nil
		},
	},
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
	)

	// turn_start for turn 1 is emitted by runAgentLoop before the loop starts.
	want := "turn_start request prepare turn_start request"
	if got := p.got(); got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}

// TestFinishTurnEndSkipsQueuesAndPrepareNextTurn pins agent-loop.test.ts
// "action:end skips queue polling and next-turn preparation": the decision sees
// the completed-turn context and is applied first, so a run that ends after its
// turn never polls the queues again nor prepares a turn it will not take.
func TestFinishTurnEndSkipsQueuesAndPrepareNextTurn(t *testing.T) {
	p := &nextTurnProbe{}
	var steeringPolls, followUpPolls int
	runProbeLoop(t, p, AgentLoopConfig{
		FinishTurn: func(_ context.Context, c AgentTurnContext) AgentTurnDecision {
			p.log("finish")
			return TurnEnd
		},
		PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
			p.log("prepare")
			return nil
		},
		GetSteeringMessages: func() []AgentMessage {
			steeringPolls++
			return nil
		},
		GetFollowUpMessages: func() []AgentMessage {
			followUpPolls++
			return []AgentMessage{ai.NewUserText("queued", 0)}
		},
	},
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "should not run"}}, StopReason: ai.StopStop},
	)

	want := "turn_start request finish"
	if got := p.got(); got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
	// Only the startup steering poll ran.
	if steeringPolls != 1 || followUpPolls != 0 {
		t.Fatalf("steeringPolls=%d followUpPolls=%d, want 1 0", steeringPolls, followUpPolls)
	}
}

// TestPrepareNextTurnPicksUpSteeringQueuedWhilePreparing pins the re-poll:
// preparation can be long-running (compaction), so steering queued while it ran
// must reach the turn it was typed for rather than the one after it. Note this
// outcome also held before 56700d42e, by a different mechanism (preparation ran
// BEFORE the end-of-turn poll), so it is not a red-green discriminator for the
// reorder — it is locked by mutation instead: deleting the re-poll turns it red.
func TestPrepareNextTurnPicksUpSteeringQueuedWhilePreparing(t *testing.T) {
	var queued []AgentMessage
	var typed bool

	steered := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "steered"}}}
	var secondRequest ai.TranscriptContext
	scripted := scriptedStream(
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
	)

	cfg := AgentLoopConfig{
		Model: testModel,
		GetSteeringMessages: func() []AgentMessage {
			out := queued
			queued = nil
			return out
		},
		PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
			// The user typed while compaction was running. Once only — this hook
			// runs before every continued turn.
			if !typed {
				typed = true
				queued = []AgentMessage{steered}
			}
			return nil
		},
	}

	var requests int
	agentCtx := AgentContext{Tools: []AgentTool{noopTool()}}
	runAgentLoop(context.Background(), []AgentMessage{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "start"}}}},
		agentCtx, cfg,
		func(e AgentEvent) error { return nil },
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests++
			if requests == 2 {
				secondRequest = req
			}
			return scripted(ctx, model, req, opts)
		})

	if requests != 2 {
		t.Fatalf("expected 2 provider requests, got %d", requests)
	}
	var texts []string
	for _, m := range secondRequest.Messages {
		if u, ok := m.(ai.UserMessage); ok {
			for _, c := range u.Content {
				if tc, ok := c.(ai.TextContent); ok {
					texts = append(texts, tc.Text)
				}
			}
		}
	}
	if !slices.Contains(texts, "steered") {
		t.Fatalf("steering queued during preparation never reached the turn it prepared; user texts = %v", texts)
	}
}

// TestPrepareNextTurnDoesNotDiscardAlreadyPolledSteering pins the
// "len(pending) == 0" guard on that re-poll. Upstream states the hazard as
// double delivery under one-at-a-time steering; in Go the re-poll ASSIGNS
// rather than appends (pending = config.GetSteeringMessages()), so an
// unguarded re-poll manifests as the already-polled message being silently
// thrown away and replaced by the next one. Same guard, same bug class — the
// assertion is written against the failure this implementation can actually
// exhibit, and is mutation-verified against it.
func TestPrepareNextTurnDoesNotDiscardAlreadyPolledSteering(t *testing.T) {
	first := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "first"}}}
	second := ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "second"}}}

	// One message per poll, in order — pi's QueueOneAtATime.
	pending := []AgentMessage{first, second}
	pollAt := 0
	poll := func() []AgentMessage {
		pollAt++
		if pollAt == 1 || len(pending) == 0 {
			return nil // the initial poll, before any turn has run
		}
		out := []AgentMessage{pending[0]}
		pending = pending[1:]
		return out
	}

	scripted := scriptedStream(
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
	)

	var requests int
	var secondRequest ai.TranscriptContext
	cfg := AgentLoopConfig{
		Model:               testModel,
		GetSteeringMessages: poll,
		PrepareNextTurn:     func(c AgentTurnContext) *AgentLoopTurnUpdate { return nil },
	}
	agentCtx := AgentContext{Tools: []AgentTool{noopTool()}}
	runAgentLoop(context.Background(), []AgentMessage{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "start"}}}},
		agentCtx, cfg,
		func(e AgentEvent) error { return nil },
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests++
			if requests == 2 {
				secondRequest = req
			}
			return scripted(ctx, model, req, opts)
		})

	var texts []string
	for _, m := range secondRequest.Messages {
		if u, ok := m.(ai.UserMessage); ok {
			for _, c := range u.Content {
				if tc, ok := c.(ai.TextContent); ok {
					texts = append(texts, tc.Text)
				}
			}
		}
	}
	// Exactly one message per turn. The unguarded re-poll discards "first" and
	// substitutes "second", so both halves are load-bearing.
	if !slices.Contains(texts, "first") {
		t.Fatalf("the already-polled steering message was discarded by the re-poll; user texts = %v", texts)
	}
	if slices.Contains(texts, "second") {
		t.Fatalf("two steering messages landed in one turn; user texts = %v", texts)
	}
}

// TestPrepareNextTurnSnapshotReachesTheNextRequest covers the payload of the
// upstream fix rather than only its timing: a snapshot returned by
// PrepareNextTurn must actually reach the provider request it prepares. The
// replacement Context stands in for the compacted transcript that motivated
// upstream #6879 (its leading system message replaced), the prepared Messages
// append a prompt update to it (upstream 9e05370b2), and ThinkingLevel is
// included because "off" is the one field whose absent and present-but-off
// cases differ (absent keeps the current level, "off" clears it). Without this
// the whole apply path could be deleted with the suite still green.
func TestPrepareNextTurnSnapshotReachesTheNextRequest(t *testing.T) {
	scripted := scriptedStream(
		assistantWithToolCall("tool-1", "noop", map[string]any{}),
		&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "done"}}, StopReason: ai.StopStop},
	)

	replacementModel := &ai.Model{ID: "prepared", Name: "prepared", Api: "faux", Provider: "faux"}
	off := ThinkingLevel("off")

	var requests int
	var seenPrompts []string
	var seenModels []string
	var seenReasoning []ai.ThinkingLevel

	cfg := AgentLoopConfig{
		Model:     testModel,
		Reasoning: ThinkingLevel("high"),
		PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
			next := *c.Context
			next.Messages = append([]AgentMessage{ai.NewSystemText("compacted", 0)}, ai.WithoutInitialSystemMessage(c.Context.Messages)...)
			return &AgentLoopTurnUpdate{
				Context:       &next,
				Messages:      []AgentMessage{ai.NewSystemText("updated guidance", 1)},
				Model:         replacementModel,
				ThinkingLevel: &off,
			}
		},
	}

	original, _ := ai.CreateInitialSystemMessage("original", []ai.Tool{ai.ToToolDeclaration(noopTool().asAITool())})
	agentCtx := AgentContext{Messages: []AgentMessage{original}, Tools: []AgentTool{noopTool()}}
	runAgentLoop(context.Background(), []AgentMessage{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: "start"}}}},
		agentCtx, cfg,
		func(e AgentEvent) error { return nil },
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests++
			seenPrompts = append(seenPrompts, ai.GetCurrentSystemPrompt(req.Messages))
			seenModels = append(seenModels, model.ID)
			seenReasoning = append(seenReasoning, opts.Reasoning)
			return scripted(ctx, model, req, opts)
		})

	if requests != 2 {
		t.Fatalf("expected 2 provider requests, got %d", requests)
	}
	// Turn 1 predates any preparation and must be untouched.
	if seenPrompts[0] != "original" || seenModels[0] != testModel.ID {
		t.Fatalf("turn 1 was already prepared: prompt=%q model=%q", seenPrompts[0], seenModels[0])
	}
	// Turn 2 is the one the snapshot prepared: its context, then its messages.
	if want := "compacted\n\nupdated guidance"; seenPrompts[1] != want {
		t.Fatalf("snapshot Context/Messages never reached the next request: system prompt = %q, want %q", seenPrompts[1], want)
	}
	if seenModels[1] != replacementModel.ID {
		t.Fatalf("snapshot Model never reached the next request: model = %q, want %q", seenModels[1], replacementModel.ID)
	}
	if seenReasoning[1] != "" {
		t.Fatalf(`snapshot ThinkingLevel "off" did not clear reasoning: opts.Reasoning = %q, want ""`, seenReasoning[1])
	}
	if seenReasoning[0] != ai.ThinkingLevel("high") {
		t.Fatalf("turn 1 reasoning was disturbed: %q", seenReasoning[0])
	}
}
