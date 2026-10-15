package agent

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The transcript-owned system prompt and tool loadout (upstream 9e05370b2).
//
// Expected values come from testdata/transcript/agent-95fbc0499.json, captured
// by running pi's Agent and agent loop at 95fbc0499 (and the npm build 0.87.0) under node
// (testdata/transcript/capture.mts, which names the upstream test each scenario
// transliterates). pi stamps Date.now() pinned to the golden's "now"; the Go
// side maps its own wall-clock stamps onto that value before comparing bytes.

// transcriptScenario is the union of the fields the capture records.
type transcriptScenario struct {
	Messages                     string     `json:"messages"`
	SystemPrompt                 *string    `json:"systemPrompt"`
	Requests                     [][]string `json:"requests"`
	Roles                        []string   `json:"roles"`
	System                       []string   `json:"system"`
	SystemCounts                 []int      `json:"systemCounts"`
	CurrentTools                 []string   `json:"currentTools"`
	Restated                     []string   `json:"restated"`
	RemovedUnknown               []string   `json:"removedUnknown"`
	Replayed                     string     `json:"replayed"`
	ReplayedSystemPrompt         string     `json:"replayedSystemPrompt"`
	Error                        string     `json:"error"`
	RequestCount                 int        `json:"requestCount"`
	CallbackContextRoles         []string   `json:"callbackContextRoles"`
	CallbackToolResultIDs        []string   `json:"callbackToolResultIds"`
	Keys                         []string   `json:"keys"`
	SameObject                   bool       `json:"sameObject"`
	SystemCount                  int        `json:"systemCount"`
	Events                       []string   `json:"events"`
	LLMCalls                     int        `json:"llmCalls"`
	PrepareCalls                 int        `json:"prepareCalls"`
	ConvertedSecondTurnHasUpdate bool       `json:"convertedSecondTurnHasUpdate"`
	SecondRequestRoles           []string   `json:"secondRequestRoles"`
	Contents                     []string   `json:"contents"`
	Executed                     []string   `json:"executed"`
	SteeringPolls                int        `json:"steeringPolls"`
	FollowUpPolls                int        `json:"followUpPolls"`
	CallIndex                    int        `json:"callIndex"`
}

var (
	transcriptGoldenOnce sync.Once
	transcriptGoldenNow  int64
	transcriptGolden     map[string]json.RawMessage
	transcriptGoldenErr  error
)

// golden returns one captured scenario.
func golden(t *testing.T, name string) transcriptScenario {
	t.Helper()
	transcriptGoldenOnce.Do(func() {
		raw, err := os.ReadFile("testdata/transcript/agent-95fbc0499.json")
		if err != nil {
			transcriptGoldenErr = err
			return
		}
		transcriptGoldenErr = json.Unmarshal(raw, &transcriptGolden)
		if transcriptGoldenErr == nil {
			transcriptGoldenErr = json.Unmarshal(transcriptGolden["now"], &transcriptGoldenNow)
		}
	})
	if transcriptGoldenErr != nil {
		t.Fatalf("golden: %v", transcriptGoldenErr)
	}
	var s transcriptScenario
	if err := json.Unmarshal(transcriptGolden[name], &s); err != nil || transcriptGolden[name] == nil {
		t.Fatalf("golden scenario %q missing or malformed: %v", name, err)
	}
	return s
}

var wallClockTimestamp = regexp.MustCompile(`"timestamp":\d{13}`)

// piJSON marshals v as pi's JSON.stringify would (Go escapes <, > and & — D9),
// with every wall-clock timestamp mapped onto the golden's pinned Date.now().
func piJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	s := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(raw))
	return wallClockTimestamp.ReplaceAllString(s, `"timestamp":`+jsonInt(transcriptGoldenNow))
}

func jsonInt(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// canonicalPiMessage re-encodes a message captured from pi through the Go
// codec. The message keeps pi's own key order (decoding records it); only the
// tool parameter schemas change, to the Go schema's key order, since TypeBox
// orders "required" before "properties" and ai.Schema does not.
func canonicalPiMessage(t *testing.T, raw string) string {
	t.Helper()
	message, err := ai.UnmarshalMessage([]byte(raw))
	if err != nil {
		t.Fatalf("decode golden message %s: %v", raw, err)
	}
	return piJSON(t, message)
}

func canonicalPiMessages(t *testing.T, raw []string) []string {
	t.Helper()
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = canonicalPiMessage(t, r)
	}
	return out
}

func systemMessagesJSON(t *testing.T, messages []AgentMessage) []string {
	t.Helper()
	var out []string
	for _, m := range messages {
		if system, ok := systemMessageOf(m); ok {
			out = append(out, piJSON(t, system))
		}
	}
	return out
}

func roleNames(messages []AgentMessage) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = string(m.MessageRole())
	}
	return out
}

func assertEqualStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got: %q\nwant: %q", what, got, want)
	}
}

