package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-view.test.ts (snapshot
// d2e7d8b7).

var mountedKinds = []string{"pi.agent", "pi.inbox", "pi.live", "pi.provider", "pi.usage"}

type viewFrame struct {
	value map[string]any
	ops   []delta.Op
}

type recordedView struct {
	watch   *durable.DocumentWatch
	initial map[string]any
	mu      sync.Mutex
	frames  []viewFrame
}

// recordView starts a watch of conversation that records its acquisition
// revision and every delivered frame.
func recordView(t *testing.T, conversation *Conversation) *recordedView {
	t.Helper()
	watch, err := conversation.Watch(bg)
	must(t, err)
	r := &recordedView{watch: watch, initial: watch.Value()}
	must(t, watch.Start(func(_ context.Context, value map[string]any, ops []delta.Op) error {
		r.mu.Lock()
		r.frames = append(r.frames, viewFrame{value: value, ops: ops})
		r.mu.Unlock()
		return nil
	}))
	return r
}

func (r *recordedView) all() []viewFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames)
}

// settle waits until n frames arrived, then a little longer, so a frame too
// many would show (pi awaits setTimeout(0) for the watch callbacks).
func (r *recordedView) settle(t *testing.T, n int) []viewFrame {
	t.Helper()
	eventually(t, func() bool { return len(r.all()) >= n })
	flush()
	return r.all()
}

func decodeView(t *testing.T, value map[string]any) ConversationView {
	t.Helper()
	view, err := DecodeConversationView(value)
	must(t, err)
	return view
}

func viewKinds(t *testing.T, value map[string]any) []string {
	t.Helper()
	var kinds []string
	for _, entry := range decodeView(t, value).Entries {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}

// freshView is a freshly built view of conversation.
func freshView(t *testing.T, conversation *Conversation) map[string]any {
	t.Helper()
	state, err := conversation.ViewState(bg)
	must(t, err)
	value := state.Value()
	state.Dispose()
	return value
}

// committedView is the view as committed state defines it, read without any
// mount.
func committedView(t *testing.T, h *Harness, conversation *Conversation, record any) map[string]any {
	t.Helper()
	docs := map[string]any{}
	for _, doc := range mountedDocs {
		value, err := h.Snapshot(bg, doc.Conversation(conversation.ID))
		must(t, err)
		if value != nil {
			docs[doc.Definition().Kind] = value
		}
	}
	var entries []any
	for _, entry := range contextOf(t, conversation).Entries {
		value, err := toValue(entry)
		must(t, err)
		entries = append(entries, value)
	}
	if entries == nil {
		entries = []any{}
	}
	return map[string]any{"conversation": record, "entries": entries, "docs": docs}
}

func jsonOfValue(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	must(t, err)
	return string(raw)
}

func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	if g, w := jsonOfValue(t, got), jsonOfValue(t, want); g != w {
		t.Fatalf("%s =\n%s\nwant\n%s", what, g, w)
	}
}

// replayView applies every frame's operations from initial, checking each
// delivered revision on the way.
func replayView(t *testing.T, initial map[string]any, frames []viewFrame) map[string]any {
	t.Helper()
	value := initial
	for i, frame := range frames {
		next, err := delta.ApplyImmutable(value, frame.ops)
		must(t, err)
		sameJSON(t, "replayed frame "+string(rune('0'+i)), next, frame.value)
		value = next
	}
	return value
}

func opsJSON(t *testing.T, ops []delta.Op) string {
	t.Helper()
	var parts []string
	for _, op := range ops {
		parts = append(parts, opText(t, op))
	}
	return strings.Join(parts, " | ")
}

