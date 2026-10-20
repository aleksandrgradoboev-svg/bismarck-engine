package harness

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-conversations.test.ts.

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var noteDoc = durable.MustDefineDoc(durable.DocDef{
	CheckpointWhen: durable.NeverCheckpoint,
	Kind:           "test.note",
	Version:        1,
	Scope:          durable.ScopeConversation,
	History:        "rewindable",
	Fork:           "asOf",
	Initial:        func(any) map[string]any { return map[string]any{"text": ""} },
})

func appendMessage(t *testing.T, conversation *Conversation, text string) durable.EntryRecord {
	t.Helper()
	var entry durable.EntryRecord
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: "message", Model: []ai.Message{user(text)}})
		return err
	}))
	return entry
}

func messageText(entry durable.EntryRecord) string {
	if len(entry.Model) == 0 {
		return ""
	}
	if m, ok := entry.Model[0].(ai.UserMessage); ok && len(m.Content) == 1 {
		if text, ok := m.Content[0].(ai.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func allEntries(t *testing.T, conversation *Conversation, order durable.ScanOrder) []string {
	t.Helper()
	var texts []string
	var cursor durable.Cursor
	for {
		page, err := conversation.Entries(bg, durable.EntryQuery{Order: order}, 2, cursor)
		must(t, err)
		for _, entry := range page.Items {
			texts = append(texts, messageText(entry))
		}
		if page.Next == nil {
			return texts
		}
		cursor = page.Next
	}
}

func snapshotOf(t *testing.T, h *Harness, ref durable.DocRef) map[string]any {
	t.Helper()
	value, err := h.Snapshot(bg, ref)
	must(t, err)
	return value
}

func providerID(t *testing.T, h *Harness, id durable.ConversationID) string {
	t.Helper()
	value := snapshotOf(t, h, ProviderDoc.Conversation(id))
	sessionID, _ := value["sessionId"].(string)
	if !uuidV7.MatchString(sessionID) {
		t.Fatalf("provider session ID %q is not a UUIDv7", sessionID)
	}
	return sessionID
}

func expectValue(t *testing.T, got map[string]any, want string) {
	t.Helper()
	if g := jsonOf(t, got); g != want {
		t.Fatalf("value = %s, want %s", g, want)
	}
}

func expectError(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("err = %v, want one containing %q", err, message)
	}
}

func TestRootIsCreatedLazilyWithItsAgentChangeAndInitInOneCommit(t *testing.T) {
	storage := newRecordingStorage()
	h, _ := openHarness(t, storage, "read", "bash")
	if storage.commitCount() != 0 {
		t.Fatal("open committed")
	}
	high := AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}
	root, err := h.Root(bg, &high, func(tx *durable.Tx, id durable.ConversationID) error {
		// The agent change applied before init.
		agent, err := tx.Doc(AgentDoc.Conversation(id))
		if err != nil {
			return err
		}
		if level, _ := agent.Get("thinkingLevel"); level != "high" {
			t.Errorf("thinkingLevel in init = %v", level)
		}
		note, err := tx.Doc(noteDoc.Conversation(id))
		if err != nil {
			return err
		}
		return note.Set("text", "root note")
	})
	must(t, err)
	if root.ID != durable.RootConversationID || storage.commitCount() != 1 {
		t.Fatalf("root = %d, commits = %d", root.ID, storage.commitCount())
	}
	// Conversation, five built-in documents, and the init note.
	want := []string{"conversation", "document.create", "document.create", "document.create", "document.create", "document.create", "document.create"}
	if got := storage.commit(0); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	expectValue(t, snapshotOf(t, h, LiveDoc.Conversation(root.ID)), `{}`)
	providerID(t, h, root.ID)
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(root.ID)), `{"thinkingLevel":"high"}`)
	agent, err := root.Agent(bg)
	must(t, err)
	if agent.ThinkingLevel != "high" || !reflect.DeepEqual(toolNames(agent.Tools), []string{"read", "bash"}) {
		t.Fatalf("agent = %+v", agent)
	}
	expectValue(t, snapshotOf(t, h, noteDoc.Conversation(root.ID)), `{"text":"root note"}`)

	low := AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("low")}
	again, err := h.Root(bg, &low, func(*durable.Tx, durable.ConversationID) error {
		t.Fatal("init ran again")
		return nil
	})
	must(t, err)
	if again.ID != root.ID || storage.commitCount() != 1 {
		t.Fatal("a second Root wrote")
	}
}

