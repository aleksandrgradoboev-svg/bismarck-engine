package harness

import (
	"context"
	"fmt"
	"math"
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

// Ported from pi packages/durable/test/harness-live-deltas.test.ts: the tool
// cases (the two without tools came with the conversation view).

// liveCommits records the operations of every pi.live commit with any.
type liveCommits struct {
	mu      sync.Mutex
	commits [][]string
}

func recordLive(t *testing.T, h *Harness) *liveCommits {
	t.Helper()
	l := &liveCommits{}
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && len(document.Ops) > 0 {
				ops := make([]string, len(document.Ops))
				for i, op := range document.Ops {
					ops[i] = opText(t, op)
				}
				l.mu.Lock()
				l.commits = append(l.commits, ops)
				l.mu.Unlock()
			}
		}
	})
	must(t, err)
	t.Cleanup(unsubscribe)
	return l
}

func (l *liveCommits) all() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.commits)
}

// liveDrive drives one tool call step by step.
type liveDrive struct {
	t       *testing.T
	h       *Harness
	root    *Conversation
	live    *liveCommits
	actions chan func(ToolExecutionAPI) bool
	done    *Submission
}

func drive(t *testing.T, limits *OutputLimits) *liveDrive {
	t.Helper()
	setup := newChatSetup(t)
	d := &liveDrive{t: t, actions: make(chan func(ToolExecutionAPI) bool)}
	tool := &ToolRegistration{
		Tool:         ai.Tool{Name: "drive", Description: "Driven by the test", Parameters: objectSchema()},
		OutputLimits: limits,
		Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			for {
				select {
				case action := <-d.actions:
					if !action(api) {
						return ToolExecutionResult{}, nil
					}
				case <-ctx.Done():
					return ToolExecutionResult{}, context.Cause(ctx)
				}
			}
		},
	}
	installTool(t, setup.registry, tool)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("drive", nil, "c1"))), fauxText("done")})
	d.h, d.root = openChat(t, memory.New(), setup)
	d.live = recordLive(t, d.h)
	d.done = submit(t, d.root, inputDraft("go"))
	// The intent commit must have reached the recorder too, not only the
	// state: commit subscribers run after the state is readable, so a step
	// would otherwise take it for its own commit.
	eventually(t, func() bool {
		slots := liveSlots(t, d.h, d.root)
		return len(slots) > 0 && slots[0].Status == slotRunning && slices.ContainsFunc(d.live.all(), func(ops []string) bool {
			return slices.Contains(ops, `s ["tools",0,"status"] "running"`)
		})
	})
	return d
}

func liveSlots(t *testing.T, h *Harness, conversation *Conversation) []ToolSlot {
	t.Helper()
	var live struct {
		Tools []ToolSlot `json:"tools"`
	}
	must(t, fromValue(liveOf(t, h, conversation), &live))
	return live.Tools
}

// step runs action inside the tool and returns the operations of the commit
// it caused.
func (d *liveDrive) step(action func(api ToolExecutionAPI)) []string {
	d.t.Helper()
	before := len(d.live.all())
	d.actions <- func(api ToolExecutionAPI) bool {
		action(api)
		return true
	}
	eventually(d.t, func() bool { return len(d.live.all()) > before })
	commits := d.live.all()
	return commits[before]
}

// finish lets the tool return and the run finish; it returns the commits
// made meanwhile.
func (d *liveDrive) finish() [][]string {
	d.t.Helper()
	before := len(d.live.all())
	d.actions <- func(ToolExecutionAPI) bool { return false }
	waitFor(d.t, d.done)
	return d.live.all()[before:]
}

