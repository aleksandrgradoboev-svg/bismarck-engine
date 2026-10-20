package harness

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"regexp"
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

// Ported from pi packages/durable/test/harness-generation.test.ts (the run
// cases; the retry and deferred cases are in generation_test.go).

func scanConversationTasks(t *testing.T, h *Harness, conversation *Conversation) []durable.TaskRecord {
	t.Helper()
	var tasks durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		tasks, err = tx.ScanTasks(durable.TaskQuery{ConversationID: &conversation.ID}, 10, nil)
		return err
	}))
	return tasks.Items
}

func providerSessionID(t *testing.T, h *Harness, conversation *Conversation) string {
	t.Helper()
	value, err := h.Snapshot(bg, ProviderDoc.Conversation(conversation.ID))
	must(t, err)
	id, _ := value["sessionId"].(string)
	return id
}

// contentText joins the text blocks of content.
func contentText(content ai.ContentList) string {
	var out strings.Builder
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	return out.String()
}

// sectionMap is a system message's sections by name; a removal is "<nil>".
func sectionMap(sections ai.SystemSections) map[string]string {
	out := map[string]string{}
	for _, section := range sections {
		if section.Value == nil {
			out[section.Name] = "<nil>"
		} else {
			out[section.Name] = *section.Value
		}
	}
	return out
}

// userText is the text of the newest user message of a request.
func userText(messages []ai.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if user, ok := messages[i].(ai.UserMessage); ok {
			return textOf(durable.EntryRecord{Model: []ai.Message{user}})
		}
	}
	return ""
}

func TestGenerationAnswersAnInputAndSettlesItsSubmission(t *testing.T) {
	setup := newChatSetup(t)
	preamble := constSection("preamble", "You are helpful.")
	preamble.Untagged = true
	addSection(t, setup.registry, preamble)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("Hello there")})
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	if settled.Status != durable.SubmissionDone || settled.Answer == nil || settled.Entry == nil {
		t.Fatalf("settled = %+v, want done with an answer", settled)
	}
	if text := textOf(entryByID(t, root, *settled.Answer)); text != "Hello there" {
		t.Fatalf("answer = %q", text)
	}
	entries := expectKinds(t, root, "pi.user", "pi.system", "pi.assistant")
	if entries[0].ID != *settled.Entry {
		t.Fatalf("user entry = %d, want the settled entry %d", entries[0].ID, *settled.Entry)
	}
	system, ok := entries[1].Model[0].(ai.SystemMessage)
	if !ok || contentText(system.Content) != "" || !reflect.DeepEqual(sectionMap(system.Sections), map[string]string{"preamble": "You are helpful."}) || system.Timestamp == 0 {
		t.Fatalf("system entry = %+v", entries[1].Model[0])
	}
	expectIdleLive(t, h, root)
	tasks := scanConversationTasks(t, h, root)
	if len(tasks) != 1 || tasks[0].Kind != "pi.generation" {
		t.Fatalf("tasks = %+v, want one generation", tasks)
	}
	task := tasks[0]
	// Entries written by the generation are attributed to it; the admitted
	// user entry is not task work.
	byTask := names(entries, func(entry durable.EntryRecord) *durable.TaskID { return entry.ByTaskID })
	if byTask[0] != nil || byTask[1] == nil || *byTask[1] != task.ID || byTask[2] == nil || *byTask[2] != task.ID {
		t.Fatalf("byTaskId = %v, want [nil %d %d]", byTask, task.ID, task.ID)
	}
	if task.State.Status != durable.TaskTerminal || task.State.Outcome == nil || task.State.Outcome.Status != durable.OutcomeCompleted ||
		string(task.State.Outcome.Result) != `{"entryId":`+jsonNumber(*settled.Answer)+`}` {
		t.Fatalf("task state = %+v, want completed with the answer", task.State)
	}
}