func opText(t *testing.T, op delta.Op) string {
	t.Helper()
	switch o := op.(type) {
	case delta.Set:
		return "s " + jsonOfValue(t, o.Path) + " " + jsonOfValue(t, o.Value)
	case delta.Delete:
		return "d " + jsonOfValue(t, o.Path)
	case delta.Replace:
		return "r " + jsonOfValue(t, o.Value)
	case delta.Splice:
		return "p " + jsonOfValue(t, o.Path) + " " + jsonOfValue(t, o.Index) + " " + jsonOfValue(t, o.Remove) + " " + jsonOfValue(t, o.Items)
	case delta.Append:
		return "a " + jsonOfValue(t, o.Path) + " " + jsonOfValue(t, o.Text)
	}
	return jsonOfValue(t, op)
}

// touchingCommits counts the commits that touch the conversation's entries or
// mounted documents.
func touchingCommits(t *testing.T, h *Harness, conversation *Conversation) func() int {
	t.Helper()
	var mu sync.Mutex
	count := 0
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		touched := false
		for _, change := range publication.Changes {
			switch c := change.(type) {
			case durable.EntryWrite:
				touched = touched || c.Value.ConversationID == conversation.ID
			case durable.DocumentChange:
				touched = touched || (c.ConversationID == conversation.ID && slices.Contains(mountedKinds, c.Record.Kind) && len(c.Ops) > 0)
			}
		}
		if touched {
			mu.Lock()
			count++
			mu.Unlock()
		}
	})
	must(t, err)
	t.Cleanup(unsubscribe)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

func noteOf(t *testing.T, conversation *Conversation, kind string, head *durable.EntryID) durable.EntryRecord {
	t.Helper()
	var appended durable.EntryRecord
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		var err error
		appended, err = tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: kind, Head: head})
		return err
	}))
	return appended
}

func pointerOf(v any) uintptr { return reflect.ValueOf(v).Pointer() }

func TestViewHydratesTheActiveEntriesAndTheBuiltInDocuments(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("hello")})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("hi")))
	value := freshView(t, root)
	view := decodeView(t, value)
	sameJSON(t, "conversation", view.Conversation, durable.ConversationRecord{ID: root.ID})
	sameJSON(t, "entries", view.Entries, chatEntries(t, root))
	var keys []string
	for key := range view.Docs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	expectStrings(t, "docs", keys, mountedKinds)
	sameJSON(t, "pi.live", view.Docs["pi.live"], map[string]any{})
	sameJSON(t, "pi.inbox", view.Docs["pi.inbox"], map[string]any{"items": []any{}})
}

func TestViewPublishesOneFramePerTouchingCommitWhoseOperationsRebuildEveryRevision(t *testing.T) {
	setup := newChatSetup(t)
	step, _, release := gatedStep(fauxAnswer("a longer answer"))
	setup.faux.SetResponses([]providers.FauxResponseStep{step})
	h, root := openChat(t, memory.New(), setup)
	recorded := recordView(t, root)
	touching := touchingCommits(t, h, root)
	submission := submit(t, root, inputDraft("hi"))
	eventually(t, func() bool {
		for _, frame := range recorded.all() {
			if live, _ := frame.value["docs"].(map[string]any)["pi.live"].(map[string]any); live != nil && live["generation"] != nil {
				return true
			}
		}
		return false
	})
	release()
	waitFor(t, submission)
	must(t, h.WaitForIdle(bg))
	frames := recorded.settle(t, touching())
	if len(frames) != touching() {
		t.Fatalf("frames = %d, touching commits = %d", len(frames), touching())
	}
	conversation := recorded.initial["conversation"]
	sameJSON(t, "replayed view", replayView(t, recorded.initial, frames), committedView(t, h, root, conversation))
	if first := opsJSON(t, frames[0].ops); !strings.Contains(first, `p ["entries"] 0 0 [{`) || !strings.Contains(first, `"kind":"pi.user"`) {
		t.Fatalf("first frame ops = %s, want the user entry appended", first)
	}
	// Document operations keep their exact shape under the mount path.
	var all []string
	for _, frame := range frames {
		for _, op := range frame.ops {
			all = append(all, opText(t, op))
		}
	}
	if !slices.Contains(all, `s ["docs","pi.live","generation"] {"attempt":1}`) {
		t.Fatalf("ops = %q, want the generation set under the mount", all)
	}
	recorded.watch.Stop()
}

