package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Cases beyond pi's tests: each covers a path no pi case reaches.

func TestCodemodeRecordsTheVMsFingerprintBeforeTheScriptsFirstCall(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.hold({}); return "ok";`, nil, func(h *harness.Harness, task durable.TaskID) {
		eventually(t, func() bool {
			run, err := h.Snapshot(bg, RunDoc.Task(task))
			must(t, err)
			start, _ := run["start"].(map[string]any)
			fingerprint, _ := start["fingerprint"].(string)
			return fingerprint != ""
		})
	})
	expectResult(t, r.result, "ok", false)
}

func TestCodemodeEndsTheCallWhenAResumedScriptCallsAGlobalWhereTheJournalHasAToolCall(t *testing.T) {
	c := newTestChat(t, []Global{constantGlobal("pick", 1)}, nil)
	r := restart(t, c, `if (load("k")) await pick(); else await tools.echo({ text: "a" }); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, nil)
	expectError(t, r.result, "call 1 is pick(), but was a tool call")
}

// holdingStorage holds the first commit whose writes contain marker until
// released. A test registers t.Cleanup(storage.open) after opening the chat:
// cleanups run last first, so a failed test releases the commit before the
// chat's Close waits for it.
type holdingStorage struct {
	durable.Storage
	marker  []byte
	once    sync.Once
	reached *signal
	release chan struct{}
	opened  sync.Once
}

func newHoldingStorage(marker string) *holdingStorage {
	return &holdingStorage{Storage: memory.New(), marker: []byte(marker), reached: newSignal(), release: make(chan struct{})}
}

func (s *holdingStorage) open() { s.opened.Do(func() { close(s.release) }) }

func (s *holdingStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	raw, _ := json.Marshal(writes)
	if bytes.Contains(raw, s.marker) {
		held := false
		s.once.Do(func() { held = true })
		if held {
			s.reached.fire()
			select {
			case <-s.release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
	}
	return s.Storage.Commit(ctx, writes)
}

// TestCodemodeLaunchesACallOnlyOnceTheCountOfCallsIssuedBeforeItIsCommitted
// checks that a call made after a result does not run while that count's
// commit (a journal entry's issuedAfter) is held. Call 2 is a global: it runs
// in this process without a commit of its own, so the session's FIFO commit
// line, which would put a tool call's admission behind the count, does not
// hide a launch that skips the wait.
func TestCodemodeLaunchesACallOnlyOnceTheCountOfCallsIssuedBeforeItIsCommitted(t *testing.T) {
	var ran atomic.Int32
	counting := Global{Name: "counting", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) {
		ran.Add(1)
		return 1, nil
	}}
	c := newTestChat(t, []Global{counting}, nil)
	c.holds.block.Store(false)
	storage := newHoldingStorage("issuedAfter")
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`await tools.echo({ text: "a" }); await counting(); return 1;`), chattest.Done})
	_, root := c.chat.Open(t, storage, nil)
	t.Cleanup(storage.open)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	// After result 1 the script makes call 2; its count is being committed.
	storage.reached.wait(t, "the count commit")
	time.Sleep(100 * time.Millisecond)
	if n := ran.Load(); n != 0 {
		t.Fatalf("call 2 ran %d times before its count was committed", n)
	}
	storage.open()
	_, err = submission.Wait(bg)
	must(t, err)
	expectResult(t, resultOf(t, root), "1", false)
	if n := ran.Load(); n != 1 {
		t.Fatalf("call 2 ran %d times", n)
	}
}

func TestCodemodeRejectsASearchToolsLimitThatIsNotAPositiveInteger(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `const errors = [];
		for (const limit of [1.5, 0, -1]) { try { await searchTools("echo", { limit }); } catch (e) { errors.push(e.message); } }
		return errors;`)
	want := `["searchTools() limit must be a positive integer","searchTools() limit must be a positive integer","searchTools() limit must be a positive integer"]`
	if !strings.Contains(r.text, want) {
		t.Fatalf("text = %q", r.text)
	}
}

func TestCodemodeNumbersTextItemsAsSoonAsThereAreTwo(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `text("a"); text("b");`)
	if !strings.Contains(r.text, "==> text 1/2 <==\na\n==> text 2/2 <==\nb") {
		t.Fatalf("text = %q", r.text)
	}
}

func TestCodemodeSavesAnImageShownTwiceOnce(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `image("data:image/png;base64,`+onePixelPNG+`"); image("data:image/png;base64,`+onePixelPNG+`");`)
	saved := regexp.MustCompile(`\[Image saved to (\S+) \(image/png, \d+ bytes\)\]`).FindAllStringSubmatch(r.text, -1)
	if len(saved) != 2 || saved[0][1] != saved[1][1] {
		t.Fatalf("labels = %v", saved)
	}
	files, err := os.ReadDir(filepath.Dir(saved[0][1]))
	must(t, err)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
}

func TestCodemodeSaysWhenToolsWithoutANamespaceDoNotFitTheBudget(t *testing.T) {
	var tools []*harness.ToolRegistration
	for i := range 20 {
		tools = append(tools, describeTool("tool"+jsonNumber(float64(i)), strings.Repeat("x", 200)))
	}
	text, _ := section(tools, nil, 200)
	if !strings.Contains(text, "Some tools are not listed: find them with `searchTools()`.") {
		t.Fatalf("section = %q", text)
	}
	if text, _ := section(tools[:1], nil, 3000); strings.Contains(text, "Some tools are not listed") {
		t.Fatalf("section of a tool that fits = %q", text)
	}
}