func jsonNumber[T ~int64](v T) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestGenerationStoresPartialsAsDeltasAndACompleteBaseOnceNothingIsInFlight(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 200, MinTokenSize: 1, MaxTokenSize: 1})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText(strings.Repeat("w", 200))})
	storage := newControlledStorage()
	h, root := openChat(t, storage, setup)
	var liveID durable.DocumentID
	var mu sync.Mutex
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.ConversationID == root.ID {
				mu.Lock()
				liveID = document.Record.ID
				mu.Unlock()
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	h.Resume()
	waitFor(t, submit(t, root, inputDraft("hi")))
	mu.Lock()
	id := liveID
	mu.Unlock()
	storage.mu.Lock()
	var contents []string
	for _, writes := range storage.commits {
		for _, write := range writes {
			if change, ok := write.(durable.DocumentChangeWrite); ok && change.ID == id {
				contents = append(contents, string(change.Content.Kind))
			}
		}
	}
	storage.mu.Unlock()
	// Streaming writes deltas; the commit that settles the answer clears
	// generation and writes a base.
	if !slices.Contains(contents, string(durable.ContentDelta)) || len(contents) == 0 || contents[len(contents)-1] != string(durable.ContentBase) {
		t.Fatalf("pi.live contents = %v, want deltas then a base", contents)
	}
}

func TestGenerationStillEndsARunWhoseInputSomethingElseAlreadySettled(t *testing.T) {
	setup := newChatSetup(t)
	step, reached := unanswered()
	setup.faux.SetResponses([]providers.FauxResponseStep{step})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("hi"))
	awaitSignal(t, reached, "the request")
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		return tx.SettleSubmission(submission.ID, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "withdrawn"})
	}))
	taskID := runTask(t, h, root)
	_, err := h.AbortTask(bg, taskID)
	must(t, err)
	record, err := h.WaitForTask(bg, taskID)
	must(t, err)
	if outcome := record.State.Outcome; outcome == nil || outcome.Status != durable.OutcomeAborted {
		t.Fatalf("outcome = %+v, want aborted", outcome)
	}
	// The earlier settlement stays; the run's own settlement leaves it
	// unchanged.
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "withdrawn")
	expectIdleLive(t, h, root)
}

func TestGenerationFailsWithNoModelWhenNoModelIsConfiguredOrTheModelIsUnknown(t *testing.T) {
	setup := newChatSetup(t)
	h, root := openChat(t, memory.New(), setup)
	plain, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}})
	must(t, err)
	h.Resume()
	expectSettled(t, waitFor(t, submit(t, plain, inputDraft("hi"))), durable.SubmissionUnanswered, "no_model")

	must(t, root.Configure(bg, AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "missing"})}))
	unknown := waitFor(t, submit(t, root, inputDraft("hi")))
	expectSettled(t, unknown, durable.SubmissionUnanswered, "no_model")
	if unknown.Entry == nil {
		t.Fatalf("settled = %+v, want its entry", unknown)
	}
	expectKinds(t, root, "pi.user")
	tasks := scanConversationTasks(t, h, root)
	outcome := tasks[0].State.Outcome
	if tasks[0].State.Status != durable.TaskTerminal || outcome == nil || outcome.Status != durable.OutcomeFailed ||
		outcome.Error.Message != "Model faux/missing is not available" || string(outcome.Error.Detail) != `{"reason":"no_model"}` {
		t.Fatalf("task state = %+v", tasks[0].State)
	}
	expectIdleLive(t, h, root)
	expectIdleLive(t, h, plain)
}