func TestViewSharesUnchangedPartsBetweenRevisionsAndSkipsCommitsThatTouchNothingMounted(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	other, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	recorded := recordView(t, root)
	noteOf(t, other, "note", nil)
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Set(ai.ModelThinkingLevel("high"))}))
	noteOf(t, root, "note", nil)
	frames := recorded.settle(t, 2)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if got := opsJSON(t, frames[0].ops); got != `s ["docs","pi.agent","thinkingLevel"] "high"` {
		t.Fatalf("first frame ops = %s", got)
	}
	initialDocs := recorded.initial["docs"].(map[string]any)
	docs0 := frames[0].value["docs"].(map[string]any)
	if pointerOf(docs0["pi.live"]) != pointerOf(initialDocs["pi.live"]) {
		t.Fatal("an untouched document is not shared")
	}
	if pointerOf(frames[1].value["docs"]) != pointerOf(docs0) {
		t.Fatal("untouched docs are not shared by an entry-only frame")
	}
	recorded.watch.Stop()
}

func TestViewCutsTheEntriesAtAHeadMarkerKeepingTheEntriesFromItsHead(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	noteOf(t, root, "a", nil)
	b := noteOf(t, root, "b", nil)
	noteOf(t, root, "c", nil)
	recorded := recordView(t, root)
	summary := noteOf(t, root, "summary", &b.ID)
	noteOf(t, root, "d", nil)
	reset(t, root, nil)
	frames := recorded.settle(t, 3)
	var kinds [][]string
	for _, frame := range frames {
		kinds = append(kinds, viewKinds(t, frame.value))
	}
	equalValues(t, "kinds", kinds, [][]string{{"summary", "b", "c"}, {"summary", "b", "c", "d"}, {"pi.reset"}})
	summaryValue, err := toValue(summary)
	must(t, err)
	if got, want := opsJSON(t, frames[0].ops), `p ["entries"] 0 1 `+jsonOfValue(t, []any{summaryValue}); got != want {
		t.Fatalf("first frame ops = %s, want %s", got, want)
	}
	sameJSON(t, "replayed view", replayView(t, recorded.initial, frames), committedView(t, h, root, recorded.initial["conversation"]))
	recorded.watch.Stop()
}

func TestViewKeepsOnlyMountedEntriesForARawHeadWriteThatTargetsBeforeTheActiveRange(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	old := noteOf(t, root, "old", nil)
	reset(t, root, nil)
	recorded := recordView(t, root)
	noteOf(t, root, "summary", &old.ID)
	frames := recorded.settle(t, 1)
	// Model context now starts at old again, but the mount never held it
	// (spec §12); a rebuilt mount shows it.
	expectStrings(t, "mounted", viewKinds(t, frames[0].value), []string{"summary"})
	recorded.watch.Stop()
	expectStrings(t, "rebuilt", viewKinds(t, freshView(t, root)), []string{"summary", "old"})
}