// createTool is agent.test.ts createTool.
func createTool(name string) AgentTool {
	return AgentTool{
		Name:        name,
		Label:       name,
		Description: name + " tool",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: name}}, Details: map[string]any{}}, nil
		},
	}
}

// echoTool is the "echo"/"Echo input" tool of agent.test.ts.
func echoTool() AgentTool {
	return AgentTool{
		Name:        "echo",
		Label:       "Echo",
		Description: "Echo input",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echo"}}, Details: map[string]any{}}, nil
		},
	}
}

// echoValueTool is the "echo"/"Echo tool" tool of agent-loop.test.ts.
func echoValueTool(execute func(value string) AgentToolResult) AgentTool {
	return AgentTool{
		Name:        "echo",
		Label:       "Echo",
		Description: "Echo tool",
		Parameters:  ai.Object(ai.Prop("value", ai.String())),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			value, _ := params["value"].(string)
			if execute != nil {
				return execute(value), nil
			}
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed: " + value}}, Details: map[string]any{"value": value}}, nil
		},
	}
}

func declaration(tool AgentTool) ai.Tool { return ai.ToToolDeclaration(tool.asAITool()) }

func textMessage(text string) *ai.AssistantMessage {
	return &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, StopReason: ai.StopStop, Timestamp: nowMillis()}
}

// replyWith streams a single done event, as the upstream MockAssistantStream
// tests do.
func replyWith(message *ai.AssistantMessage) *ai.AssistantMessageEventStream {
	s := ai.NewAssistantMessageEventStream()
	go func() {
		s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: message.StopReason, Message: message})
		s.End()
	}()
	return s
}

func unusedStreamFn(t *testing.T) StreamFn {
	return func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		t.Error("unexpected stream call")
		panic("unexpected stream call")
	}
}

func textStreamFn(text string) StreamFn {
	return func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		return replyWith(textMessage(text))
	}
}

// identityConverter is agent-loop.test.ts identityConverter.
func identityConverter(messages []AgentMessage) []ai.Message {
	var out []ai.Message
	for _, m := range messages {
		switch m.MessageRole() {
		case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			out = append(out, m)
		}
	}
	return out
}

// eventRecorder records event types as the capture's drain() does: a
// message_end for a system message is followed by the message's JSON.
type eventRecorder struct {
	t      *testing.T
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) sink(e AgentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, string(e.Type))
	if e.Type == EvMessageEnd {
		if system, ok := systemMessageOf(e.Message); ok {
			r.events = append(r.events, "system:"+piJSON(r.t, system))
		}
	}
	return nil
}

// canonicalPiEvents canonicalizes the system-message JSON a pi event trace
// carries.
func canonicalPiEvents(t *testing.T, events []string) []string {
	t.Helper()
	out := make([]string, len(events))
	for i, e := range events {
		if raw, ok := strings.CutPrefix(e, "system:"); ok {
			out[i] = "system:" + canonicalPiMessage(t, raw)
			continue
		}
		out[i] = e
	}
	return out
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Agent: construction, derived prompt, reset, continue
// ---------------------------------------------------------------------------

// agent.test.ts "should create an agent instance with default state".
func TestAgentDefaultStateHasNoSystemMessage(t *testing.T) {
	want := golden(t, "defaultState")
	st := NewAgent(AgentOptions{StreamFn: unusedStreamFn(t)}).State()
	// pi's empty transcript is []; Go's is a nil slice.
	if want.Messages != "[]" || len(st.Messages) != 0 {
		t.Fatalf("messages = %v, pi has %s", st.Messages, want.Messages)
	}
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// agent.test.ts "should create an agent instance with custom initial state":
// the prompt becomes the leading system message, and State derives the prompt
// back from it.
func TestAgentInitialPromptBecomesTheLeadingSystemMessage(t *testing.T) {
	want := golden(t, "customInitialState")
	model := &ai.Model{ID: "gpt-4o-mini", Provider: "openai", Api: "openai-responses"}
	st := NewAgent(AgentOptions{
		StreamFn:     unusedStreamFn(t),
		InitialState: &AgentState{SystemPrompt: "You are a helpful assistant.", Model: model, ThinkingLevel: ThinkLow},
	}).State()
	if got := piJSON(t, st.Messages); got != want.Messages {
		t.Fatalf("messages:\n got: %s\nwant: %s", got, want.Messages)
	}
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
	if st.Model != model || st.ThinkingLevel != ThinkLow {
		t.Fatalf("model/thinking not seeded: %v %q", st.Model, st.ThinkingLevel)
	}
}

// agent.test.ts "converts initial prompt and tools into transcript state".
func TestAgentConvertsInitialPromptAndToolsIntoTranscriptState(t *testing.T) {
	want := golden(t, "initialPromptAndTools")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Tools: []AgentTool{echoTool()}},
		StreamFn:     unusedStreamFn(t),
	})
	st := a.State()
	if got := piJSON(t, st.Messages); got != want.Messages {
		t.Fatalf("messages:\n got: %s\nwant: %s", got, want.Messages)
	}
	if len(st.Tools) != 1 || st.Tools[0].Execute == nil {
		t.Fatalf("the executable tools must stay on the state, got %#v", st.Tools)
	}
}

