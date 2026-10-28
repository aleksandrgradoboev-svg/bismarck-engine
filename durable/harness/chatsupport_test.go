package harness

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Chat test support (pi packages/durable/test/chat-support.ts).

// chatSetup is the models and registry that survive a close/reopen, like a
// host process's own objects.
type chatSetup struct {
	faux   *providers.FauxProviderRegistration
	models ai.Models
	// modelsFunc, when set, is Options.ModelsFunc.
	modelsFunc func() ai.Models
	registry   Registry
	reports    *reportLog
	mu         sync.Mutex
	// settings are the live Harness settings; tests change them between
	// decisions through setSettings.
	settings HarnessSettings
	now      func() time.Time
	// telemetry is the Harness's adapter, nil for none.
	telemetry telemetry.Context
	// identity is Options.Identity.
	identity *ai.ClientIdentity
}

func newChatSetup(t *testing.T) *chatSetup {
	t.Helper()
	return newChatSetupWith(t, providers.RegisterFauxProviderOptions{})
}

// newChatSetupWith is newChatSetup with faux provider options, such as a
// streaming rate (pi chatSetup(options)).
func newChatSetupWith(t *testing.T, fauxOptions providers.RegisterFauxProviderOptions) *chatSetup {
	t.Helper()
	faux := providers.RegisterFauxProvider(fauxOptions)
	t.Cleanup(faux.Unregister)
	models := ai.CreateModels(nil)
	models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
		ID:   faux.Models[0].Provider,
		Name: "Faux",
		Auth: ai.ProviderAuth{APIKey: &ai.ApiKeyAuth{Name: "Faux", Resolve: func(context.Context, ai.AuthContext, *ai.Credential) (*ai.AuthResult, error) {
			return &ai.AuthResult{}, nil
		}}},
		Models: faux.Models,
		// The faux streams are registered by API; the provider hands each
		// request to them.
		API: &ai.ProviderStreams{
			StreamSimple: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return ai.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, opts)
			},
			FetchDeferred: func(ctx context.Context, model *ai.Model, handle ai.DeferredHandle, opts *ai.DeferredFetchOptions) *ai.AssistantMessageEventStream {
				api, _ := ai.GetApiProvider(model.Api)
				return api.FetchDeferred(ctx, model, handle, opts)
			},
			CancelDeferred: func(ctx context.Context, model *ai.Model, handle ai.DeferredHandle, opts *ai.DeferredCancelOptions) error {
				api, _ := ai.GetApiProvider(model.Api)
				return api.CancelDeferred(ctx, model, handle, opts)
			},
		},
	}))
	return &chatSetup{faux: faux, models: models, registry: NewRegistry(), reports: &reportLog{}, now: time.Now}
}

func (c *chatSetup) setSettings(change func(*HarnessSettings)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change(&c.settings)
}

func (c *chatSetup) setNow(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func (c *chatSetup) options() Options {
	return Options{
		Models:     c.models,
		ModelsFunc: c.modelsFunc,
		Registry:   c.registry,
		Settings: func() HarnessSettings {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.settings
		},
		Now: func() time.Time {
			c.mu.Lock()
			now := c.now
			c.mu.Unlock()
			return now()
		},
		OnReport:  c.reports.add,
		Telemetry: c.telemetry,
		Identity:  c.identity,
	}
}

// openChat opens a Harness over storage and returns its root, configured
// with the faux model on first creation. The Harness closes at cleanup.
func openChat(t *testing.T, storage durable.Storage, setup *chatSetup) (*Harness, *Conversation) {
	t.Helper()
	h, err := Open(bg, storage, setup.options())
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	root, err := h.Root(bg, &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})}, nil)
	must(t, err)
	return h, root
}

// chatEntries returns the raw entries of a conversation, oldest first.
func chatEntries(t *testing.T, conversation *Conversation) []durable.EntryRecord {
	t.Helper()
	page, err := conversation.Entries(bg, durable.EntryQuery{}, 1000, nil)
	must(t, err)
	out := make([]durable.EntryRecord, len(page.Items))
	for i, entry := range page.Items {
		out[len(out)-1-i] = entry
	}
	return out
}

// unanswered is a faux response that never answers: the run stays busy until
// its generation is cancelled. reached closes once the request was sent,
// after the generation's preparation and request commits.
func unanswered() (step providers.FauxResponseStep, reached <-chan struct{}) {
	signal := make(chan struct{})
	var once sync.Once
	return func(ctx context.Context, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		once.Do(func() { close(signal) })
		<-ctx.Done()
		return nil
	}, signal
}

// gatedStep is a faux response held until release or cancellation (pi's
// gated): reached closes when the request was sent; cancelled, it returns
// what the provider makes of a cancelled step.
func gatedStep(message *ai.AssistantMessage) (step providers.FauxResponseStep, reached <-chan struct{}, release func()) {
	signal := make(chan struct{})
	gate := make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	return func(ctx context.Context, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		reachedOnce.Do(func() { close(signal) })
		select {
		case <-gate:
		case <-ctx.Done():
			return nil
		}
		copied := *message
		return &copied
	}, signal, func() { releaseOnce.Do(func() { close(gate) }) }
}

// fauxAnswer is a final answer with text, as a message for gatedStep.
func fauxAnswer(text string) *ai.AssistantMessage {
	return providers.FauxAssistantMessage(ai.ContentList{providers.FauxText(text)}, ai.StopStop)
}

// fauxError is a provider error message without content.
func fauxError(message string) *ai.AssistantMessage {
	m := providers.FauxAssistantMessage(nil, ai.StopError)
	m.ErrorMessage = message
	return m
}

// entryByID finds an entry of a conversation.
func entryByID(t *testing.T, conversation *Conversation, id durable.EntryID) durable.EntryRecord {
	t.Helper()
	for _, entry := range chatEntries(t, conversation) {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("no entry %d", id)
	return durable.EntryRecord{}
}

// awaitSignal waits for a channel to close.
func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not reached", what)
	}
}

// fauxText is a faux final answer with text.
func fauxText(text string) providers.FauxResponseStep {
	return providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxText(text)}, ai.StopStop))
}

// interruptedText is the text of a paced faux answer that a test interrupts
// mid-stream. faux sends four characters per token, so pi's 400 characters
// at 200 tokens per second stream in half a second, and a slow runner closed
// or aborted after the answer had finished; 40 000 outlast any test at 20
// tokens per second or more, and the interruption still ends them at once.
func interruptedText(char string) string {
	return strings.Repeat(char, 40_000)
}

// inputDraft is an input submission of plain text.
func inputDraft(text string) SubmissionDraft {
	return SubmissionDraft{Type: durable.SubmissionTypeInput, Input: &InputDraft{Content: TextInput(text)}}
}

// writeDraft is an entry write of kind with optional JSON data.
func writeDraft(kind string, data string) SubmissionDraft {
	entry := &durable.EntryDraft{Kind: kind}
	if data != "" {
		entry.Data = []byte(data)
	}
	return SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: entry}
}

// textOf is the text of the first text content of an entry's first message.
func textOf(entry durable.EntryRecord) string {
	if len(entry.Model) == 0 {
		return ""
	}
	var content ai.ContentList
	switch m := entry.Model[0].(type) {
	case ai.UserMessage:
		content = m.Content
	case ai.AssistantMessage:
		content = m.Content
	case ai.ToolResultMessage:
		content = m.Content
	}
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			return text.Text
		}
	}
	return ""
}
