package agent

import (
	"context"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Upstream 540e174c7 (agent-loop.ts streamAssistantResponse): the loop records
// the pi thinking level it requested on every response it hands back —
// config.reasoning ?? "off" — whichever stream function answered, on success
// and on failure alike. It is set on the result object itself, so the
// transcript, message_start's copy (a response that streamed no start event)
// and message_end all carry it.
func TestLoopRecordsRequestedThinkingLevel(t *testing.T) {
	cases := []struct {
		name  string
		level ThinkingLevel
		reply *ai.AssistantMessage
		start bool // whether the stream emits a start event
		want  ai.ModelThinkingLevel
	}{
		{"requested level on success", ThinkHigh, &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop}, true, "high"},
		{"off when reasoning is off", ThinkOff, &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop}, true, "off"},
		{"requested level on failure", ThinkLow, &ai.AssistantMessage{StopReason: ai.StopError, ErrorMessage: "boom"}, true, "low"},
		{"requested level without a start event", ThinkHigh, &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, StopReason: ai.StopStop}, false, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				s := ai.NewAssistantMessageEventStream()
				go func() {
					if tc.start {
						s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{}})
					}
					if tc.reply.StopReason == ai.StopError {
						s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: tc.reply.StopReason, Error: tc.reply})
					} else {
						s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: tc.reply.StopReason, Message: tc.reply})
					}
					s.End()
				}()
				return s
			}
			a := NewAgent(AgentOptions{InitialState: &AgentState{Model: testModel, ThinkingLevel: tc.level}, StreamFn: stream})
			var starts, ends []*ai.AssistantMessage
			a.Subscribe(func(_ context.Context, ev AgentEvent) error {
				if m, ok := ev.Message.(*ai.AssistantMessage); ok {
					switch ev.Type {
					case EvMessageStart:
						starts = append(starts, m)
					case EvMessageEnd:
						ends = append(ends, m)
					}
				}
				return nil
			})
			// A failed response ends the run without an error from Prompt;
			// any error here is the harness, not the case under test.
			if err := a.Prompt(context.Background(), "hi"); err != nil {
				t.Fatal(err)
			}

			msgs := a.State().Messages
			final, ok := msgs[len(msgs)-1].(*ai.AssistantMessage)
			if !ok {
				t.Fatalf("last transcript message is %T, want the response", msgs[len(msgs)-1])
			}
			if final.ThinkingLevel != tc.want {
				t.Errorf("transcript response thinkingLevel = %q, want %q", final.ThinkingLevel, tc.want)
			}
			if len(ends) != 1 || ends[0].ThinkingLevel != tc.want {
				t.Errorf("message_end thinkingLevels = %q, want one %q", thinkingLevels(ends), tc.want)
			}
			if !tc.start && (len(starts) != 1 || starts[0].ThinkingLevel != tc.want) {
				t.Errorf("message_start thinkingLevels = %q, want one %q", thinkingLevels(starts), tc.want)
			}
		})
	}
}

func thinkingLevels(msgs []*ai.AssistantMessage) []ai.ModelThinkingLevel {
	out := make([]ai.ModelThinkingLevel, len(msgs))
	for i, m := range msgs {
		out[i] = m.ThinkingLevel
	}
	return out
}