func TestRootAndConversationsKeepIdentityAndStateAcrossReopen(t *testing.T) {
	storage := newRecordingStorage()
	first, _ := openHarness(t, storage, "read")
	root, err := first.Root(bg, nil, nil)
	must(t, err)
	must(t, root.Configure(bg, AgentChange{Model: Set(ModelRef{Provider: "anthropic", ModelID: "claude"}), Cwd: Set("/repo")}))
	entry := appendMessage(t, root, "hello")
	child, err := first.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	fork, err := root.Fork(bg, entry.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	ids := []durable.ConversationID{root.ID, child.ID, fork.ID}
	var providers []string
	for _, id := range ids {
		providers = append(providers, providerID(t, first, id))
	}
	// Regression coverage for pi #10424: a fork must not inherit its parent's
	// provider identity.
	if providers[0] == providers[1] || providers[0] == providers[2] || providers[1] == providers[2] {
		t.Fatalf("provider identities repeat: %v", providers)
	}
	must(t, first.Close(bg))
	if _, err := root.Agent(bg); err == nil {
		t.Fatal("a closed Harness resolved an agent")
	}

	// The new process installs nothing: the stored choices survive, the tools
	// do not resolve.
	second, _ := openHarness(t, storage)
	reopened, err := second.Root(bg, nil, func(*durable.Tx, durable.ConversationID) error {
		t.Fatal("init ran on reopen")
		return nil
	})
	must(t, err)
	agent, err := reopened.Agent(bg)
	must(t, err)
	if reopened.ID != durable.RootConversationID || *agent.Model != (ModelRef{Provider: "anthropic", ModelID: "claude"}) || *agent.Cwd != "/repo" || len(agent.Tools) != 0 {
		t.Fatalf("reopened agent = %+v", agent)
	}
	if got := allEntries(t, reopened, ""); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Fatalf("entries = %v", got)
	}
	if got, err := second.Conversation(bg, child.ID); err != nil || got == nil || got.ID != child.ID {
		t.Fatalf("child = %v, %v", got, err)
	}
	for i, id := range ids {
		if got := providerID(t, second, id); got != providers[i] {
			t.Fatalf("provider identity of %d changed across reopen", id)
		}
	}
	reopenedFork, err := second.Conversation(bg, fork.ID)
	must(t, err)
	if got := allEntries(t, reopenedFork, ""); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Fatalf("fork entries = %v", got)
	}
	forkAgent, err := reopenedFork.Agent(bg)
	must(t, err)
	if *forkAgent.Model != (ModelRef{Provider: "anthropic", ModelID: "claude"}) {
		t.Fatalf("fork model = %+v", forkAgent.Model)
	}
	if missing, err := second.Conversation(bg, 999); err != nil || missing != nil {
		t.Fatalf("conversation 999 = %v, %v", missing, err)
	}
}

func TestIndependentConversationsAreCreatedAtomicallyWithInit(t *testing.T) {
	storage := newRecordingStorage()
	h, registry := openHarness(t, storage, "read")
	created, err := h.CreateConversation(bg, ConversationCreateOptions{
		Ownership: durable.Ownerless,
		Init: func(tx *durable.Tx, id durable.ConversationID) error {
			_, err := tx.AppendEntry(id, durable.EntryDraft{Kind: "message", Model: []ai.Message{user("seed")}})
			return err
		},
	})
	must(t, err)
	if storage.commitCount() != 1 {
		t.Fatalf("commits = %d", storage.commitCount())
	}
	if got := allEntries(t, created, ""); !reflect.DeepEqual(got, []string{"seed"}) {
		t.Fatalf("entries = %v", got)
	}

	before := storage.commitCount()
	high := AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}
	_, err = h.CreateConversation(bg, ConversationCreateOptions{
		Ownership: durable.Ownerless,
		Agent:     &high,
		Init:      func(*durable.Tx, durable.ConversationID) error { return errors.New("init failed") },
	})
	expectError(t, err, "init failed")
	if storage.commitCount() != before {
		t.Fatal("a failed creation committed")
	}

	// A conversation on the default selection follows installs live.
	addTool(t, registry, testTool("bash"))
	agent, err := created.Agent(bg)
	must(t, err)
	if got := toolNames(agent.Tools); !reflect.DeepEqual(got, []string{"read", "bash"}) {
		t.Fatalf("tools = %v", got)
	}
}