// An initial transcript that already starts with a system message is kept as
// is: the seed prompt and tools are not prepended.
func TestAgentInitialTranscriptStartingWithASystemMessageIsKept(t *testing.T) {
	want := golden(t, "initialTranscriptStartsWithSystem")
	st := NewAgent(AgentOptions{
		InitialState: &AgentState{
			SystemPrompt: "ignored",
			Tools:        []AgentTool{echoTool()},
			Messages:     []AgentMessage{ai.NewSystemText("kept", 5), ai.NewUserText("old", 6)},
		},
		StreamFn: unusedStreamFn(t),
	}).State()
	if got := piJSON(t, st.Messages); got != want.Messages {
		t.Fatalf("messages:\n got: %s\nwant: %s", got, want.Messages)
	}
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// Only a leading system message stops seeding: a system message later in the
// initial transcript still gets the seed prompt prepended ahead of it.
func TestAgentInitialTranscriptWithALaterSystemMessageIsSeeded(t *testing.T) {
	want := golden(t, "initialTranscriptWithLaterSystem")
	st := NewAgent(AgentOptions{
		InitialState: &AgentState{
			SystemPrompt: "P",
			Messages:     []AgentMessage{ai.NewUserText("u", 1), ai.NewSystemText("late", 2)},
		},
		StreamFn: unusedStreamFn(t),
	}).State()
	if got := piJSON(t, st.Messages); got != want.Messages {
		t.Fatalf("messages:\n got: %s\nwant: %s", got, want.Messages)
	}
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// State().SystemPrompt is replayed from the transcript, so a sections patch
// the run appended shows up in it.
func TestAgentSystemPromptIsDerivedFromTheTranscript(t *testing.T) {
	want := golden(t, "derivedSystemPrompt")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel},
		StreamFn:     textStreamFn("done"),
	})
	patch := ai.SystemMessage{Sections: ai.SystemSections{{Name: "skills", Value: strPtr("<skills>x</skills>")}}, Timestamp: 1}
	if err := a.PromptMessages(context.Background(), []AgentMessage{patch, ai.NewUserText("hi", 2)}); err != nil {
		t.Fatal(err)
	}
	if got := a.State().SystemPrompt; got != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", got, *want.SystemPrompt)
	}
}

// agent.test.ts "restores the transcript baseline when reset", plus the replay
// of a transcript whose later system messages changed the prompt and tools.
func TestAgentRestoresTheTranscriptBaselineWhenReset(t *testing.T) {
	want := golden(t, "resetBaseline")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{
			SystemPrompt: "You are helpful.",
			Tools:        []AgentTool{echoTool()},
			Messages:     []AgentMessage{ai.NewUserText("old", 1)},
		},
		StreamFn: unusedStreamFn(t),
	})
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := piJSON(t, a.State().Messages); got != want.Messages {
		t.Fatalf("messages after reset:\n got: %s\nwant: %s", got, want.Messages)
	}

	base := ai.NewSystemText("base", 3)
	base.Sections = ai.SystemSections{{Name: "a", Value: strPtr("A")}}
	base.ToolsAdded = []ai.Tool{declaration(echoTool())}
	more := ai.NewSystemText("more", 5)
	more.Sections = ai.SystemSections{{Name: "a"}, {Name: "b", Value: strPtr("B")}}
	more.ToolsRemoved = []ai.ToolReference{{Name: "echo"}}
	replayed := NewAgent(AgentOptions{
		InitialState: &AgentState{Messages: []AgentMessage{base, ai.NewUserText("u", 4), more}},
		StreamFn:     unusedStreamFn(t),
	})
	if err := replayed.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := piJSON(t, replayed.State().Messages); got != want.Replayed {
		t.Fatalf("replayed baseline:\n got: %s\nwant: %s", got, want.Replayed)
	}
	if got := replayed.State().SystemPrompt; got != want.ReplayedSystemPrompt {
		t.Fatalf("replayed SystemPrompt = %q, want %q", got, want.ReplayedSystemPrompt)
	}

	empty := NewAgent(AgentOptions{
		InitialState: &AgentState{Messages: []AgentMessage{ai.NewUserText("old", 1)}},
		StreamFn:     unusedStreamFn(t),
	})
	if err := empty.Reset(); err != nil {
		t.Fatal(err)
	}
	if got, want := empty.State().Messages, golden(t, "resetWithoutSystemMessage").Messages; want != "[]" || len(got) != 0 {
		t.Fatalf("reset without a system message kept %v, pi keeps %s", got, want)
	}
}