func expectOps(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// expectContains checks that ops hold every one of want and n ops in all
// (n < 0: any number).
func expectContains(t *testing.T, what string, ops []string, n int, want ...string) {
	t.Helper()
	for _, op := range want {
		if !slices.Contains(ops, op) {
			t.Fatalf("%s = %q, missing %q", what, ops, op)
		}
	}
	if n >= 0 && len(ops) != n {
		t.Fatalf("%s = %q, want %d ops", what, ops, n)
	}
}

const outputPath = `["tools",0,"output"]`

func TestLiveHandsAGenerationOverToItsToolRoundAndStartsAToolWithOneFieldWriteEach(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, &ToolRegistration{Tool: ai.Tool{Name: "noop", Description: "noop", Parameters: objectSchema()}, Execute: noOutput})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("noop", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	live := recordLive(t, h)
	waitFor(t, submit(t, root, inputDraft("go")))
	generations := conversationTasks(t, h, root, "pi.generation")
	slices.SortFunc(generations, func(a, b durable.TaskRecord) int { return int(a.ID - b.ID) })
	tool := conversationTasks(t, h, root, "pi.tool")[0].ID
	var result durable.EntryID
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == durable.ToolResultEntry.Kind {
			result = entry.ID
		}
	}
	commits := live.all()
	if len(commits) != 8 {
		t.Fatalf("commits = %q, want 8", commits)
	}
	// submission
	if len(commits[0]) != 1 || !strings.HasPrefix(commits[0][0], `s ["run"] {"inputs":[`) || !strings.HasSuffix(commits[0][0], fmt.Sprintf(`],"taskId":%d}`, generations[0].ID)) {
		t.Fatalf("submission = %q", commits[0])
	}
	// request
	expectOps(t, "request", commits[1], `s ["generation"] {"attempt":1}`)
	// the generation starts its tool round and keeps the run
	expectContains(t, "tool round", commits[2], 2, `d ["generation"]`, fmt.Sprintf(`s ["tools"] [{"callId":"c1","name":"noop","status":"pending","taskId":%d}]`, tool))
	// intent
	expectOps(t, "intent", commits[3], `s ["tools",0,"status"] "running"`)
	// result
	expectContains(t, "result", commits[4], 2, `s ["tools",0,"status"] "done"`, fmt.Sprintf(`s ["tools",0,"entry"] %d`, result))
	// the generation's tools phase hands the run to the next generation
	expectContains(t, "handover", commits[5], 2, `d ["tools"]`, fmt.Sprintf(`s ["run","taskId"] %d`, generations[1].ID))
	expectOps(t, "second request", commits[6], `s ["generation"] {"attempt":1}`)
	// the answer ends the run
	expectContains(t, "end", commits[7], -1, `d ["run"]`, `d ["generation"]`)
}

func TestLiveAppendsHeadOutputAndThenOnlyUpdatesTheDroppedCountsOnceTheWindowIsFull(t *testing.T) {
	run := drive(t, &OutputLimits{MaxLines: 2})
	out := func(text string) func(ToolExecutionAPI) {
		return func(api ToolExecutionAPI) { must(t, api.Output([]byte(text), nil)) }
	}
	expectOps(t, "one", run.step(out("one\n")), `s `+outputPath+` "one\n"`)
	expectOps(t, "two", run.step(out("two\n")), `a `+outputPath+` "two\n"`)
	// The window is full: the retained text stays; only the counts change.
	expectOps(t, "three", run.step(out("three\n")), `s ["tools",0,"droppedBytes"] 6`, `s ["tools",0,"droppedLines"] 1`)
	expectOps(t, "four", run.step(out("four\n")), `s ["tools",0,"droppedBytes"] 11`, `s ["tools",0,"droppedLines"] 2`)
	run.finish()
}