func TestGenerationReportsSectionWrapperFailuresWhilePreparing(t *testing.T) {
	setup := newChatSetup(t)
	addSection(t, setup.registry, constSection("cwd", "/repo"))
	must(t, setup.registry.Install(&Extension{Name: "broken", Wraps: []Wrap{
		WrapSection("cwd", func(*PromptSection) (*PromptSection, error) { return nil, errorString("wrapper failed") }),
	}}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("ok")})
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	if settled := waitFor(t, submit(t, root, inputDraft("hi"))); settled.Status != durable.SubmissionDone {
		t.Fatalf("settled = %+v, want done", settled)
	}
	if _, ok := setup.reports.infoOf("wrapper failed"); !ok {
		t.Fatalf("reports = %v, want wrapper failed", setup.reports.all())
	}
	// The failed section is absent, so nothing was rendered.
	expectKinds(t, root, "pi.user", "pi.assistant")
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestGenerationConvertsTheCommittedPartialWhenAbortedDuringStreaming(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{TokensPerSecond: 20, MinTokenSize: 1, MaxTokenSize: 1})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText(strings.Repeat("x", 400))})
	h, root := openChat(t, memory.New(), setup)
	h.Resume()
	submission := submit(t, root, inputDraft("hi"))
	taskID := runTask(t, h, root)
	var partial string
	eventually(t, func() bool {
		generation := decodeLive(t, liveOf(t, h, root)).Generation
		if generation == nil || generation.Message == nil {
			return false
		}
		partial = textOf(durable.EntryRecord{Model: []ai.Message{*generation.Message}})
		return partial != ""
	})
	marked, err := h.AbortTask(bg, taskID)
	must(t, err)
	if marked != "marked" {
		t.Fatalf("abort = %q, want marked", marked)
	}
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "aborted")
	entries := expectKinds(t, root, "pi.user", "pi.assistant")
	converted := entryAssistant(t, entries[1])
	if converted.StopReason != ai.StopAborted || !strings.HasPrefix(textOf(entries[1]), partial) {
		t.Fatalf("converted = %s %q, want aborted starting with %q", converted.StopReason, textOf(entries[1]), partial)
	}
	expectIdleLive(t, h, root)
	record, err := h.WaitForTask(bg, taskID)
	must(t, err)
	if outcome := record.State.Outcome; outcome == nil || outcome.Status != durable.OutcomeAborted {
		t.Fatalf("outcome = %+v, want aborted", outcome)
	}
}

func TestGenerationForwardsStreamOptionsAndTheThinkingLevel(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var seen []ai.SimpleStreamOptions
	capture := func(text string) providers.FauxResponseStep {
		return func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			mu.Lock()
			seen = append(seen, *opts)
			mu.Unlock()
			return fauxAnswer(text)
		}
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{capture("a"), capture("b")})
	setup.setSettings(func(s *HarnessSettings) {
		s.Stream = ConversationStreamOptions{TimeoutMs: ptr(1234), Headers: map[string]string{"x-test": "1"}}
	})
	h, root := openChat(t, memory.New(), setup)
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Set(ai.ModelThinkingLevel("high"))}))
	h.Resume()
	waitFor(t, submit(t, root, inputDraft("one")))
	// Both are read at the next preparation: the thinking level from
	// pi.agent, the stream options live from settings.
	must(t, root.Configure(bg, AgentChange{ThinkingLevel: Clear[ai.ModelThinkingLevel]()}))
	setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(99)} })
	waitFor(t, submit(t, root, inputDraft("two")))
	sessionID := providerSessionID(t, h, root)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(seen))
	}
	first, second := seen[0], seen[1]
	if first.TimeoutMs != 1234 || first.Headers["x-test"] == nil || *first.Headers["x-test"] != "1" || first.Reasoning != "high" || first.SessionID != sessionID {
		t.Fatalf("first options = %+v", first)
	}
	// pi's signal is the request context here, which the faux step receives.
	if second.Reasoning != "" || second.TimeoutMs != 99 || second.SessionID != sessionID || second.Headers != nil {
		t.Fatalf("second options = %+v", second)
	}
}