func TestForksStartAtAnEntryWithTheAsOfAgentAndApplyOverrides(t *testing.T) {
	h, registry := openHarness(t, memory.New(), "read")
	legacy := testTool("legacy")
	addTool(t, registry, legacy)
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("low")}))
	at := appendMessage(t, root, "one")
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high"), Tools: Set(Exactly(testTool("read")))}))
	appendMessage(t, root, "two")

	child, err := root.Fork(bg, at.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(child.ID)), `{"thinkingLevel":"low"}`)
	if got := allEntries(t, child, ""); !reflect.DeepEqual(got, []string{"one"}) {
		t.Fatalf("fork entries = %v", got)
	}

	overrides := AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("minimal"), Tools: Set(Exactly(legacy))}
	overridden, err := root.Fork(bg, at.ID, ConversationCreateOptions{
		Ownership: durable.Ownerless,
		Agent:     &overrides,
		Init: func(tx *durable.Tx, id durable.ConversationID) error {
			agent, err := tx.Doc(AgentDoc.Conversation(id))
			if err != nil {
				return err
			}
			if level, _ := agent.Get("thinkingLevel"); level != "minimal" {
				t.Errorf("thinkingLevel in init = %v", level)
			}
			return nil
		},
	})
	must(t, err)
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(overridden.ID)), `{"thinkingLevel":"minimal","tools":["legacy"]}`)
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(root.ID)), `{"thinkingLevel":"high","tools":["read"]}`)

	unrelated, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	if _, err := root.Fork(bg, at.ID, ConversationCreateOptions{Ownership: durable.Ownerless}); err != nil {
		t.Fatal(err)
	}
	_, err = unrelated.Fork(bg, at.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	expectError(t, err, "is not visible")
}