func TestLiveSlidesATailWindowAsAFrontTrimPlusAnAppend(t *testing.T) {
	run := drive(t, &OutputLimits{MaxLines: 3, Retain: "tail"})
	out := func(text string) func(ToolExecutionAPI) {
		return func(api ToolExecutionAPI) { must(t, api.Output([]byte(text), nil)) }
	}
	expectOps(t, "first", run.step(out("line 1\nline 2\nline 3\n")), `s `+outputPath+` "line 1\nline 2\nline 3\n"`)
	expectOps(t, "slide", run.step(out("line 4\n")),
		`t `+outputPath+` 7`, `a `+outputPath+` "line 4\n"`, `s ["tools",0,"droppedBytes"] 7`, `s ["tools",0,"droppedLines"] 1`)
	// The buffer keeps only the window, and later slides stay minimal and
	// exact.
	expectOps(t, "second slide", run.step(out("line 5\nline 6\n")),
		`t `+outputPath+` 14`, `a `+outputPath+` "line 5\nline 6\n"`, `s ["tools",0,"droppedBytes"] 21`, `s ["tools",0,"droppedLines"] 3`)
	if output := liveSlots(t, run.h, run.root)[0].Output; output == nil || *output != "line 4\nline 5\nline 6\n" {
		t.Fatalf("slot output = %v", output)
	}
	run.finish()
}

func TestLiveWritesTheWholeWindowWhenChordsOverlapSearchCannotFindTheSharedPart(t *testing.T) {
	// A retained window beyond the 64 KiB overlap scan.
	wide := drive(t, &OutputLimits{MaxBytes: 100 * 1024, MaxLines: 1_000_000, Retain: "tail"})
	line := func(index int) string { return fmt.Sprintf("%010d %s\n", index, strings.Repeat("x", 989)) }
	var text strings.Builder
	for index := range 100 {
		text.WriteString(line(index))
	}
	wide.step(func(api ToolExecutionAPI) { must(t, api.Output([]byte(text.String()), nil)) })
	slid := wide.step(func(api ToolExecutionAPI) {
		must(t, api.Output([]byte(line(100)+line(101)+line(102)+line(103)), nil))
	})
	var outputOps []string
	for _, op := range slid {
		if strings.Contains(op, outputPath) {
			outputOps = append(outputOps, op[:1])
		}
	}
	expectOps(t, "wide slide", outputOps, "s")
	wide.finish()

	// Repetitive output still finds an overlap here; the bounded candidate
	// search can give up on other inputs and then writes one window.
	repetitive := drive(t, &OutputLimits{MaxLines: 50, Retain: "tail"})
	repetitive.step(func(api ToolExecutionAPI) { must(t, api.Output([]byte(strings.Repeat("y\n", 50)), nil)) })
	repeated := repetitive.step(func(api ToolExecutionAPI) { must(t, api.Output([]byte("z\n"), nil)) })
	var repeatedOutput []string
	for _, op := range repeated {
		if strings.Contains(op, outputPath) {
			repeatedOutput = append(repeatedOutput, op)
		}
	}
	expectOps(t, "repetitive slide", repeatedOutput, `t `+outputPath+` 2`, `a `+outputPath+` "z\n"`)
	repetitive.finish()
}

func TestLiveDiffsDetailsLeafByLeafAndAppendsDiagnostics(t *testing.T) {
	run := drive(t, nil)
	details := `["tools",0,"details"]`
	setDetails := func(value map[string]any) func(ToolExecutionAPI) {
		return func(api ToolExecutionAPI) { must(t, api.Details(bg, value)) }
	}
	expectOps(t, "first details", run.step(setDetails(map[string]any{"step": 1, "log": "a"})), `s `+details+` {"log":"a","step":1}`)
	expectContains(t, "second details", run.step(setDetails(map[string]any{"step": 2, "log": "ab"})), -1,
		`s ["tools",0,"details","step"] 2`, `a ["tools",0,"details","log"] "b"`)
	expectOps(t, "third details", run.step(setDetails(map[string]any{"step": 2})), `d ["tools",0,"details","log"]`)
	diagnostics := `["tools",0,"diagnostics"]`
	expectOps(t, "first diagnostic", run.step(func(api ToolExecutionAPI) {
		must(t, api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "first"}))
	}), `s `+diagnostics+` [{"message":"first","severity":"info"}]`)
	expectOps(t, "second diagnostic", run.step(func(api ToolExecutionAPI) {
		must(t, api.Diagnostic(ToolDiagnostic{Severity: "warn", Message: "second"}))
	}), `p `+diagnostics+` 1 0 [{"message":"second","severity":"warn"}]`)
	// Settlement moves everything into the result entry and keeps the slot
	// small.
	settled := run.finish()[0]
	expectContains(t, "settlement", settled, -1, `s ["tools",0,"status"] "done"`, `d `+details, `d `+diagnostics)
}

