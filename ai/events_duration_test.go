package ai

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Response timing of the assistant event stream (pi event-stream.test.ts,
// "AssistantMessageEventStream timing", upstream 36a686ee).

func timedMessage(timestamp int64, durationMs ...int64) *AssistantMessage {
	m := &AssistantMessage{Api: "openai-responses", Provider: "openai", Model: "m", StopReason: StopStop, Timestamp: timestamp}
	if len(durationMs) > 0 {
		m.DurationMs = &durationMs[0]
	}
	return m
}

func TestAssistantStreamTimesTheFinalMessage(t *testing.T) {
	done := NewAssistantMessageEventStream()
	answer := timedMessage(time.Now().UnixMilli())
	time.Sleep(20 * time.Millisecond)
	done.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	if answer.DurationMs == nil || *answer.DurationMs < 15 {
		t.Fatalf("done: durationMs = %v, want >= 15", answer.DurationMs)
	}
	if got := done.Result(); got.DurationMs != answer.DurationMs {
		t.Fatal("result is not the timed message")
	}

	failed := NewAssistantMessageEventStream()
	errMsg := timedMessage(time.Now().UnixMilli())
	errMsg.StopReason = StopError
	failed.Push(AssistantMessageEvent{Type: EventError, Reason: StopError, Error: errMsg})
	if errMsg.DurationMs == nil || *errMsg.DurationMs < 0 {
		t.Fatalf("error: durationMs = %v", errMsg.DurationMs)
	}

	ended := NewAssistantMessageEventStream()
	result := timedMessage(time.Now().UnixMilli())
	ended.End(result)
	if result.DurationMs == nil {
		t.Fatal("end: durationMs not set")
	}
}

func TestAssistantStreamKeepsAnExistingDuration(t *testing.T) {
	outer := NewAssistantMessageEventStream()
	time.Sleep(20 * time.Millisecond)
	inner := NewAssistantMessageEventStream()
	answer := timedMessage(time.Now().UnixMilli())
	inner.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	measured := *answer.DurationMs
	outer.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: answer})
	if *answer.DurationMs != measured || measured >= 20 {
		t.Fatalf("durationMs = %d, measured %d", *answer.DurationMs, measured)
	}

	preset := timedMessage(time.Now().UnixMilli(), 1234)
	NewAssistantMessageEventStream().Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: preset})
	if *preset.DurationMs != 1234 {
		t.Fatalf("preset durationMs = %d", *preset.DurationMs)
	}
}

func TestAssistantStreamLeavesAnEarlierResponseUntimed(t *testing.T) {
	fetched := timedMessage(time.Now().UnixMilli() - 60_000)
	NewAssistantMessageEventStream().Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: fetched})
	if fetched.DurationMs != nil {
		t.Fatalf("a deferred result fetched later got durationMs %d", *fetched.DurationMs)
	}
}

func TestAssistantStreamDoesNotTimeAfterCompletion(t *testing.T) {
	stream := NewAssistantMessageEventStream()
	stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: timedMessage(time.Now().UnixMilli())})
	late := timedMessage(time.Now().UnixMilli())
	stream.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: late})
	stream.End(late)
	if late.DurationMs != nil {
		t.Fatal("a message pushed after completion was timed")
	}
}

// durationMs is the assistant message's last key (pi sets it on the built
// message) and sits between isError and timestamp on a tool result (pi's
// createToolResultMessage).
func TestDurationKeyOrder(t *testing.T) {
	d := int64(7)
	raw, err := json.Marshal(AssistantMessage{StopReason: StopStop, Timestamp: 1, DurationMs: &d})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), `"timestamp":1,"durationMs":7}`) {
		t.Fatalf("assistant message: %s", raw)
	}
	raw, err = json.Marshal(ToolResultMessage{ToolCallID: "c", ToolName: "t", IsError: true, DurationMs: &d, Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"isError":true,"durationMs":7,"timestamp":1`) {
		t.Fatalf("tool result: %s", raw)
	}
	raw, err = json.Marshal(ToolResultMessage{ToolCallID: "c", ToolName: "t", Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "durationMs") {
		t.Fatalf("an untimed result carries durationMs: %s", raw)
	}
}