// Regression coverage for pi #10424.
func TestGenerationKeepsProviderSessionIDsRequestLocalAcrossConcurrentConversations(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	seen := map[string][]string{}
	capture := func(_ context.Context, req ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		text := userText(req.Messages)
		mu.Lock()
		seen[text] = append(seen[text], opts.SessionID)
		mu.Unlock()
		return fauxAnswer("answer:" + text)
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{capture, capture, capture, capture})
	h, root := openChat(t, memory.New(), setup)
	child, err := h.CreateConversation(bg, ConversationCreateOptions{
		Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless},
		Agent:     &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})},
	})
	must(t, err)
	h.Resume()
	for _, round := range []string{"first", "second"} {
		var wg sync.WaitGroup
		for index, conversation := range []*Conversation{root, child} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				submission, err := conversation.Submit(bg, inputDraft(round+"-"+string(rune('0'+index))))
				if err == nil {
					_, err = submission.Wait(bg)
				}
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	rootID, childID := providerSessionID(t, h, root), providerSessionID(t, h, child)
	if rootID == childID {
		t.Fatalf("session IDs are shared: %s", rootID)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{"first-0": {rootID}, "second-0": {rootID}, "first-1": {childID}, "second-1": {childID}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("session IDs per request = %v, want %v", seen, want)
	}
}

// Regression coverage for pi #10424.
func TestGenerationCreatesAndPersistsProviderStateBeforeALegacyConversationsRequest(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var sent string
	setup.faux.SetResponses([]providers.FauxResponseStep{
		func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			mu.Lock()
			sent = opts.SessionID
			mu.Unlock()
			return fauxAnswer("ok")
		},
	})
	h, root := openChat(t, memory.New(), setup)
	must(t, root.Commit(bg, func(tx *durable.Tx) error { return tx.RetireDoc(ProviderDoc.Conversation(root.ID)) }))
	if value, err := h.Snapshot(bg, ProviderDoc.Conversation(root.ID)); err != nil || value != nil {
		t.Fatalf("pi.provider after retirement = %v, %v; want absent", value, err)
	}
	h.Resume()
	waitFor(t, submit(t, root, inputDraft("legacy")))
	stored := providerSessionID(t, h, root)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(stored) {
		t.Fatalf("stored session ID = %q, want a UUIDv7", stored)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent != stored {
		t.Fatalf("sent %q, stored %q", sent, stored)
	}
}

func TestGenerationResolvesSettingsOverTheBuiltInDefaults(t *testing.T) {
	got := resolveSettings(HarnessSettings{})
	if got.Retry != (RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelay: 2 * time.Second, MaxAgentDelay: time.Minute}) ||
		got.Compaction != (CompactionPolicy{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000, BackgroundTokens: 32768}) ||
		got.Progress != (ProgressPolicy{PartialInterval: 100 * time.Millisecond, OutputInterval: 100 * time.Millisecond}) ||
		got.ToolExecution != ToolExecutionParallel || got.SteeringMode != QueueOneAtATime || got.FollowUpMode != QueueOneAtATime ||
		got.ContextRetention != 10*time.Minute || got.AfterFailedRun != AfterFailedRunKeep || got.RestartLimit != 5 ||
		!reflect.DeepEqual(got.Stream, ConversationStreamOptions{}) || got.Extensions != nil {
		t.Fatalf("defaults = %+v", got)
	}
	// A limit below one restart is pi's default; a negative one is pi's
	// Infinity here.
	if limit := resolveSettings(HarnessSettings{RestartLimit: ptr(0)}).RestartLimit; limit != 5 {
		t.Fatalf("restart limit 0 resolves to %d, want 5", limit)
	}
	if limit := resolveSettings(HarnessSettings{RestartLimit: ptr(-1)}).RestartLimit; limit >= 0 {
		t.Fatalf("restart limit -1 resolves to %d, want never", limit)
	}
	var partial HarnessSettings
	partial.Retry.Enabled = ptr(false)
	partial.Compaction.BackgroundTokens = ptr(0)
	resolved := resolveSettings(partial)
	if resolved.Retry.Enabled || resolved.Retry.MaxRetries != 3 || resolved.Retry.BaseDelay != 2*time.Second ||
		!resolved.Compaction.Enabled || resolved.Compaction.BackgroundTokens != 0 {
		t.Fatalf("partial = %+v %+v", resolved.Retry, resolved.Compaction)
	}
	var progress HarnessSettings
	progress.Progress.OutputInterval = ptr(500 * time.Millisecond)
	if got := resolveSettings(progress).Progress; got != (ProgressPolicy{PartialInterval: 100 * time.Millisecond, OutputInterval: 500 * time.Millisecond}) {
		t.Fatalf("progress = %+v", got)
	}
}

func TestGenerationCommitsPartialsNoMoreOftenThanTheProgressInterval(t *testing.T) {
	publishedPartial := func(interval *time.Duration) bool {
		setup := newChatSetup(t)
		// Longer than the default 100 ms, shorter than the configured
		// interval.
		setup.models = streamModels{Models: setup.models, stream: scriptedStream(fauxAnswer("partial"), fauxAnswer("final"), 300*time.Millisecond)}
		setup.setSettings(func(s *HarnessSettings) { s.Progress.PartialInterval = interval })
		h, root := openChat(t, memory.New(), setup)
		values := watchLive(t, h)
		h.Resume()
		if settled := waitFor(t, submit(t, root, inputDraft("hi"))); settled.Status != durable.SubmissionDone {
			t.Fatalf("settled = %+v, want done", settled)
		}
		must(t, h.Close(bg))
		return slices.ContainsFunc(values.generations(t), func(g LiveGeneration) bool {
			return g.Message != nil && textOf(durable.EntryRecord{Model: []ai.Message{*g.Message}}) == "partial"
		})
	}
	if !publishedPartial(nil) {
		t.Fatal("the default interval published no partial")
	}
	if publishedPartial(ptr(5 * time.Second)) {
		t.Fatal("a 5 s interval published the partial")
	}
}

var sectionAgentDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:    "test.agent",
	Version: 1,
	Scope:   durable.ScopeConversation,
	History: "latest",
	Fork:    "current",
	Initial: func(any) map[string]any { return map[string]any{"cwd": "/", "kind": "main"} },
})

func TestGenerationRendersSectionsThatReadConversationDocumentsThroughInputRead(t *testing.T) {
	setup := newChatSetup(t)
	read := func(ctx context.Context, input PromptInput, key string) (string, error) {
		value, err := input.Read.Snapshot(ctx, sectionAgentDoc.Conversation(input.ConversationID))
		if err != nil || value == nil {
			return "", err
		}
		text, _ := value[key].(string)
		return text, nil
	}
	addSection(t, setup.registry, Section("cwd", func(ctx context.Context, input PromptInput) (string, bool, error) {
		cwd, err := read(ctx, input, "cwd")
		return cwd, cwd != "", err
	}))
	addSection(t, setup.registry, Section("agents", func(ctx context.Context, input PromptInput) (string, bool, error) {
		kind, err := read(ctx, input, "kind")
		return "Read AGENTS.md", kind != "sub", err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("a"), fauxText("b")})
	h, root := openChat(t, memory.New(), setup)
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		doc, err := tx.Doc(sectionAgentDoc.Conversation(root.ID))
		if err != nil {
			return err
		}
		return doc.Set("cwd", "/repo")
	}))
	sub, err := h.CreateConversation(bg, ConversationCreateOptions{
		Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless},
		Agent:     &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})},
		Init: func(tx *durable.Tx, id durable.ConversationID) error {
			doc, err := tx.Doc(sectionAgentDoc.Conversation(id))
			if err != nil {
				return err
			}
			if err := doc.Set("kind", "sub"); err != nil {
				return err
			}
			return doc.Set("cwd", "/sub")
		},
	})
	must(t, err)
	h.Resume()
	waitFor(t, submit(t, root, inputDraft("one")))
	waitFor(t, submit(t, sub, inputDraft("two")))
	sections := func(conversation *Conversation) ai.SystemMessage {
		for _, entry := range chatEntries(t, conversation) {
			if entry.Kind == "pi.system" {
				system, _ := entry.Model[0].(ai.SystemMessage)
				return system
			}
		}
		t.Fatalf("conversation %d has no pi.system entry", conversation.ID)
		return ai.SystemMessage{}
	}
	if got := sectionMap(sections(root).Sections); !reflect.DeepEqual(got, map[string]string{"cwd": "<cwd>\n/repo\n</cwd>", "agents": "<agents>\nRead AGENTS.md\n</agents>"}) {
		t.Fatalf("root sections = %v", got)
	}
	if got := sections(sub); contentText(got.Content) != "" || got.Timestamp == 0 || !reflect.DeepEqual(sectionMap(got.Sections), map[string]string{"cwd": "<cwd>\n/sub\n</cwd>"}) {
		t.Fatalf("sub system message = %+v", got)
	}
}