func TestViewCutsAForksViewIntoItsInheritedEntries(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	a := noteOf(t, root, "a", nil)
	b := noteOf(t, root, "b", nil)
	fork, err := root.Fork(bg, b.ID, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	recorded := recordView(t, fork)
	noteOf(t, fork, "summary", &b.ID)
	frames := recorded.settle(t, 1)
	var ids []durable.EntryID
	for _, entry := range decodeView(t, recorded.initial).Entries {
		ids = append(ids, entry.ID)
	}
	equalValues(t, "inherited", ids, []durable.EntryID{a.ID, b.ID})
	expectStrings(t, "fork kinds", viewKinds(t, frames[0].value), []string{"summary", "b"})
	sameJSON(t, "fork view", frames[0].value, committedView(t, h, fork, recorded.initial["conversation"]))
	recorded.watch.Stop()
}

func TestViewShowsAForksInheritedEntriesAndFollowsOnlyTheForksOwnCommits(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	first := noteOf(t, root, "first", nil)
	fork, err := root.Fork(bg, first.ID, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	recorded := recordView(t, fork)
	expectStrings(t, "initial", viewKinds(t, recorded.initial), []string{"first"})
	if parent := decodeView(t, recorded.initial).Conversation.Parent; parent == nil || parent.ConversationID != root.ID || parent.At != first.ID {
		t.Fatalf("parent = %+v", parent)
	}
	noteOf(t, root, "parent", nil)
	noteOf(t, fork, "child", nil)
	frames := recorded.settle(t, 1)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	expectStrings(t, "fork kinds", viewKinds(t, frames[0].value), []string{"first", "child"})
	recorded.watch.Stop()
}

func TestViewUnmountsARetiredDocumentAndMountsItsRecreationWhole(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	recorded := recordView(t, root)
	must(t, root.Commit(bg, func(tx *durable.Tx) error { return tx.RetireDoc(LiveDoc.Conversation(root.ID)) }))
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		live, err := tx.Doc(LiveDoc.Conversation(root.ID))
		if err != nil {
			return err
		}
		return live.Set("tools", []any{})
	}))
	frames := recorded.settle(t, 2)
	var ops []string
	for _, frame := range frames {
		ops = append(ops, opsJSON(t, frame.ops))
	}
	expectStrings(t, "frame ops", ops, []string{`d ["docs","pi.live"]`, `s ["docs","pi.live"] {"tools":[]}`})
	if _, present := frames[0].value["docs"].(map[string]any)["pi.live"]; present {
		t.Fatal("the retired document is still mounted")
	}
	recorded.watch.Stop()
}

func TestViewReplacesUndeliveredFramesWithTheNewestViewAfter100PendingFrames(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	watch, err := root.Watch(bg)
	must(t, err)
	for range 101 {
		noteOf(t, root, "note", nil)
	}
	var mu sync.Mutex
	var frames []viewFrame
	must(t, watch.Start(func(_ context.Context, value map[string]any, ops []delta.Op) error {
		mu.Lock()
		frames = append(frames, viewFrame{value: value, ops: ops})
		mu.Unlock()
		return nil
	}))
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(frames) >= 1 })
	flush()
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if replace, ok := frames[0].ops[0].(delta.Replace); !ok || len(frames[0].ops) != 1 || jsonOfValue(t, replace.Value) != jsonOfValue(t, frames[0].value) {
		t.Fatalf("ops = %v, want one replacement by the newest view", frames[0].ops)
	}
	if got := len(frames[0].value["entries"].([]any)); got != 101 {
		t.Fatalf("entries = %d, want 101", got)
	}
	watch.Stop()
}

func TestViewKeepsStatesAndWatchesOfOneConversationIndependentAndRemountsAfterTheLastOneDetaches(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	state, err := root.ViewState(bg)
	must(t, err)
	recorded := recordView(t, root)
	noteOf(t, root, "one", nil)
	recorded.settle(t, 1)
	eventually(t, func() bool { return len(viewKinds(t, state.Value())) == 1 })
	expectStrings(t, "state", viewKinds(t, state.Value()), []string{"one"})
	recorded.watch.Stop()
	if got := len(recorded.all()); got != 1 {
		t.Fatalf("frames = %d, want 1", got)
	}
	noteOf(t, root, "two", nil)
	eventually(t, func() bool { return len(viewKinds(t, state.Value())) == 2 })
	expectStrings(t, "state", viewKinds(t, state.Value()), []string{"one", "two"})
	last := state.Value()
	state.Dispose()
	// No observer is left, so the mount was dropped: a new observer builds a
	// new revision from committed state.
	rebuilt := freshView(t, root)
	sameJSON(t, "rebuilt", rebuilt, last)
	if pointerOf(rebuilt) == pointerOf(last) {
		t.Fatal("the rebuilt view is the dropped mount's revision")
	}
}