// agent.ts continue(): a transcript holding only system messages has nothing
// to continue from. (pi seeds it from initialState.systemPrompt; the transcript
// is given directly here so the check does not lean on seeding.)
func TestAgentContinueRejectsASystemOnlyTranscript(t *testing.T) {
	want := golden(t, "continueSystemOnly")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Messages: []AgentMessage{ai.NewSystemText("You are helpful.", 0)}},
		StreamFn:     unusedStreamFn(t),
	})
	err := a.Continue(context.Background())
	if err == nil || err.Error() != want.Error {
		t.Fatalf("Continue error = %v, want %q", err, want.Error)
	}
	if got := roleNames(a.State().Messages); !slices.Equal(got, []string{"system"}) {
		t.Fatalf("a refused Continue must leave the transcript alone, got %v", got)
	}
}

// agent.ts continue(): a transcript that merely ends in a system message still
// holds a message to continue from, so Continue makes the request.
func TestAgentContinueFromATranscriptEndingInASystemMessage(t *testing.T) {
	want := golden(t, "continueLastSystem")
	var requests [][]string
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{
			Model:    testModel,
			Messages: []AgentMessage{ai.NewSystemText("P", 0), ai.NewUserText("u", 1), ai.NewSystemText("late", 2)},
		},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests = append(requests, roleNames(req.Messages))
			return replyWith(textMessage("done"))
		},
	})
	err := a.Continue(context.Background())
	if want.Error != "" || err != nil {
		t.Fatalf("Continue error = %v, pi's is %q", err, want.Error)
	}
	if len(requests) != len(want.Requests) {
		t.Fatalf("requests = %q, want %q", requests, want.Requests)
	}
	for i := range want.Requests {
		assertEqualStrings(t, "request roles", requests[i], want.Requests[i])
	}
	st := a.State()
	assertEqualStrings(t, "transcript roles", roleNames(st.Messages), want.Roles)
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// ---------------------------------------------------------------------------
// Agent: tool loadout declarations
// ---------------------------------------------------------------------------

// agent.test.ts "declares tool loadout changes to the model before the next
// request".
func TestAgentDeclaresToolLoadoutChangesBeforeTheNextRequest(t *testing.T) {
	want := golden(t, "declaresLoadoutChanges")
	var requests [][]string
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel, Tools: []AgentTool{createTool("first")}},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			var request []string
			for _, m := range req.Messages {
				system, ok := systemMessageOf(m)
				if !ok {
					continue
				}
				var added, removed []string
				for _, tool := range system.ToolsAdded {
					added = append(added, tool.Name)
				}
				for _, tool := range system.ToolsRemoved {
					removed = append(removed, tool.Name)
				}
				request = append(request, "+"+strings.Join(added, ","), "-"+strings.Join(removed, ","))
			}
			requests = append(requests, request)
			return replyWith(textMessage("done"))
		},
	})

	ctx := context.Background()
	if err := a.Prompt(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	a.SetTools([]AgentTool{createTool("second")})
	if err := a.Prompt(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(ctx, "three"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != len(want.Requests) {
		t.Fatalf("requests = %q, want %q", requests, want.Requests)
	}
	for i := range want.Requests {
		assertEqualStrings(t, "request tool declarations", requests[i], want.Requests[i])
	}
	st := a.State()
	assertEqualStrings(t, "transcript roles", roleNames(st.Messages), want.Roles)
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// agent.test.ts "merges tool changes into a pending system message".
func TestAgentMergesToolChangesIntoAPendingSystemMessage(t *testing.T) {
	want := golden(t, "mergesPendingSystemMessage")
	var systemCounts []int
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel},
		StreamFn: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			count := 0
			for _, m := range req.Messages {
				if m.MessageRole() == ai.RoleSystem {
					count++
				}
			}
			systemCounts = append(systemCounts, count)
			return replyWith(textMessage("done"))
		},
	})
	a.SetTools([]AgentTool{echoTool()})
	pending := ai.SystemMessage{Sections: ai.SystemSections{{Name: "skills", Value: strPtr("<skills>x</skills>")}}, Timestamp: 1}
	if err := a.PromptMessages(context.Background(), []AgentMessage{pending, ai.NewUserText("hi", 2)}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(systemCounts, want.SystemCounts) {
		t.Fatalf("system messages per request = %v, want %v", systemCounts, want.SystemCounts)
	}
	st := a.State()
	assertEqualStrings(t, "transcript roles", roleNames(st.Messages), want.Roles)
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
	if st.SystemPrompt != *want.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want %q", st.SystemPrompt, *want.SystemPrompt)
	}
}

// agent.test.ts "rewrites pending tool declarations to match the executable
// set": the pending message claims to add `second` and remove `first`, but the
// executable set still has `first` and lacks `second`, so the executable set
// wins.
func TestAgentRewritesPendingToolDeclarationsToMatchTheExecutableSet(t *testing.T) {
	want := golden(t, "rewritesPendingDeclarations")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel, Tools: []AgentTool{createTool("first")}},
		StreamFn:     textStreamFn("done"),
	})
	pending := ai.SystemMessage{
		Sections:     ai.SystemSections{{Name: "note", Value: strPtr("<note>x</note>")}},
		ToolsAdded:   []ai.Tool{declaration(createTool("second"))},
		ToolsRemoved: []ai.ToolReference{{Name: "first"}},
		Timestamp:    1,
	}
	if err := a.PromptMessages(context.Background(), []AgentMessage{pending, ai.NewUserText("hi", 2)}); err != nil {
		t.Fatal(err)
	}

	st := a.State()
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
	current, _ := ai.GetCurrentSystemMessage(st.Messages)
	var names []string
	for _, tool := range current.ToolsAdded {
		names = append(names, tool.Name)
	}
	assertEqualStrings(t, "current tools", names, want.CurrentTools)
}

