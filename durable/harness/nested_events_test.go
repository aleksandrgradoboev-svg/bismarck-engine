package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-nested-tools.test.ts: the
// cases that read nested calls' tool events.

// toolLifecycle is [type, call ID, parent call ID] of each start and end.
func toolLifecycle(events []AgentEvent) []string {
	var out []string
	for _, event := range events {
		switch e := event.(type) {
		case ToolExecutionStartEvent:
			out = append(out, e.EventType()+" "+e.ToolCallID+" "+e.ParentToolCallID)
		case ToolExecutionEndEvent:
			out = append(out, e.EventType()+" "+e.ToolCallID+" "+e.ParentToolCallID)
		}
	}
	return out
}

func endOf(events []AgentEvent, callID string) *ToolExecutionEndEvent {
	for _, event := range events {
		if end, ok := event.(ToolExecutionEndEvent); ok && end.ToolCallID == callID {
			return &end
		}
	}
	return nil
}

func TestNestedToolsReportNestedCallsInPiLiveNestedToolsAndAsToolEventsWithTheirParent(t *testing.T) {
	setup := newChatSetup(t)
	release := newLatch()
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("slow", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if err := api.Output([]byte("working\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		close(running)
		<-release.ch
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "slow done"}}}, nil
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		result, err := api.ExecuteTool(ctx, "slow", map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: nestedText(result)}}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	openAtCleanup(t, release)
	stream, log := listen(t, root)
	if got := jsonOf(t, stream.Snapshot.NestedTools); got != `[]` {
		t.Fatalf("snapshot nestedTools = %s", got)
	}
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, running, "the nested call")
	slots := liveNested(t, h, root)
	if len(slots) != 1 {
		t.Fatalf("slots = %s", jsonOf(t, slots))
	}
	want := fmt.Sprintf(`{"arguments":{},"callId":"c1/1","details":{"step":1},"name":"slow","output":"working\n","parentCallId":"c1","parentTaskId":%v,"status":"running","taskId":%v}`, slots[0]["parentTaskId"], slots[0]["taskId"])
	if got := jsonOf(t, slots[0]); got != want {
		t.Fatalf("slot = %s, want %s", got, want)
	}
	release.open()
	waitFor(t, submission)
	must(t, h.WaitForIdle(bg))
	log.quiet(t)
	all := log.events()
	expectStrings(t, "tool events", toolLifecycle(all), []string{
		"tool_execution_start c1 ",
		"tool_execution_start c1/1 c1",
		"tool_execution_end c1/1 c1",
		"tool_execution_end c1 ",
	})
	nestedEnd := endOf(all, "c1/1")
	if nestedEnd.Result == nil || nestedEnd.Result.IsError || nestedText(*nestedEnd.Result) != "slow done" {
		t.Fatalf("nested end = %s", jsonOf(t, nestedEnd))
	}
	// Events name the call's task and, for a nested call, the calling one.
	tasks := allToolTasks(t, h, root)
	parent, child := tasks[0], tasks[1]
	if nestedEnd.TaskID == nil || *nestedEnd.TaskID != child.ID || nestedEnd.ParentTaskID == nil || *nestedEnd.ParentTaskID != parent.ID {
		t.Fatalf("nested end = %s", jsonOf(t, nestedEnd))
	}
	if parentEnd := endOf(all, "c1"); parentEnd.TaskID == nil || *parentEnd.TaskID != parent.ID || parentEnd.ParentTaskID != nil {
		t.Fatalf("parent end = %s", jsonOf(t, parentEnd))
	}
	updated := false
	for _, event := range all {
		if update, ok := event.(ToolExecutionUpdateEvent); ok && update.ToolCallID == "c1/1" && update.ParentToolCallID == "c1" {
			updated = true
		}
	}
	if !updated {
		t.Fatal("no update of the nested call")
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsReportTheOutputOfSeveralRunningNestedCallsAndShowThemToALateSubscriber(t *testing.T) {
	setup := newChatSetup(t)
	release := newLatch()
	running := map[string]chan struct{}{"one": make(chan struct{}), "two": make(chan struct{})}
	wrote := map[string]*latch{"one": newLatch(), "two": newLatch()}
	installTool(t, setup.registry, nestedTool("slow", func(ctx context.Context, args map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		name := args["text"].(string)
		close(running[name])
		<-wrote[name].ch
		if err := api.Output([]byte(name+"\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"name": name}); err != nil {
			return ToolExecutionResult{}, err
		}
		<-release.ch
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// Admitted in call order, as pi's synchronous admissions are.
		done := make(chan error, 1)
		go func() {
			_, err := api.ExecuteTool(ctx, "slow", map[string]any{"text": "one"}, NestedCallOptions{})
			done <- err
		}()
		<-running["one"]
		_, err := api.ExecuteTool(ctx, "slow", map[string]any{"text": "two"}, NestedCallOptions{})
		if first := <-done; first != nil {
			return ToolExecutionResult{}, first
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	openAtCleanup(t, release, wrote["one"], wrote["two"])
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, running["one"], "the first call")
	awaitSignal(t, running["two"], "the second call")
	// The second call's slot is running once its tool runs.
	eventually(t, func() bool {
		slots := liveNested(t, h, root)
		return len(slots) == 2 && slots[1]["status"] == slotRunning
	})
	stream, log := listen(t, root)
	var snapshot []string
	for _, slot := range stream.Snapshot.NestedTools {
		snapshot = append(snapshot, slot.CallID+" "+slot.Status)
	}
	expectStrings(t, "snapshot", snapshot, []string{"c1/1 running", "c1/2 running"})
	updates := func() []string {
		var out []string
		for _, event := range log.events() {
			if update, ok := event.(ToolExecutionUpdateEvent); ok && update.Output != nil {
				out = append(out, update.ToolCallID+" "+jsonOf(t, update.Output))
			}
		}
		return out
	}
	// The second slot writes first, so a wrong slot index in the update path
	// would show.
	wrote["two"].open()
	eventually(t, func() bool { return len(updates()) == 1 })
	wrote["one"].open()
	eventually(t, func() bool { return len(updates()) == 2 })
	release.open()
	waitFor(t, submission)
	expectStrings(t, "updates", updates(), []string{`c1/2 {"set":"two\n"}`, `c1/1 {"set":"one\n"}`})
}

// Beyond pi, whose nested output updates are all a slot's first output, a
// set: a later write is an append on the nested slot's own path.
func TestNestedToolsReportALaterOutputOfANestedCallAsAnAppend(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, nestedTool("chatty", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for i, line := range []string{"one\n", "two\n"} {
			if err := api.Output([]byte(line), nil); err != nil {
				return ToolExecutionResult{}, err
			}
			// Each write commits before the next.
			if err := api.Details(ctx, map[string]any{"n": i}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installTool(t, setup.registry, batchCalling("chatty"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	_, log := listen(t, root)
	waitFor(t, submit(t, root, inputDraft("go")))
	must(t, h.WaitForIdle(bg))
	log.quiet(t)
	var updates []string
	for _, event := range log.events() {
		if update, ok := event.(ToolExecutionUpdateEvent); ok && update.Output != nil && update.ToolCallID == "c1/1" {
			updates = append(updates, jsonOf(t, update.Output))
		}
	}
	expectStrings(t, "updates", updates, []string{`{"set":"one\n"}`, `{"append":"two\n"}`})
}

func TestNestedToolsLoseTheRunningOutputOfANestedCallMadeWithoutProgressWhenItIsAborted(t *testing.T) {
	setup := newChatSetup(t)
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("chatty", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if err := api.Output([]byte("line\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "note"}); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		close(running)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		go func() { _, _ = api.ExecuteTool(ctx, "chatty", map[string]any{}, NestedCallOptions{NoProgress: true}) }()
		<-running
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	_, log := listen(t, root)
	waitFor(t, submit(t, root, inputDraft("go")))
	must(t, h.WaitForIdle(bg))
	log.quiet(t)
	// The abort's result is built from the slot, which never got the output.
	end := endOf(log.events(), "c1/1")
	if end == nil || end.Result == nil {
		t.Fatalf("nested end = %s", jsonOf(t, end))
	}
	if result := end.Result; !result.IsError || result.StructuredOutput != nil || result.Details != nil {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
	expectStrings(t, "codes", codes(end.Result.Diagnostics), []string{"aborted"})
}