func TestViewEndsStatesAndWatchesAtCloseAndRejectsLaterAcquisition(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	watch, err := root.Watch(bg)
	must(t, err)
	state, err := root.ViewState(bg)
	must(t, err)
	must(t, h.Close(bg))
	awaitSignal(t, watch.Closed(), "the watch end")
	if end := watch.End(); end.Reason != durable.WatchSessionClosed {
		t.Fatalf("watch end = %+v, want session_closed", end)
	}
	if entries := decodeView(t, state.Value()).Entries; len(entries) != 0 {
		t.Fatalf("state entries = %v", entries)
	}
	if _, err := root.Watch(bg); err == nil {
		t.Fatal("a watch after close was acquired")
	}
	if _, err := root.ViewState(bg); err == nil {
		t.Fatal("a state after close was acquired")
	}
}

// holdLine occupies the Session line with a commit that waits for release.
func holdCommitLine(t *testing.T, conversation *Conversation) (release func(), blocking <-chan result[struct{}]) {
	t.Helper()
	gate := make(chan struct{})
	entered := make(chan struct{})
	blocking = async(func() (struct{}, error) {
		return struct{}{}, conversation.Commit(bg, func(*durable.Tx) error {
			close(entered)
			<-gate
			return nil
		})
	})
	awaitSignal(t, entered, "the held commit")
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }, blocking
}

func TestViewRejectsAnAcquisitionCancelledOrClosedWhileItWaitsForTheSessionLine(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	release, blocking := holdCommitLine(t, root)
	ctx, cancel := context.WithCancelCause(bg)
	cancelled := async(func() (*durable.DocumentWatch, error) { return root.Watch(ctx) })
	parked(t, 1, "durable.(*Session).enqueue(", "harness.(*conversationViews).attach")
	cancel(errors.New("cancelled"))
	release()
	_, err := await(t, blocking)
	must(t, err)
	if _, err := await(t, cancelled); err == nil || err.Error() != "cancelled" {
		t.Fatalf("cancelled acquisition = %v, want cancelled", err)
	}

	release, blocking = holdCommitLine(t, root)
	queued := async(func() (*durable.DocumentWatch, error) { return root.Watch(bg) })
	parked(t, 1, "durable.(*Session).enqueue(", "harness.(*conversationViews).attach")
	closing := async(func() (struct{}, error) { return struct{}{}, h.Close(bg) })
	release()
	_, err = await(t, blocking)
	must(t, err)
	if _, err := await(t, queued); err == nil || !strings.Contains(err.Error(), "Harness is closed") {
		t.Fatalf("queued acquisition = %v, want Harness is closed", err)
	}
	_, err = await(t, closing)
	must(t, err)
}

func TestViewSharesOneMountBetweenConcurrentObserversAndIsolatesAFailingListener(t *testing.T) {
	_, root := openChat(t, memory.New(), newChatSetup(t))
	failing, err := root.Watch(bg)
	must(t, err)
	recorded := recordView(t, root)
	shared, err := root.ViewState(bg)
	must(t, err)
	if pointerOf(failing.Value()) != pointerOf(shared.Value()) {
		t.Fatal("concurrent observers do not share the mount's revision")
	}
	shared.Dispose()
	must(t, failing.Start(func(context.Context, map[string]any, []delta.Op) error { return errors.New("listener failed") }))
	noteOf(t, root, "one", nil)
	noteOf(t, root, "two", nil)
	awaitSignal(t, failing.Closed(), "the failing watch end")
	if end := failing.End(); end.Reason != durable.WatchListenerError {
		t.Fatalf("failing end = %+v, want listener_error", end)
	}
	if frames := recorded.settle(t, 2); len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	recorded.watch.Stop()
}