// A pending message that already declares exactly the missing tools is rebuilt
// all the same: its declarations are intent, replaced by the delta, and the tool
// keys move after timestamp as withToolChanges' spread puts them.
func TestAgentPendingDeclarationOfTheDeltaIsRebuilt(t *testing.T) {
	want := golden(t, "pendingDeclaresTheDelta")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel},
		StreamFn:     textStreamFn("done"),
	})
	a.SetTools([]AgentTool{echoTool()})
	pending := ai.NewSystemText("", 1)
	pending.ToolsAdded = []ai.Tool{declaration(echoTool())}
	if err := a.PromptMessages(context.Background(), []AgentMessage{pending, ai.NewUserText("hi", 2)}); err != nil {
		t.Fatal(err)
	}
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, a.State().Messages), canonicalPiMessages(t, want.System))
}

// A pending message whose tool fields change nothing is rebuilt without them;
// only one declaring no tool fields at all is kept as the caller's.
func TestAgentPendingToolFieldsThatChangeNothingAreDropped(t *testing.T) {
	want := golden(t, "pendingNoOpToolFieldsAreDropped")
	run := func(edit func(*ai.SystemMessage)) []string {
		a := NewAgent(AgentOptions{
			InitialState: &AgentState{SystemPrompt: "You are helpful.", Model: testModel, Tools: []AgentTool{echoTool()}},
			StreamFn:     textStreamFn("done"),
		})
		pending := ai.NewSystemText("", 1)
		edit(&pending)
		if err := a.PromptMessages(context.Background(), []AgentMessage{pending, ai.NewUserText("hi", 2)}); err != nil {
			t.Fatal(err)
		}
		return systemMessagesJSON(t, a.State().Messages)
	}
	assertEqualStrings(t, "restated loadout",
		run(func(m *ai.SystemMessage) { m.ToolsAdded = []ai.Tool{declaration(echoTool())} }),
		canonicalPiMessages(t, want.Restated))
	assertEqualStrings(t, "removal of an undeclared tool",
		run(func(m *ai.SystemMessage) { m.ToolsRemoved = []ai.ToolReference{{Name: "ghost"}} }),
		canonicalPiMessages(t, want.RemovedUnknown))
}

// Only the last pending system message carries the delta; an earlier one keeps
// its own declarations, which count toward the baseline.
func TestAgentLastPendingSystemMessageCarriesTheDelta(t *testing.T) {
	want := golden(t, "lastPendingSystemMessageCarriesTheDelta")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{echoTool()}},
		StreamFn:     textStreamFn("done"),
	})
	first := ai.NewSystemText("a", 1)
	first.ToolsAdded = []ai.Tool{declaration(createTool("x"))}
	if err := a.PromptMessages(context.Background(), []AgentMessage{first, ai.NewSystemText("b", 2), ai.NewUserText("hi", 3)}); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	assertEqualStrings(t, "transcript roles", roleNames(st.Messages), want.Roles)
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
}

// Without a pending system message the declaration is a new message inserted
// before the first non-system pending message.
func TestAgentDeclarationIsInsertedBeforeTheFirstNonSystemPendingMessage(t *testing.T) {
	want := golden(t, "declarationInsertedBeforeFirstNonSystem")
	a := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: testModel, Tools: []AgentTool{echoTool()}, Messages: []AgentMessage{ai.NewUserText("old", 1)}},
		StreamFn:     textStreamFn("done"),
	})
	a.SetTools([]AgentTool{echoTool(), createTool("second")})
	if err := a.PromptMessages(context.Background(), []AgentMessage{ai.NewUserText("one", 2), ai.NewUserText("two", 3)}); err != nil {
		t.Fatal(err)
	}
	st := a.State()
	assertEqualStrings(t, "transcript roles", roleNames(st.Messages), want.Roles)
	assertEqualStrings(t, "system messages", systemMessagesJSON(t, st.Messages), canonicalPiMessages(t, want.System))
}

// ---------------------------------------------------------------------------
// Loop
// ---------------------------------------------------------------------------

