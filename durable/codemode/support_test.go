package codemode

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/jsonl"
)

// Test support of pi packages/durable/test/codemode.test.ts.

var bg = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	chattest.Must(t, err)
}

// signal is a one-shot event, pi's deferred().
type signal struct {
	once sync.Once
	ch   chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }
func (s *signal) fire()  { s.once.Do(func() { close(s.ch) }) }

func (s *signal) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was not reached", what)
	}
}

// countingSandbox counts the VMs it starts and shows each run to onStart.
type countingSandbox struct {
	*sandbox.Sandbox
	starts  atomic.Int32
	onStart func(*sandbox.Run)
}

func (s *countingSandbox) Start(ctx context.Context, code string, options sandbox.RunOptions) (*sandbox.Run, error) {
	s.starts.Add(1)
	run, err := s.Sandbox.Start(ctx, code, options)
	if err == nil && s.onStart != nil {
		s.onStart(run)
	}
	return run, err
}

func testParameters() *ai.Schema {
	return ai.Object(ai.Opt("text", ai.String("")), ai.Opt("id", ai.Number("")))
}

func testTool(name string, replay string, execute func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error)) *harness.ToolRegistration {
	return &harness.ToolRegistration{Tool: ai.Tool{Name: name, Description: name, Parameters: testParameters()}, Replay: replay, Execute: execute}
}

func text(s string) harness.ToolExecutionResult {
	return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: s}}}
}

func idOf(args map[string]any) string {
	if id, ok := args["id"].(float64); ok {
		return jsonNumber(id)
	}
	return ""
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// blocker is pi's holds/pauses: while block is set, a call fires reached
// and waits for its cancellation.
type blocker struct {
	block   atomic.Bool
	mu      sync.Mutex
	reached *signal
	runs    atomic.Int32
}

func newBlocker() *blocker {
	b := &blocker{reached: newSignal()}
	b.block.Store(true)
	return b
}

func (b *blocker) signal() *signal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reached
}

// reset makes reached fire again at the next blocked call.
func (b *blocker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reached = newSignal()
}

// hold blocks while block is set; otherwise it returns.
func (b *blocker) hold(ctx context.Context) error {
	b.runs.Add(1)
	if b.block.Load() {
		b.signal().fire()
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return nil
}

// slows are calls that return once released, by id.
type slows struct {
	mu       sync.Mutex
	gates    map[int]*signal
	done     map[int]*signal
	finished []int
}

func (s *slows) of(m map[int]*signal, id int) *signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m[id] == nil {
		m[id] = newSignal()
	}
	return m[id]
}

func (s *slows) release(id int)                      { s.of(s.gates, id).fire() }
func (s *slows) finishedCall(id int) <-chan struct{} { return s.of(s.done, id).ch }

// testChat is a chat with the code mode tool and test tools: echo returns
// "echo <text>" and counts its runs by text; hold (replay-safe) waits while
// holds.block is set, then returns "held <id>"; pause is hold without an id;
// slow returns "slow <id>" once released.
type testChat struct {
	chat    *chattest.Chat
	sandbox *countingSandbox
	echoMu  sync.Mutex
	echo    map[string]int
	holds   *blocker
	pauses  *blocker
	slows   *slows
}

func (c *testChat) echoes() map[string]int {
	c.echoMu.Lock()
	defer c.echoMu.Unlock()
	out := map[string]int{}
	for k, v := range c.echo {
		out[k] = v
	}
	return out
}

func newTestChat(t *testing.T, globals []Global, namespaces []Namespace) *testChat {
	t.Helper()
	s, err := sandbox.New(sandbox.Options{MemoryLimitBytes: 64 * 1024 * 1024})
	must(t, err)
	t.Cleanup(s.Close)
	c := &testChat{
		chat:    chattest.New(t),
		sandbox: &countingSandbox{Sandbox: s},
		echo:    map[string]int{},
		holds:   newBlocker(),
		pauses:  newBlocker(),
		slows:   &slows{gates: map[int]*signal{}, done: map[int]*signal{}},
	}
	c.chat.Install(t, "tool:codemode", NewTool(Options{Sandbox: c.sandbox, Globals: globals, Namespaces: namespaces}))
	c.chat.Install(t, "tool:echo", testTool("echo", harness.ReplayUnsafe, func(_ context.Context, args map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		value, _ := args["text"].(string)
		c.echoMu.Lock()
		c.echo[value]++
		c.echoMu.Unlock()
		return text("echo " + value), nil
	}))
	c.chat.Install(t, "tool:hold", testTool("hold", harness.ReplaySafe, func(ctx context.Context, args map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		if err := c.holds.hold(ctx); err != nil {
			return harness.ToolExecutionResult{}, err
		}
		return text("held " + idOf(args)), nil
	}))
	c.chat.Install(t, "tool:pause", testTool("pause", harness.ReplaySafe, func(ctx context.Context, _ map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		if err := c.pauses.hold(ctx); err != nil {
			return harness.ToolExecutionResult{}, err
		}
		return text("paused"), nil
	}))
	c.chat.Install(t, "tool:slow", testTool("slow", harness.ReplayUnsafe, func(ctx context.Context, args map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		id, _ := args["id"].(float64)
		select {
		case <-c.slows.of(c.slows.gates, int(id)).ch:
		case <-ctx.Done():
			return harness.ToolExecutionResult{}, context.Cause(ctx)
		}
		c.slows.mu.Lock()
		c.slows.finished = append(c.slows.finished, int(id))
		c.slows.mu.Unlock()
		c.slows.of(c.slows.done, int(id)).fire()
		return text("slow " + idOf(args)), nil
	}))
	return c
}