func TestViewPublishesOneFrameForACommitThatAppendsSeveralEntriesAndEditsADocument(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	other := durable.MustDefineDoc(durable.DocDef{Kind: "app.other", Version: 1, Scope: durable.ScopeConversation, History: "latest", Fork: "initial", Initial: func(any) map[string]any { return map[string]any{"n": float64(0)} }})
	recorded := recordView(t, root)
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		if _, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "a"}); err != nil {
			return err
		}
		live, err := tx.Doc(LiveDoc.Conversation(root.ID))
		if err != nil {
			return err
		}
		if err := live.Set("tools", []any{}); err != nil {
			return err
		}
		_, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "b"})
		return err
	}))
	// A document that is not mounted publishes nothing.
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		doc, err := tx.Doc(other.Conversation(root.ID))
		if err != nil {
			return err
		}
		return doc.Set("n", float64(1))
	}))
	frames := recorded.settle(t, 1)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	expectStrings(t, "kinds", viewKinds(t, frames[0].value), []string{"a", "b"})
	sameJSON(t, "replayed view", replayView(t, recorded.initial, frames), committedView(t, h, root, recorded.initial["conversation"]))
	recorded.watch.Stop()
}

// Ported from pi packages/durable/test/harness-live-deltas.test.ts: the two
// cases without tools; the others drive a tool and wait for slice 2.

func TestLiveStreamsPartialTextAsAppends(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 400, MinTokenSize: 4, MaxTokenSize: 4})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText(strings.Repeat("word ", 250))})
	h, root := openChat(t, memory.New(), setup)
	var mu sync.Mutex
	var commits [][]delta.Op
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && len(document.Ops) > 0 {
				mu.Lock()
				commits = append(commits, slices.Clone(document.Ops))
				mu.Unlock()
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	waitFor(t, submit(t, root, inputDraft("go")))
	mu.Lock()
	defer mu.Unlock()
	if len(commits) < 4 {
		t.Fatalf("pi.live commits = %d", len(commits))
	}
	partials := commits[2 : len(commits)-1]
	if len(partials) <= 2 {
		t.Fatalf("partials = %d, want more than 2", len(partials))
	}
	if got := opsJSON(t, partials[0]); !strings.HasPrefix(got, `s ["generation","message"] {`) || len(partials[0]) != 1 {
		t.Fatalf("first partial = %s, want the message set", got)
	}
	for _, ops := range partials[1:] {
		if len(ops) != 1 || !strings.HasPrefix(opText(t, ops[0]), `a ["generation","message","content",0,"text"] "`) {
			t.Fatalf("partial = %s, want one text append", opsJSON(t, ops))
		}
	}
}

func TestLiveKeepsACompleteBaseExactlyWhileNothingRuns(t *testing.T) {
	base := func(value map[string]any) bool {
		return LiveDoc.Definition().CheckpointWhen(value, nil, durable.CheckpointInfo{DeltasSinceBase: 1000})
	}
	slot := func(status string) any { return map[string]any{"callId": "c", "name": "n", "status": status} }
	run := map[string]any{"taskId": float64(1), "inputs": []any{}}
	cases := []struct {
		value map[string]any
		want  bool
	}{
		{map[string]any{}, true},
		{map[string]any{"run": run, "generation": map[string]any{"attempt": float64(1)}}, false},
		{map[string]any{"run": run, "tools": []any{slot("pending"), slot("done")}}, true},
		{map[string]any{"run": run, "tools": []any{slot("done"), slot("running")}}, false},
		{map[string]any{"run": run}, true},
	}
	for i, c := range cases {
		if got := base(c.value); got != c.want {
			t.Fatalf("case %d: base = %v, want %v", i, got, c.want)
		}
	}
}