// agent-loop.test.ts "should build provider context exclusively from
// transcript messages". The provider receives a transcript: the prompt system
// message is the caller's own (kept, not rebuilt, since it declares no tool
// change) and nothing is synthesized ahead of it.
func TestLoopBuildsProviderContextExclusivelyFromTranscriptMessages(t *testing.T) {
	want := golden(t, "loopProviderContext")
	system := ai.NewSystemText("Transcript prompt", 1)
	system.ToolsAdded = []ai.Tool{}
	initial := &system

	var keys []string
	var sameObject bool
	var systemCount int
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{initial, ai.NewUserText("Hello", 2)},
		AgentContext{Tools: []AgentTool{}},
		AgentLoopConfig{Model: testModel, ConvertToLlm: identityConverter},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			raw, _ := json.Marshal(req)
			var object map[string]json.RawMessage
			_ = json.Unmarshal(raw, &object)
			for key := range object {
				keys = append(keys, key)
			}
			sameObject = len(req.Messages) > 0 && req.Messages[0] == AgentMessage(initial)
			for _, m := range req.Messages {
				if m.MessageRole() == ai.RoleSystem {
					systemCount++
				}
			}
			return replyWith(textMessage("done"))
		})

	assertEqualStrings(t, "provider context keys", keys, want.Keys)
	if sameObject != want.SameObject || systemCount != want.SystemCount {
		t.Fatalf("provider messages[0] is the caller's message = %v (want %v); system messages = %d (want %d)",
			sameObject, want.SameObject, systemCount, want.SystemCount)
	}
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// The same, with executable tools the pending message already declares: the
// declaration is rebuilt on that message, never added as a second one, and no
// tools ride on the provider context.
func TestLoopProviderContextCarriesDeclaredToolsOnlyInTheTranscript(t *testing.T) {
	want := golden(t, "loopProviderContextDeclaredTools")
	tool := echoValueTool(nil)
	initial := ai.NewSystemText("Transcript prompt", 1)
	initial.ToolsAdded = []ai.Tool{declaration(tool)}
	pointer := &initial

	var sameObject bool
	var systemCount int
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{pointer, ai.NewUserText("Hello", 2)},
		AgentContext{Tools: []AgentTool{tool}},
		AgentLoopConfig{Model: testModel, ConvertToLlm: identityConverter},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			sameObject = len(req.Messages) > 0 && req.Messages[0] == AgentMessage(pointer)
			for _, m := range req.Messages {
				if m.MessageRole() == ai.RoleSystem {
					systemCount++
				}
			}
			return replyWith(textMessage("done"))
		})

	if sameObject != want.SameObject || systemCount != want.SystemCount {
		t.Fatalf("provider messages[0] is the caller's message = %v (want %v); system messages = %d (want %d)",
			sameObject, want.SameObject, systemCount, want.SystemCount)
	}
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// agent-loop.test.ts "should use prepareNextTurn snapshot before continuing":
// the snapshot's messages are appended, with lifecycle events, before the
// request it prepares.
func TestLoopAppendsPreparedMessagesBeforeTheNextRequest(t *testing.T) {
	want := golden(t, "loopPrepareNextTurn")
	var prepareCalls, llmCalls int
	prepared := false
	convertedSecondTurnHasUpdate := false
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo something", 1)},
		AgentContext{Tools: []AgentTool{echoValueTool(nil)}},
		AgentLoopConfig{
			Model:        testModel,
			ConvertToLlm: identityConverter,
			PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
				prepareCalls++
				if prepared {
					return nil
				}
				prepared = true
				return &AgentLoopTurnUpdate{
					Context:  &AgentContext{Messages: slices.Clone(c.Context.Messages), Tools: c.Context.Tools},
					Messages: []AgentMessage{ai.NewSystemText("updated guidance", 1)},
				}
			},
		},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			llmCalls++
			if llmCalls == 1 {
				return replyWith(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
			}
			for _, m := range req.Messages {
				if system, ok := systemMessageOf(m); ok && ai.ContentText(system.Content) == "updated guidance" {
					convertedSecondTurnHasUpdate = true
				}
			}
			return replyWith(textMessage("done"))
		})

	if llmCalls != want.LLMCalls || prepareCalls != want.PrepareCalls || convertedSecondTurnHasUpdate != want.ConvertedSecondTurnHasUpdate {
		t.Fatalf("llmCalls=%d prepareCalls=%d secondTurnHasUpdate=%v, want %d %d %v",
			llmCalls, prepareCalls, convertedSecondTurnHasUpdate, want.LLMCalls, want.PrepareCalls, want.ConvertedSecondTurnHasUpdate)
	}
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// Prepared messages go ahead of queued steering, and a tool change in the
// prepared context is declared on the prepared system message.
func TestLoopPreparedMessagesPrecedeSteeringAndCarryToolChanges(t *testing.T) {
	want := golden(t, "loopPreparedBeforeSteering")
	var llmCalls int
	var secondRequestRoles []string
	prepared, steered := false, false
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo something", 1)},
		AgentContext{Tools: []AgentTool{echoValueTool(nil)}},
		AgentLoopConfig{
			Model:        testModel,
			ConvertToLlm: identityConverter,
			GetSteeringMessages: func() []AgentMessage {
				if llmCalls == 1 && !steered {
					steered = true
					return []AgentMessage{ai.NewUserText("steer", 7)}
				}
				return nil
			},
			PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
				if prepared {
					return nil
				}
				prepared = true
				return &AgentLoopTurnUpdate{
					Context: &AgentContext{
						Messages: slices.Clone(c.Context.Messages),
						Tools:    append(slices.Clone(c.Context.Tools), createTool("second")),
					},
					Messages: []AgentMessage{ai.NewSystemText("updated guidance", 1), ai.NewUserText("prepared", 2)},
				}
			},
		},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			llmCalls++
			if llmCalls == 1 {
				return replyWith(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
			}
			secondRequestRoles = roleNames(req.Messages)
			return replyWith(textMessage("done"))
		})

	if llmCalls != want.LLMCalls {
		t.Fatalf("llmCalls = %d, want %d", llmCalls, want.LLMCalls)
	}
	assertEqualStrings(t, "second request roles", secondRequestRoles, want.SecondRequestRoles)
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
	var contents []string
	for _, m := range messages {
		switch v := m.(type) {
		case ai.SystemMessage:
			s, _ := v.StringContent()
			contents = append(contents, s)
		case ai.UserMessage:
			contents = append(contents, ai.ContentText(v.Content))
		default:
			contents = append(contents, "")
		}
	}
	assertEqualStrings(t, "new message contents", contents, want.Contents)
}