func TestEntriesPaginateForkAwareHistoryThroughAncestorsAndSameCommitPrefixes(t *testing.T) {
	h, _ := openHarness(t, memory.New())
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	appendMessage(t, root, "r1")
	var r2 durable.EntryRecord
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if r2, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "message", Model: []ai.Message{user("r2")}}); err != nil {
			return err
		}
		_, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "message", Model: []ai.Message{user("r3")}})
		return err
	}))
	child, err := root.Fork(bg, r2.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	c1 := appendMessage(t, child, "c1")
	appendMessage(t, child, "c2")
	grandchild, err := child.Fork(bg, c1.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	appendMessage(t, grandchild, "g1")

	for _, each := range []struct {
		conversation *Conversation
		order        durable.ScanOrder
		want         []string
	}{
		{root, "", []string{"r3", "r2", "r1"}},
		{child, "", []string{"c2", "c1", "r2", "r1"}},
		{grandchild, "", []string{"g1", "c1", "r2", "r1"}},
		// pi #10546
		{grandchild, durable.ScanOrder("ascending"), []string{"r1", "r2", "c1", "g1"}},
	} {
		if got := allEntries(t, each.conversation, each.order); !reflect.DeepEqual(got, each.want) {
			t.Fatalf("entries of %d (%s) = %v, want %v", each.conversation.ID, each.order, got, each.want)
		}
	}
	// The handle's conversation wins over a query's.
	bounded, err := grandchild.Entries(bg, durable.EntryQuery{MinEntryID: &r2.ID, MaxEntryID: &c1.ID, ConversationID: root.ID}, 10, nil)
	must(t, err)
	if got := names(bounded.Items, func(e durable.EntryRecord) durable.EntryID { return e.ID }); !reflect.DeepEqual(got, []durable.EntryID{c1.ID, r2.ID}) {
		t.Fatalf("bounded = %v", got)
	}
	message := durable.MustDefineEntry("message")
	if !message.Is(&bounded.Items[0]) || message.Is(nil) {
		t.Fatal("entry kind checks")
	}
}

func TestConversationCommitsBindTaskCreationToTheConversation(t *testing.T) {
	h, _ := openHarness(t, memory.New())
	conversation, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	task := stubTask(t, "test.work", 1)
	var taskID durable.TaskID
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		var err error
		taskID, err = CreateTask(tx, task, map[string]int{"n": 1}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	record, err := h.GetTask(bg, taskID)
	must(t, err)
	if record == nil || record.ConversationID != conversation.ID {
		t.Fatalf("task = %+v", record)
	}
	err = h.Commit(bg, func(tx *durable.Tx) error {
		_, err := CreateTask(tx, task, map[string]int{"n": 2}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	})
	expectError(t, err, "requires options.ConversationID")
}

func TestConversationCreatedRunsInEveryCreatingCommitAfterBuiltInsAndBeforeAgentAndInit(t *testing.T) {
	var seen []string
	h, err := Open(bg, memory.New(), Options{
		Registry: NewRegistry(),
		ConversationCreated: func(tx *durable.Tx, conversation durable.ConversationRecord) error {
			agent, err := tx.Doc(AgentDoc.Conversation(conversation.ID))
			if err != nil {
				return err
			}
			cwd, _ := agent.Get("cwd")
			kind := "new"
			if conversation.Parent != nil {
				kind = "fork"
			}
			seen = append(seen, jsonOf(t, []any{conversation.ID, kind, cwd}))
			note, err := tx.Doc(noteDoc.Conversation(conversation.ID))
			if err != nil {
				return err
			}
			if text, _ := note.Get("text"); text == "" {
				if err := note.Set("text", "created"); err != nil {
					return err
				}
			}
			if cwd == "/fail" {
				return errors.New("no")
			}
			return nil
		},
	})
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	rootCwd := AgentChange{Cwd: Set("/root")}
	root, err := h.Root(bg, &rootCwd, func(tx *durable.Tx, id durable.ConversationID) error {
		note, err := tx.Doc(noteDoc.Conversation(id))
		if err != nil {
			return err
		}
		text, _ := note.Get("text")
		seen = append(seen, "init:"+text.(string))
		return nil
	})
	must(t, err)
	// A raw creation in a commit, as in a tool, and a fork, which already has
	// the asOf copies.
	raw, err := durable.CommitValue(bg, h.Session, func(tx *durable.Tx) (durable.ConversationID, error) {
		record, err := tx.CreateConversation(durable.Ownerless)
		return record.ID, err
	})
	must(t, err)
	entry := appendMessage(t, root, "hello")
	fork, err := root.Fork(bg, entry.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	want := []string{
		jsonOf(t, []any{root.ID, "new", nil}),
		"init:created",
		jsonOf(t, []any{raw, "new", nil}),
		jsonOf(t, []any{fork.ID, "fork", "/root"}),
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	expectValue(t, snapshotOf(t, h, noteDoc.Conversation(raw)), `{"text":"created"}`)
	// An error fails the creating commit.
	must(t, root.Configure(bg, AgentChange{Cwd: Set("/fail")}))
	if _, err := root.Fork(bg, entry.ID, ConversationCreateOptions{Ownership: durable.Ownerless}); err != nil {
		t.Fatal(err)
	}
	failing := appendMessage(t, root, "after")
	_, err = root.Fork(bg, failing.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	expectError(t, err, "no")
}

func TestConfigureReplacesWholeFieldsClearsThemAndLeavesUnsetFieldsAlone(t *testing.T) {
	h, registry := openHarness(t, memory.New())
	read, bash, edit := testTool("read"), testTool("bash"), testTool("edit")
	must(t, registry.Install(&Extension{Name: "coding", Tools: []*ToolRegistration{read, bash, edit}}))
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	stored := func() map[string]any { return snapshotOf(t, h, AgentDoc.Conversation(root.ID)) }
	offered := func() []string {
		agent, err := root.Agent(bg)
		must(t, err)
		return toolNames(agent.Tools)
	}
	agent, err := root.Agent(bg)
	must(t, err)
	if agent.ThinkingLevel != "off" || agent.Model != nil || !reflect.DeepEqual(agent.Tools, []*ToolRegistration{read, bash, edit}) {
		t.Fatalf("agent = %+v", agent)
	}

	must(t, root.Configure(bg, AgentChange{Model: Set(ModelRef{Provider: "openai", ModelID: "gpt"}), ThinkingLevel: Set[ai.ModelThinkingLevel]("medium")}))
	must(t, root.Configure(bg, AgentChange{Instructions: Set("Be terse.")}))
	expectValue(t, stored(), `{"instructions":"Be terse.","model":{"modelId":"gpt","provider":"openai"},"thinkingLevel":"medium"}`)
	must(t, root.Configure(bg, AgentChange{Model: Clear[ModelRef](), Instructions: Clear[string]()}))
	expectValue(t, stored(), `{"thinkingLevel":"medium"}`)

	check := func(change AgentChange, want ...string) {
		t.Helper()
		must(t, root.Configure(bg, change))
		if got := offered(); !reflect.DeepEqual(got, want) {
			t.Fatalf("offered = %v, want %v", got, want)
		}
	}
	check(AgentChange{Tools: Set(Edit(nil, []*ToolRegistration{edit}))}, "read", "bash")
	// A new filter replaces the old one: edit is offered again.
	check(AgentChange{Tools: Set(Edit(nil, []*ToolRegistration{bash}))}, "read", "edit")
	check(AgentChange{Tools: Set(Exactly(edit, read))}, "edit", "read")
	check(AgentChange{Tools: Clear[Selection[*ToolRegistration]]()}, "read", "bash", "edit")
	// Names are stored without checking the registry.
	check(AgentChange{Tools: Set(Exactly(testTool("missing"), read))}, "read")
	if got := stored()["tools"]; !reflect.DeepEqual(got, []any{"missing", "read"}) {
		t.Fatalf("stored tools = %v", got)
	}
	// A tools change cannot add.
	expectError(t, root.Configure(bg, AgentChange{Tools: Set(Edit([]*ToolRegistration{read}, nil))}), "cannot add")
}

func TestConversationsCreatedThroughTxGetTheirDocuments(t *testing.T) {
	h, _ := openHarness(t, memory.New(), "read")
	main := AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "m"}), Instructions: Set("Main role."), Cwd: Set("/repo")}
	root, err := h.Root(bg, &main, nil)
	must(t, err)
	owner := stubTask(t, "test.owner", 1)
	var taskID durable.TaskID
	var plain, owned durable.ConversationID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if taskID, err = CreateTask(tx, owner, struct{}{}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}}); err != nil {
			return err
		}
		p, err := tx.CreateConversation(durable.Ownerless)
		if err != nil {
			return err
		}
		o, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: taskID})
		if err != nil {
			return err
		}
		// The copy exists when CreateConversation returns, so a Configure in
		// the same callback overrides it.
		agent, err := tx.Doc(AgentDoc.Conversation(o.ID))
		if err != nil {
			return err
		}
		if cwd, _ := agent.Get("cwd"); cwd != "/repo" {
			t.Errorf("copied cwd = %v", cwd)
		}
		plain, owned = p.ID, o.ID
		return Configure(tx, o.ID, AgentChange{Cwd: Set("/worktree")})
	}))
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(plain)), `{}`)
	expectValue(t, snapshotOf(t, h, LiveDoc.Conversation(plain)), `{}`)
	providerID(t, h, plain)
	if providerID(t, h, owned) == providerID(t, h, root.ID) {
		t.Fatal("an owned conversation copied its owner's provider identity")
	}
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(owned)), `{"cwd":"/worktree","instructions":"Main role.","model":{"modelId":"m","provider":"faux"}}`)

	// A later owner change does not reach the child.
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}))
	if _, ok := snapshotOf(t, h, AgentDoc.Conversation(owned))["thinkingLevel"]; ok {
		t.Fatal("an owner change reached the child")
	}

	// A task-owned fork keeps its as-of copy of its fork parent, not its
	// owner's agent.
	var at durable.EntryID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		entry, err := tx.AppendEntry(plain, durable.EntryDraft{Kind: "note"})
		at = entry.ID
		return err
	}))
	fork, err := durable.CommitValue(bg, h.Session, func(tx *durable.Tx) (durable.ConversationID, error) {
		record, err := tx.ForkConversation(plain, at, durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: taskID})
		return record.ID, err
	})
	must(t, err)
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(fork)), `{}`)
	expectValue(t, snapshotOf(t, h, LiveDoc.Conversation(fork)), `{}`)
	if providerID(t, h, fork) == providerID(t, h, plain) {
		t.Fatal("a fork copied its parent's provider identity")
	}
}

