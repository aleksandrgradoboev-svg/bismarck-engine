// Package chattest is chat test support for packages that build on the
// Harness (pi packages/durable/test/chat-support.ts): a faux model, a
// registry, and a Harness opened on them through the exported API. The
// harness package's own tests keep their copy, as they cannot import this one
// without a cycle.
package chattest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Chat is the models and registry that survive a close and reopen, like a
// host process's own objects.
type Chat struct {
	Faux     *providers.FauxProviderRegistration
	Models   ai.Models
	Registry harness.Registry
	// Telemetry is the Harness's adapter; nil for none.
	Telemetry telemetry.Context
}

// New registers a faux provider for the test's duration and returns a chat
// on it with an empty registry.
func New(t testing.TB) *Chat {
	t.Helper()
	faux := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	t.Cleanup(faux.Unregister)
	models := ai.CreateModels(nil)
	models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
		ID:   faux.Models[0].Provider,
		Name: "Faux",
		Auth: ai.ProviderAuth{APIKey: &ai.ApiKeyAuth{Name: "Faux", Resolve: func(context.Context, ai.AuthContext, *ai.Credential) (*ai.AuthResult, error) {
			return &ai.AuthResult{}, nil
		}}},
		Models: faux.Models,
		API: &ai.ProviderStreams{
			StreamSimple: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return ai.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, opts)
			},
		},
	}))
	return &Chat{Faux: faux, Models: models, Registry: harness.NewRegistry()}
}

// Install installs an extension with tools.
func (c *Chat) Install(t testing.TB, name string, tools ...*harness.ToolRegistration) {
	t.Helper()
	Must(t, c.Registry.Install(&harness.Extension{Name: name, Tools: tools}))
}

// Open opens a Harness over storage, with e as every conversation's
// environment (nil for none), and returns its root on the faux model. The
// Harness closes at cleanup.
func (c *Chat) Open(t testing.TB, storage durable.Storage, e env.ExecutionEnv) (*harness.Harness, *harness.Conversation) {
	t.Helper()
	options := harness.Options{Models: c.Models, Registry: c.Registry, Telemetry: c.Telemetry}
	if e != nil {
		options.Env = func(context.Context, harness.EnvTarget) (env.ExecutionEnv, error) { return e, nil }
	}
	h, err := harness.Open(context.Background(), storage, options)
	Must(t, err)
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	root, err := h.Root(context.Background(), &harness.AgentChange{Model: harness.Set(harness.ModelRef{Provider: "faux", ModelID: "faux-1"})}, nil)
	Must(t, err)
	return h, root
}

// Must fails the test on an error.
func Must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Call is one tool call of a faux answer.
type Call struct {
	Name string
	Args map[string]any
	ID   string
}

// ToolCalls is a faux answer calling tools.
func ToolCalls(calls ...Call) providers.FauxResponseStep {
	content := ai.ContentList{}
	for _, c := range calls {
		args := c.Args
		if args == nil {
			args = map[string]any{}
		}
		content = append(content, providers.FauxToolCall(c.Name, args, c.ID))
	}
	return providers.FauxStatic(providers.FauxAssistantMessage(content, ai.StopToolUse))
}

// Done is a faux final answer "done".
var Done = providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("done")}, ai.StopStop))

// Input is an input submission of text.
func Input(text string) harness.SubmissionDraft {
	return harness.SubmissionDraft{Type: durable.SubmissionTypeInput, Input: &harness.InputDraft{Content: harness.TextInput(text)}}
}

// SubmitAndWait submits text and waits until the submission settles.
func SubmitAndWait(t testing.TB, conversation *harness.Conversation, text string) durable.SubmissionRecord {
	t.Helper()
	submission, err := conversation.Submit(context.Background(), Input(text))
	Must(t, err)
	record, err := submission.Wait(context.Background())
	Must(t, err)
	return record
}

// Entries are a conversation's entries, oldest first.
func Entries(t testing.TB, conversation *harness.Conversation) []durable.EntryRecord {
	t.Helper()
	page, err := conversation.Entries(context.Background(), durable.EntryQuery{}, 1000, nil)
	Must(t, err)
	out := make([]durable.EntryRecord, len(page.Items))
	for i, entry := range page.Items {
		out[len(out)-1-i] = entry
	}
	return out
}

// ToolResults are the tool result entries among entries.
func ToolResults(entries []durable.EntryRecord) []durable.EntryRecord {
	var out []durable.EntryRecord
	for _, entry := range entries {
		if entry.Kind == durable.ToolResultEntry.Kind {
			out = append(out, entry)
		}
	}
	return out
}

// ResultMessage is a tool result entry's message.
func ResultMessage(entry durable.EntryRecord) ai.ToolResultMessage {
	message, _ := entry.Model[0].(ai.ToolResultMessage)
	return message
}

// ResultText joins a tool result's content with "|", a non-text item as
// "[other]".
func ResultText(message ai.ToolResultMessage) string {
	parts := make([]string, len(message.Content))
	for i, item := range message.Content {
		if text, ok := item.(ai.TextContent); ok {
			parts[i] = text.Text
		} else {
			parts[i] = "[other]"
		}
	}
	return strings.Join(parts, "|")
}

// DiagnosticCodes are the codes of a tool result entry's diagnostics.
func DiagnosticCodes(t testing.TB, entry durable.EntryRecord) []string {
	t.Helper()
	var data struct {
		Diagnostics []harness.ToolDiagnostic `json:"diagnostics"`
	}
	Must(t, json.Unmarshal(entry.Data, &data))
	codes := make([]string, len(data.Diagnostics))
	for i, d := range data.Diagnostics {
		codes[i] = d.Code
	}
	return codes
}