// A prepared context that drops a tool, with no prepared messages, gets a
// declaration of its own before the request.
func TestLoopDeclaresToolsDroppedByAPreparedContext(t *testing.T) {
	want := golden(t, "loopPreparedToolRemoval")
	var llmCalls int
	prepared := false
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo something", 1)},
		AgentContext{Tools: []AgentTool{echoValueTool(nil)}},
		AgentLoopConfig{
			Model:        testModel,
			ConvertToLlm: identityConverter,
			PrepareNextTurn: func(c AgentTurnContext) *AgentLoopTurnUpdate {
				if prepared {
					return nil
				}
				prepared = true
				return &AgentLoopTurnUpdate{Context: &AgentContext{Messages: slices.Clone(c.Context.Messages), Tools: []AgentTool{}}}
			},
		},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			llmCalls++
			if llmCalls == 1 {
				return replyWith(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
			}
			return replyWith(textMessage("done"))
		})

	if llmCalls != want.LLMCalls {
		t.Fatalf("llmCalls = %d, want %d", llmCalls, want.LLMCalls)
	}
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// A snapshot with prepared messages but no context still appends them, declared
// against the unchanged context: the prepared system message's claim to add a
// tool the runtime cannot execute is dropped.
func TestLoopAppendsPreparedMessagesWithoutAContext(t *testing.T) {
	want := golden(t, "loopPreparedMessagesWithoutContext")
	var llmCalls int
	var secondRequestRoles []string
	prepared := false
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo something", 1)},
		AgentContext{Tools: []AgentTool{echoValueTool(nil)}},
		AgentLoopConfig{
			Model:        testModel,
			ConvertToLlm: identityConverter,
			PrepareNextTurn: func(AgentTurnContext) *AgentLoopTurnUpdate {
				if prepared {
					return nil
				}
				prepared = true
				guide := ai.NewSystemText("guide", 5)
				guide.ToolsAdded = []ai.Tool{declaration(createTool("ghost"))}
				return &AgentLoopTurnUpdate{Messages: []AgentMessage{guide}}
			},
		},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			llmCalls++
			if llmCalls == 1 {
				return replyWith(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
			}
			secondRequestRoles = roleNames(req.Messages)
			return replyWith(textMessage("done"))
		})

	if llmCalls != want.LLMCalls {
		t.Fatalf("llmCalls = %d, want %d", llmCalls, want.LLMCalls)
	}
	assertEqualStrings(t, "second request roles", secondRequestRoles, want.SecondRequestRoles)
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// A new declaration is inserted before the first non-system pending message
// even when that is a custom message the provider never sees, not before the
// first user message.
func TestLoopDeclarationIsInsertedBeforeAPendingCustomMessage(t *testing.T) {
	want := golden(t, "loopDeclarationBeforeCustomMessage")
	var requests [][]string
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{uiNotification{}, ai.NewUserText("u", 2)},
		AgentContext{Tools: []AgentTool{createTool("echo")}},
		AgentLoopConfig{Model: testModel, ConvertToLlm: identityConverter},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			requests = append(requests, roleNames(req.Messages))
			return replyWith(textMessage("done"))
		})

	if len(requests) != len(want.Requests) {
		t.Fatalf("requests = %q, want %q", requests, want.Requests)
	}
	for i := range want.Requests {
		assertEqualStrings(t, "request roles", requests[i], want.Requests[i])
	}
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// agent-loop.test.ts "action:end receives finalized turn context and stops
// before queue polling": the context declares no tools, so the loop announces
// the loadout with a system message ahead of the prompt.
func TestLoopFinishTurnEndAnnouncesTheLoadoutFirst(t *testing.T) {
	want := golden(t, "loopFinishTurnEnd")
	var executed []string
	tool := echoValueTool(func(value string) AgentToolResult {
		executed = append(executed, value)
		return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echoed: " + value}}, Details: map[string]any{"value": value}}
	})
	var steeringPolls, followUpPolls, llmCalls int
	var callbackToolResultIDs, callbackContextRoles []string
	rec := &eventRecorder{t: t}
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo something", 1)},
		AgentContext{Tools: []AgentTool{tool}},
		AgentLoopConfig{
			Model:        testModel,
			ConvertToLlm: identityConverter,
			GetSteeringMessages: func() []AgentMessage {
				steeringPolls++
				return nil
			},
			GetFollowUpMessages: func() []AgentMessage {
				followUpPolls++
				return []AgentMessage{ai.NewUserText("follow up should stay queued", 2)}
			},
			FinishTurn: func(_ context.Context, c AgentTurnContext) AgentTurnDecision {
				for _, r := range c.ToolResults {
					callbackToolResultIDs = append(callbackToolResultIDs, r.ToolCallID)
				}
				callbackContextRoles = roleNames(c.Context.Messages)
				return TurnEnd
			},
		},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			llmCalls++
			if llmCalls == 1 {
				return replyWith(assistantWithToolCall("tool-1", "echo", map[string]any{"value": "hello"}))
			}
			return replyWith(textMessage("should not run"))
		})

	if llmCalls != want.LLMCalls || steeringPolls != want.SteeringPolls || followUpPolls != want.FollowUpPolls {
		t.Fatalf("llmCalls=%d steeringPolls=%d followUpPolls=%d, want %d %d %d",
			llmCalls, steeringPolls, followUpPolls, want.LLMCalls, want.SteeringPolls, want.FollowUpPolls)
	}
	assertEqualStrings(t, "executed", executed, want.Executed)
	assertEqualStrings(t, "callback tool result ids", callbackToolResultIDs, want.CallbackToolResultIDs)
	assertEqualStrings(t, "callback context roles", callbackContextRoles, want.CallbackContextRoles)
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
}