func TestGenerationFaultsARunTaskSettlingItsInputsAndConvertingTheCommittedPartial(t *testing.T) {
	setup := newChatSetup(t)
	// The final message cannot be stored as JSON (pi: a function property),
	// so the classification commit fails and the scheduler faults the task.
	final := fauxAnswer("final")
	final.Usage.Cost.Total = math.NaN()
	setup.models = streamModels{Models: setup.models, stream: scriptedStream(fauxAnswer("partial"), final, 300*time.Millisecond)}
	h, root := openChat(t, memory.New(), setup)
	values := watchLive(t, h)
	h.Resume()
	settled := waitFor(t, submit(t, root, inputDraft("hi")))
	expectSettled(t, settled, durable.SubmissionUnanswered, "faulted")
	if detail := string(settled.Detail); !strings.Contains(detail, "NaN") {
		t.Fatalf("detail = %s, want the JSON failure", detail)
	}
	if !slices.ContainsFunc(values.generations(t), func(g LiveGeneration) bool {
		return g.Message != nil && textOf(durable.EntryRecord{Model: []ai.Message{*g.Message}}) == "partial"
	}) {
		t.Fatal("the partial was never published")
	}
	entries := expectKinds(t, root, "pi.user", "pi.assistant")
	if converted := entryAssistant(t, entries[1]); converted.StopReason != ai.StopAborted || textOf(entries[1]) != "partial" {
		t.Fatalf("converted = %s %q, want the aborted partial", converted.StopReason, textOf(entries[1]))
	}
	expectIdleLive(t, h, root)
	tasks := scanConversationTasks(t, h, root)
	if tasks[0].State.Status != durable.TaskTerminal || tasks[0].State.Outcome == nil || tasks[0].State.Outcome.Status != durable.OutcomeFaulted {
		t.Fatalf("task state = %+v, want faulted", tasks[0].State)
	}
}

func TestGenerationOrphansABlockedRunTaskWithFullRunCleanup(t *testing.T) {
	setup := newChatSetup(t)
	h, root := openChat(t, memory.New(), setup)
	// A run whose task was stored by a newer generation definition this
	// process cannot run.
	var taskID durable.TaskID
	var submissionID durable.SubmissionID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		entry, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.UserEntry.Kind, Model: []ai.Message{TextInput("hi").userMessage(1)}})
		if err != nil {
			return err
		}
		submission, err := tx.CreateSubmission(durable.SubmissionRecord{ConversationID: root.ID, Type: durable.SubmissionTypeInput, Status: durable.SubmissionPlaced, Entry: &entry.ID})
		if err != nil {
			return err
		}
		submissionID = submission.ID
		taskID, err = CreateTask[any](tx, versioned(t, "pi.generation", 2, nil, nil), nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		if err != nil {
			return err
		}
		live, err := liveDraft(tx, root.ID)
		if err != nil {
			return err
		}
		return setDraftField(live, "run", LiveRun{TaskID: taskID, Inputs: []durable.SubmissionID{submissionID}})
	}))
	h.Resume()
	draft := inputDraft("busy")
	draft.Input.WhenBusy = WhenBusyReject
	if _, err := root.Submit(bg, draft); err == nil || !strings.Contains(err.Error(), "is busy") {
		t.Fatalf("busy submit = %v, want is busy", err)
	}
	marked, err := h.AbortTask(bg, taskID)
	must(t, err)
	if marked != "marked" {
		t.Fatalf("abort = %q, want marked", marked)
	}
	record, err := h.WaitForTask(bg, taskID)
	must(t, err)
	if outcome := record.State.Outcome; outcome == nil || outcome.Status != durable.OutcomeOrphaned || outcome.Reason != "task_too_old" {
		t.Fatalf("outcome = %+v, want orphaned task_too_old", outcome)
	}
	submission, err := h.Submission(bg, submissionID)
	must(t, err)
	expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "task_too_old")
	expectIdleLive(t, h, root)
}

// withoutGeneration is a registry whose snapshots lack pi.generation.
type withoutGeneration struct{ Registry }

func (r withoutGeneration) Snapshot() *RegistrySnapshot {
	snapshot := *r.Registry.Snapshot()
	snapshot.tasks = slices.DeleteFunc(slices.Clone(snapshot.tasks), func(task Task) bool { return task.Name() == "pi.generation" })
	snapshot.taskByName = map[string]Task{}
	for _, task := range snapshot.tasks {
		snapshot.taskByName[task.Name()] = task
	}
	return &snapshot
}

func TestGenerationRejectsARegistryWithoutTheBuiltInTasks(t *testing.T) {
	setup := newChatSetup(t)
	options := setup.options()
	options.Registry = withoutGeneration{NewRegistry()}
	if _, err := Open(bg, memory.New(), options); err == nil || !strings.Contains(err.Error(), "Registry lacks built-in tasks pi.generation") {
		t.Fatalf("open = %v, want the missing built-in", err)
	}
}