func script(code string) providers.FauxResponseStep {
	return chattest.ToolCalls(chattest.Call{Name: ToolName, Args: map[string]any{"code": code}, ID: "s1"})
}

// openJSONL opens JSONL storage over dir, as a fresh process would.
func openJSONL(t *testing.T, dir string) durable.Storage {
	t.Helper()
	storage, err := jsonl.OpenDir(bg, dir, jsonl.Options{})
	must(t, err)
	return storage
}

func tasksOf(t *testing.T, h *harness.Harness) []durable.TaskRecord {
	t.Helper()
	var items []durable.TaskRecord
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		page, err := tx.ScanTasks(durable.TaskQuery{Kind: harness.ToolTask.Name()}, 1000, nil)
		items = page.Items
		return err
	}))
	return items
}

func inputKind(task durable.TaskRecord) string {
	var input harness.ToolTaskInput
	_ = json.Unmarshal(task.Input, &input)
	return input.Kind
}

// scriptTask is the code mode call's task.
func scriptTask(t *testing.T, h *harness.Harness) durable.TaskID {
	t.Helper()
	for _, task := range tasksOf(t, h) {
		if inputKind(task) == "model" {
			return task.ID
		}
	}
	t.Fatal("no code mode task")
	return 0
}

func nestedCount(t *testing.T, h *harness.Harness) int {
	t.Helper()
	n := 0
	for _, task := range tasksOf(t, h) {
		if inputKind(task) == "nested" {
			n++
		}
	}
	return n
}

var header = regexp.MustCompile(`^Script (completed|failed)\nWall time [\d.]+ seconds\nOutput:\n`)

type scriptResult struct {
	text    string
	isError bool
}

// resultOf is the code mode call's result text, without the header.
func resultOf(t *testing.T, root *harness.Conversation) scriptResult {
	t.Helper()
	entries := chattest.ToolResults(chattest.Entries(t, root))
	if len(entries) == 0 {
		t.Fatal("no tool result")
	}
	message := chattest.ResultMessage(entries[0])
	var parts []string
	for _, item := range message.Content {
		if text, ok := item.(ai.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return scriptResult{text: header.ReplaceAllString(strings.Join(parts, ""), ""), isError: message.IsError}
}

// eventually waits until check holds.
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// journalOf is the call's journal snapshot.
func journalOf(t *testing.T, h *harness.Harness, task durable.TaskID) map[string]any {
	t.Helper()
	value, err := h.Snapshot(bg, JournalDoc.Task(task))
	must(t, err)
	return value
}

// issuedAfter is entry i's issuedAfter, -1 when absent.
func issuedAfter(t *testing.T, h *harness.Harness, task durable.TaskID, i int) int {
	t.Helper()
	entries, _ := journalOf(t, h, task)["entries"].([]any)
	if i >= len(entries) {
		return -1
	}
	entry, _ := entries[i].(map[string]any)
	value, ok := entry["issuedAfter"].(float64)
	if !ok {
		return -1
	}
	return int(value)
}

type restarted struct {
	h      *harness.Harness
	root   *harness.Conversation
	result scriptResult
}

// restart runs code until holds.reached, closes the Harness (a restart),
// lets between change the stored state, then reopens with hold no longer
// blocking and waits for the answer.
func restart(t *testing.T, c *testChat, code string, between, beforeClose func(h *harness.Harness, task durable.TaskID)) restarted {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(code), chattest.Done})
	h, root := c.chat.Open(t, openJSONL(t, dir), nil)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	id := submission.ID
	c.holds.signal().wait(t, "hold")
	if beforeClose != nil {
		beforeClose(h, scriptTask(t, h))
	}
	must(t, h.Close(bg))
	c.holds.block.Store(false)
	h, root = c.chat.Open(t, openJSONL(t, dir), nil)
	if between != nil {
		between(h, scriptTask(t, h))
	}
	must(t, h.Resume())
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	return restarted{h: h, root: root, result: resultOf(t, root)}
}

// edit changes a document in one commit.
func edit(t *testing.T, h *harness.Harness, ref durable.DocRef, change func(draft *delta.Draft) error) {
	t.Helper()
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		draft, err := tx.Doc(ref)
		if err != nil {
			return err
		}
		return change(draft)
	}))
}

// Short names for Global.Execute's parameter types in test globals.
type (
	ctxT = context.Context
	rawT = json.RawMessage
)