// commitKinds records whether each commit wrote pi.live as a base or a
// delta.
type commitKinds struct {
	*memory.Storage
	mu      sync.Mutex
	liveID  durable.DocumentID
	written map[durable.Seq]string
}

func (s *commitKinds) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	id := s.liveID
	s.mu.Unlock()
	var kind string
	for _, write := range writes {
		if change, ok := write.(durable.DocumentChangeWrite); ok && change.ID == id {
			kind = change.Content.Kind
		}
	}
	seq, err := s.Storage.Commit(ctx, writes)
	if err == nil && kind != "" {
		s.mu.Lock()
		s.written[seq] = kind
		s.mu.Unlock()
	}
	return seq, err
}

func TestLiveStoresACompleteBaseExactlyInTheCommitsWhereNothingRuns(t *testing.T) {
	setup := newChatSetup(t)
	for _, name := range []string{"first", "second"} {
		installTool(t, setup.registry, &ToolRegistration{
			Tool: ai.Tool{Name: name, Description: name, Parameters: objectSchema()},
			Execute: func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
				must(t, api.Output([]byte(name+" output\n"), nil))
				time.Sleep(150 * time.Millisecond)
				must(t, api.Output([]byte(name+" more\n"), nil))
				return ToolExecutionResult{}, nil
			},
		})
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("first", nil, "a"), call("second", nil, "b"))), fauxText("done")})
	setup.setSettings(func(s *HarnessSettings) { s.ToolExecution = ToolExecutionSequential })
	storage := &commitKinds{Storage: memory.New(), written: map[durable.Seq]string{}}
	h, root := openChat(t, storage, setup)
	var mu sync.Mutex
	values := map[durable.Seq]map[string]any{}
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind {
				storage.mu.Lock()
				storage.liveID = document.Record.ID
				storage.mu.Unlock()
				if document.Value != nil {
					mu.Lock()
					values[publication.Seq] = document.Value
					mu.Unlock()
				}
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	waitFor(t, submit(t, root, inputDraft("go")))
	nothingRuns := func(value map[string]any) bool {
		if _, generating := value["generation"]; generating {
			return false
		}
		slots, _ := value["tools"].([]any)
		for _, slot := range slots {
			if status, _ := slot.(map[string]any)["status"].(string); status == slotRunning {
				return false
			}
		}
		return true
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	bases, deltas := 0, 0
	for seq, kind := range storage.written {
		value, ok := values[seq]
		if !ok {
			continue
		}
		if (kind == durable.ContentBase) != nothingRuns(value) {
			t.Fatalf("commit %d wrote a %s for %v", seq, kind, value)
		}
		if kind == durable.ContentBase {
			bases++
		} else {
			deltas++
		}
	}
	// Bases at the handover, after each sequential tool, after the round,
	// and when the run ends.
	if bases < 5 || deltas == 0 {
		t.Fatalf("bases %d, deltas %d", bases, deltas)
	}
}

func TestLiveStartsCallsTheRequestDidNotOfferAsDoneAndMarksAFaultedToolsSlotDoneWithoutAnEntry(t *testing.T) {
	setup := newChatSetup(t)
	// Not strict JSON: the result commit fails and the scheduler faults the
	// task (pi: a function in the details).
	installTool(t, setup.registry, &ToolRegistration{
		Tool: ai.Tool{Name: "bad", Description: "bad", Parameters: objectSchema()},
		Execute: func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
			return ToolExecutionResult{Output: ai.ContentList{}, Details: map[string]any{"n": math.NaN()}}, nil
		},
	})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("ghost", nil, "g"), call("bad", nil, "b"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	live := recordLive(t, h)
	waitFor(t, submit(t, root, inputDraft("go")))
	var ghost durable.EntryID
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == durable.ToolResultEntry.Kind {
			ghost = entry.ID
			break
		}
	}
	bad := conversationTasks(t, h, root, "pi.tool")[0].ID
	commits := live.all()
	var handover []string
	for _, ops := range commits {
		if slices.ContainsFunc(ops, func(op string) bool { return strings.HasPrefix(op, `s ["tools"] `) }) {
			handover = ops
			break
		}
	}
	want := fmt.Sprintf(`s ["tools"] [{"callId":"g","entry":%d,"name":"ghost","status":"done"},{"callId":"b","name":"bad","status":"pending","taskId":%d}]`, ghost, bad)
	expectContains(t, "handover", handover, -1, want)
	// The fault cleanup writes only the status.
	if !slices.ContainsFunc(commits, func(ops []string) bool {
		return slices.Equal(ops, []string{`s ["tools",1,"status"] "done"`})
	}) {
		t.Fatalf("commits = %q, want the fault cleanup's status write", commits)
	}
}