// agent-loop.test.ts "should continue after parallel tool calls when not all
// tool results terminate".
func TestLoopParallelToolCallsContinueUnlessAllTerminate(t *testing.T) {
	want := golden(t, "loopParallelNotAllTerminate")
	tool := echoValueTool(func(value string) AgentToolResult {
		return AgentToolResult{
			Content:   ai.ContentList{ai.TextContent{Text: "echoed: " + value}},
			Details:   map[string]any{"value": value},
			Terminate: value == "first",
		}
	})
	var callIndex int
	messages := runAgentLoop(context.Background(),
		[]AgentMessage{ai.NewUserText("echo both", 1)},
		AgentContext{Tools: []AgentTool{tool}},
		AgentLoopConfig{Model: testModel, ConvertToLlm: identityConverter, ToolExecution: ToolParallel},
		func(AgentEvent) error { return nil },
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			message := textMessage("done")
			if callIndex == 0 {
				message = &ai.AssistantMessage{
					Content: ai.ContentList{
						ai.ToolCall{ID: "tool-1", Name: "echo", Arguments: map[string]any{"value": "first"}},
						ai.ToolCall{ID: "tool-2", Name: "echo", Arguments: map[string]any{"value": "second"}},
					},
					StopReason: ai.StopToolUse,
				}
			}
			callIndex++
			return replyWith(message)
		})

	if callIndex != want.CallIndex {
		t.Fatalf("callIndex = %d, want %d", callIndex, want.CallIndex)
	}
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// A continuation declares the executable tools before its first request.
func TestLoopContinueDeclaresTheExecutableTools(t *testing.T) {
	want := golden(t, "loopContinueDeclaresTools")
	rec := &eventRecorder{t: t}
	messages := runAgentLoopContinue(context.Background(),
		AgentContext{Messages: []AgentMessage{ai.NewUserText("Hello", 1)}, Tools: []AgentTool{echoValueTool(nil)}},
		AgentLoopConfig{Model: testModel, ConvertToLlm: identityConverter},
		rec.sink,
		func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return replyWith(textMessage("done"))
		})
	assertEqualStrings(t, "events", rec.events, canonicalPiEvents(t, want.Events))
	assertEqualStrings(t, "new messages", roleNames(messages), want.Roles)
}

// agent.ts defaultConvertToLlm passes system messages through.
func TestDefaultConvertToLlmPassesSystemMessages(t *testing.T) {
	in := []AgentMessage{ai.NewSystemText("sp", 0), ai.NewUserText("u", 1), &ai.AssistantMessage{}, ai.ToolResultMessage{}, uiNotification{}}
	got := roleNames(defaultConvertToLlm(in))
	assertEqualStrings(t, "converted roles", got, []string{"system", "user", "assistant", "toolResult"})
}

// uiNotification is a custom, UI-only transcript message.
type uiNotification struct{}

func (uiNotification) MessageRole() ai.Role { return "notification" }