func TestAnAbsentAgentReadsWithoutWritingForPlainSessionConversations(t *testing.T) {
	storage := newRecordingStorage()
	session := durable.NewSession(storage, durable.SessionOptions{})
	id, err := durable.CommitValue(bg, session, func(tx *durable.Tx) (durable.ConversationID, error) {
		record, err := tx.CreateConversation(durable.Ownerless)
		return record.ID, err
	})
	must(t, err)
	must(t, session.Close(bg))
	h, _ := openHarness(t, storage, "read")
	raw, err := h.Conversation(bg, id)
	must(t, err)
	if value := snapshotOf(t, h, LiveDoc.Conversation(id)); value != nil {
		t.Fatalf("live = %v", value)
	}
	commits := storage.commitCount()
	agent, err := raw.Agent(bg)
	must(t, err)
	if agent.ThinkingLevel != "off" || !reflect.DeepEqual(toolNames(agent.Tools), []string{"read"}) || storage.commitCount() != commits {
		t.Fatalf("agent = %+v, commits %d -> %d", agent, commits, storage.commitCount())
	}
	must(t, raw.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("low")}))
	expectValue(t, snapshotOf(t, h, AgentDoc.Conversation(id)), `{"thinkingLevel":"low"}`)
}

func TestHandlesAreStatelessAndOperationsFailAfterClose(t *testing.T) {
	h, err := Open(bg, memory.New(), Options{Registry: NewRegistry()})
	must(t, err)
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	again, err := h.Root(bg, nil, nil)
	must(t, err)
	if again == root || again.ID != root.ID {
		t.Fatal("handles are not stateless")
	}
	must(t, h.Close(bg))
	_, err = h.Root(bg, nil, nil)
	expectError(t, err, "is closed")
	_, err = h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	expectError(t, err, "is closed")
	_, err = h.Conversation(bg, root.ID)
	expectError(t, err, "is closed")
}

func TestTheHarnessForwardsTheSessionDocumentAPIs(t *testing.T) {
	h, _ := openHarness(t, memory.New())
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	var entry durable.EntryRecord
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		note, err := tx.Doc(noteDoc.Conversation(root.ID))
		if err != nil {
			return err
		}
		if err := note.Set("text", "first"); err != nil {
			return err
		}
		entry, err = tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "message", Model: []ai.Message{user("m")}})
		return err
	}))
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		note, err := tx.Doc(noteDoc.Conversation(root.ID))
		if err != nil {
			return err
		}
		return note.Set("text", "second")
	}))
	expectValue(t, snapshotOf(t, h, noteDoc.Conversation(root.ID)), `{"text":"second"}`)
	asOf, err := h.SnapshotAsOf(bg, noteDoc.Conversation(root.ID), entry.ID)
	must(t, err)
	expectValue(t, asOf, `{"text":"first"}`)
	state, err := h.DocumentState(bg, noteDoc.Conversation(root.ID))
	must(t, err)
	expectValue(t, state.Value(), `{"text":"second"}`)
	state.Dispose()
}