func TestLiveCommitsTheToolCallingAnswerItsToolTasksTheGenerationsWaitAndTheToolRoundInOneCommit(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, &ToolRegistration{Tool: ai.Tool{Name: "noop", Description: "noop", Parameters: objectSchema()}, Execute: noOutput})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("noop", nil, "a"), call("noop", nil, "b"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	var mu sync.Mutex
	var kinds []string
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if kinds != nil {
			return
		}
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind {
				if slots, _ := document.Value["tools"].([]any); len(slots) == 2 {
					kinds = []string{}
					for _, other := range publication.Changes {
						switch c := other.(type) {
						case durable.EntryWrite:
							kinds = append(kinds, c.Value.Kind)
						case durable.TaskWrite:
							kinds = append(kinds, c.Value.Kind)
						}
					}
				}
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	waitFor(t, submit(t, root, inputDraft("go")))
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"pi.assistant", "pi.generation", "pi.tool", "pi.tool"}) {
		t.Fatalf("handover commit wrote %v", kinds)
	}
}

func TestLiveWritesAnAbortedToolsSlotWithFieldLevelOps(t *testing.T) {
	run := drive(t, nil)
	run.step(func(api ToolExecutionAPI) { must(t, api.Output([]byte("partial\n"), nil)) })
	run.step(func(api ToolExecutionAPI) { must(t, api.Details(bg, map[string]any{"n": 1})) })
	taskID := *liveSlots(t, run.h, run.root)[0].TaskID
	before := len(run.live.all())
	_, err := run.h.AbortTask(bg, taskID)
	must(t, err)
	isAbortCommit := func(ops []string) bool { return slices.Contains(ops, `s ["tools",0,"status"] "done"`) }
	var abortCommit []string
	eventually(t, func() bool {
		for _, ops := range run.live.all()[before:] {
			if isAbortCommit(ops) {
				abortCommit = ops
				return true
			}
		}
		return false
	})
	if len(abortCommit) != 4 || !slices.Contains(abortCommit, `d `+outputPath) || !slices.Contains(abortCommit, `d ["tools",0,"details"]`) ||
		!slices.ContainsFunc(abortCommit, func(op string) bool { return strings.HasPrefix(op, `s ["tools",0,"entry"] `) }) {
		t.Fatalf("abort commit = %q", abortCommit)
	}
	waitFor(t, run.done)
}
