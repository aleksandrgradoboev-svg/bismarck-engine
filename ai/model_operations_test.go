package ai

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Transliterations of upstream a328aa89a's model-type tests
// (packages/ai/test/model-types.test.ts, images-models.test.ts,
// classifier-models.test.ts, models-runtime.test.ts). Where pi builds a
// provider with an `images` or `classifiers` map — operations the port does
// not have yet — these use an APIByApi chat implementation instead: what they
// pin is how models of every type are listed, merged and filtered, not the
// operations themselves.

func typedChatModel(provider ProviderId, id string) *Model {
	return &Model{ID: id, Name: id, Api: "test-chat", Provider: provider, BaseURL: "https://example.test/v1",
		Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}
}

func typedImageModel(provider ProviderId, id string) *Model {
	return &Model{Type: ModelTypeImage, ID: id, Name: id, Api: "test-images", Provider: provider,
		BaseURL: "https://example.test/v1", Input: []string{"text"}}
}

func typedClassifierModel(provider ProviderId, id string) *Model {
	return &Model{Type: ModelTypeClassifier, ID: id, Name: id, Api: "test-classifier", Provider: provider,
		BaseURL: "https://example.test/v1", Input: []string{"text"}, ContextWindow: 1000}
}

// noAuthConfigured is pi's `{apiKey: {name: "Test", resolve: async () => ({auth: {}})}}`:
// always configured, with no credential to apply.
func noAuthConfigured() ProviderAuth {
	return ProviderAuth{APIKey: &ApiKeyAuth{Name: "Test",
		Resolve: func(context.Context, AuthContext, *Credential) (*AuthResult, error) {
			return &AuthResult{}, nil
		}}}
}

// envKeyAuth is images-models.test.ts testProvider's auth: configured when the
// env var (or a stored key) is set.
func envKeyAuth(envVar string) ProviderAuth {
	return ProviderAuth{APIKey: &ApiKeyAuth{Name: "Test key",
		Resolve: func(_ context.Context, ac AuthContext, credential *Credential) (*AuthResult, error) {
			key := ""
			if credential != nil {
				key = credential.Key
			}
			if key == "" {
				key = ac.Env(envVar)
			}
			if key == "" {
				return nil, nil
			}
			return &AuthResult{Auth: ModelAuth{APIKey: key}, Source: envVar}, nil
		}}}
}

// typedTestProvider is images-models.test.ts testProvider: a chat api for
// "test-chat" models, with an APIByApi entry standing in for pi's images map.
func typedTestProvider(id string, auth ProviderAuth, models ...*Model) Provider {
	return CreateProvider(CreateProviderOptions{
		ID:     id,
		Auth:   auth,
		Models: models,
		APIByApi: map[Api]ProviderStreams{
			"test-chat":   *stubAPI(),
			"test-images": {},
		},
	})
}

func modelIDs(models []*Model) []string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}

func wantIDs(t *testing.T, what string, models []*Model, want ...string) {
	t.Helper()
	if got := modelIDs(models); !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// pi images-models.test.ts "treats models without a type as chat models".
func TestModelsWithoutTypeAreChatModels(t *testing.T) {
	chat := typedChatModel("p", "c")
	image := typedImageModel("p", "i")
	typedChat := *chat
	typedChat.Type = ModelTypeChat

	if chat.Type != "" {
		t.Fatalf("fixture chat model must carry no type, got %q", chat.Type)
	}
	if got := GetModelType(chat); got != ModelTypeChat {
		t.Fatalf("GetModelType(untyped) = %q, want chat", got)
	}
	if got := GetModelType(&typedChat); got != ModelTypeChat {
		t.Fatalf("GetModelType(typed chat) = %q, want chat", got)
	}
	if got := GetModelType(image); got != ModelTypeImage {
		t.Fatalf("GetModelType(image) = %q, want image", got)
	}
	if !IsModelType(chat, ModelTypeChat) || IsModelType(chat, ModelTypeImage) || !IsModelType(image, ModelTypeImage) {
		t.Fatal("IsModelType must read an untyped model as chat and an image model as image")
	}

	// HasApi never matches an image model, even on an equal api string.
	sameAPI := *image
	sameAPI.Api = "test-chat"
	if HasApi(&sameAPI, "test-chat") {
		t.Fatal("HasApi must not match an image model")
	}
	if !HasApi(chat, "test-chat") {
		t.Fatal("HasApi must match an untyped chat model on its api")
	}
}

// pi model-types.test.ts "narrow mixed lists with isModelType".
func TestIsModelTypeNarrowsMixedLists(t *testing.T) {
	typed := typedChatModel("p", "typed")
	typed.Type = ModelTypeChat
	mixed := []*Model{typedChatModel("p", "c"), typed, typedImageModel("p", "i")}
	filter := func(tp ModelType) []*Model {
		var out []*Model
		for _, m := range mixed {
			if IsModelType(m, tp) {
				out = append(out, m)
			}
		}
		return out
	}
	wantIDs(t, "chat", filter(ModelTypeChat), "c", "typed")
	wantIDs(t, "image", filter(ModelTypeImage), "i")
	wantIDs(t, "classifier", filter(ModelTypeClassifier))
}

// handwrittenProvider is a Provider written by hand rather than through
// CreateProvider, and without GetAllModels (pi model-types.test.ts's
// `handwritten` object over a faux provider).
type handwrittenProvider struct {
	models []*Model
}

func (p *handwrittenProvider) ID() string               { return "handwritten" }
func (p *handwrittenProvider) Name() string             { return "Handwritten" }
func (p *handwrittenProvider) BaseURL() string          { return "" }
func (p *handwrittenProvider) Headers() ProviderHeaders { return nil }
func (p *handwrittenProvider) Auth() ProviderAuth       { return noAuthConfigured() }
func (p *handwrittenProvider) GetModels() []*Model      { return p.models }
func (p *handwrittenProvider) DynamicModels() bool      { return false }
func (p *handwrittenProvider) RefreshModels(context.Context, RefreshModelsContext) error {
	return nil
}
func (p *handwrittenProvider) FilterModels(models []*Model, _ *Credential) []*Model { return models }
func (p *handwrittenProvider) Stream(_ context.Context, model *Model, _ TranscriptContext, _ *StreamOptions) *AssistantMessageEventStream {
	s := NewAssistantMessageEventStream()
	msg := &AssistantMessage{Content: ContentList{TextContent{Text: "hi"}}, Api: model.Api,
		Provider: model.Provider, Model: model.ID, StopReason: StopStop}
	s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: msg})
	s.End()
	return s
}
func (p *handwrittenProvider) StreamSimple(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
	return p.Stream(ctx, model, req, &opts.StreamOptions)
}

// pi model-types.test.ts "work through a handwritten provider without
// getAllModels".
func TestHandwrittenProviderWithoutGetAllModels(t *testing.T) {
	provider := &handwrittenProvider{models: []*Model{typedChatModel("handwritten", "faux-1")}}
	models := CreateModels(nil)
	models.SetProvider(provider)

	model := models.GetModel("handwritten", "faux-1")
	if model == nil {
		t.Fatal("GetModel must find the handwritten provider's model")
	}
	if model.Type != "" || GetModelType(model) != ModelTypeChat {
		t.Fatalf("model type = %q (read as %q), want untyped chat", model.Type, GetModelType(model))
	}
	if !HasApi(model, model.Api) {
		t.Fatal("HasApi must match the model's own api")
	}
	typed := *model
	typed.Type = ModelTypeChat
	if !ModelsAreEqual(model, &typed) {
		t.Fatal("an untyped model must equal the same model typed chat")
	}
	if ModelsAreEqual(model, typedImageModel(model.Provider, model.ID)) {
		t.Fatal("a chat model must not equal an image model with the same provider and id")
	}
	wantIDs(t, "GetModelsOfType(chat)", models.GetModelsOfType(ModelTypeChat, "handwritten"), "faux-1")
	wantIDs(t, "GetAllModels", models.GetAllModels("handwritten"), "faux-1")
	wantIDs(t, "GetModelsOfType(image)", models.GetModelsOfType(ModelTypeImage, "handwritten"))

	result := models.Complete(context.Background(), model, Context{Messages: []Message{NewUserText("hi", 0)}}, nil)
	if result.StopReason != StopStop {
		t.Fatalf("complete = %q (%s), want stop", result.StopReason, result.ErrorMessage)
	}
}

// pi images-models.test.ts "lists models without a type as chat models at
// createProvider boundaries".
func TestCreateProviderListsUntypedModelsAsChat(t *testing.T) {
	provider := CreateProvider(CreateProviderOptions{
		ID:     "legacy",
		Auth:   noAuthConfigured(),
		Models: []*Model{typedChatModel("legacy", "static"), typedImageModel("legacy", "static")},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			return []*Model{typedChatModel("legacy", "dynamic")}, nil
		},
		API: stubAPI(),
	})
	models := CreateModels(nil)
	models.SetProvider(provider)

	wantIDs(t, "GetModels before refresh", provider.GetModels(), "static")
	if result := models.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"legacy"}}); len(result.Errors) != 0 {
		t.Fatalf("refresh errors: %v", result.Errors)
	}
	chat := provider.GetModels()
	wantIDs(t, "GetModels after refresh", chat, "static", "dynamic")
	for _, m := range chat {
		if m.Type != "" {
			t.Fatalf("model %s gained type %q; createProvider must not rewrite models", m.ID, m.Type)
		}
	}
	wantIDs(t, "GetModelsOfType(image)", models.GetModelsOfType(ModelTypeImage, "legacy"), "static")
}

// pi images-models.test.ts "lists chat, image, and all models through typed
// accessors".
func TestTypedModelAccessors(t *testing.T) {
	models := CreateModels(nil)
	models.SetProvider(typedTestProvider("p1", noAuthConfigured(),
		typedChatModel("p1", "c1"), typedImageModel("p1", "i1"), typedImageModel("p1", "i2")))
	models.SetProvider(typedTestProvider("p2", noAuthConfigured(), typedImageModel("p2", "i3")))

	wantIDs(t, "GetModels", models.GetModels(""), "c1")
	wantIDs(t, "GetModelsOfType(chat)", models.GetModelsOfType(ModelTypeChat, ""), "c1")
	wantIDs(t, "GetModelsOfType(image)", models.GetModelsOfType(ModelTypeImage, ""), "i1", "i2", "i3")
	wantIDs(t, "GetModelsOfType(image, p1)", models.GetModelsOfType(ModelTypeImage, "p1"), "i1", "i2")
	wantIDs(t, "GetModelsOfType(classifier)", models.GetModelsOfType(ModelTypeClassifier, ""))
	wantIDs(t, "GetAllModels", models.GetAllModels(""), "c1", "i1", "i2", "i3")

	if m := models.GetModel("p1", "c1"); m == nil || m.ID != "c1" {
		t.Fatalf("GetModel(p1, c1) = %+v", m)
	}
	if m := models.GetModel("p1", "i1"); m != nil {
		t.Fatalf("GetModel must not return an image model, got %+v", m)
	}
	if m := models.GetModelOfType(ModelTypeChat, "p1", "c1"); m == nil || m.ID != "c1" {
		t.Fatalf("GetModelOfType(chat, p1, c1) = %+v", m)
	}
	if m := models.GetModelOfType(ModelTypeImage, "p1", "i1"); m == nil || m.ID != "i1" {
		t.Fatalf("GetModelOfType(image, p1, i1) = %+v", m)
	}
	if m := models.GetModelOfType(ModelTypeImage, "p1", "c1"); m != nil {
		t.Fatalf("GetModelOfType(image, p1, c1) = %+v, want nil", m)
	}
}

// pi images-models.test.ts "splits available models by type".
func TestAvailableModelsSplitByType(t *testing.T) {
	models := modelsWithEnv(map[string]string{"KEY": "k"}, nil)
	models.SetProvider(typedTestProvider("p1", envKeyAuth("KEY"), typedChatModel("p1", "c1"), typedImageModel("p1", "i1")))
	models.SetProvider(typedTestProvider("p2", envKeyAuth("MISSING"), typedImageModel("p2", "i2")))
	ctx := context.Background()

	available, err := models.GetAvailable(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAvailable", available, "c1")
	chat, err := models.GetAvailableOfType(ctx, ModelTypeChat, "")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAvailableOfType(chat)", chat, "c1")
	images, err := models.GetAvailableOfType(ctx, ModelTypeImage, "")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAvailableOfType(image)", images, "i1")
	all, err := models.GetAllAvailable(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAllAvailable", all, "c1", "i1")
}

// GetAllAvailable's fallback for a createProvider provider with a chat filter
// and no all-type filter: pi keeps every non-chat model and the chat models
// filterModels keeps (models.ts getAllAvailable). A filterAllModels, when
// given, decides alone.
func TestGetAllAvailableFiltersChatModelsOnly(t *testing.T) {
	keepC1 := func(models []*Model, _ *Credential) []*Model {
		var out []*Model
		for _, m := range models {
			if m.ID == "c1" {
				out = append(out, m)
			}
		}
		return out
	}
	models := CreateModels(nil)
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "filtered", Auth: noAuthConfigured(), API: stubAPI(), FilterModels: keepC1,
		Models: []*Model{typedChatModel("filtered", "c1"), typedChatModel("filtered", "c2"), typedImageModel("filtered", "i1")},
	}))
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "all-filtered", Auth: noAuthConfigured(), API: stubAPI(), FilterModels: keepC1,
		FilterAllModels: func(models []*Model, _ *Credential) []*Model { return models[len(models)-1:] },
		Models:          []*Model{typedChatModel("all-filtered", "c1"), typedImageModel("all-filtered", "i2")},
	}))
	ctx := context.Background()

	filtered, err := models.GetAllAvailable(ctx, "filtered")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAllAvailable(filtered)", filtered, "c1", "i1")
	allFiltered, err := models.GetAllAvailable(ctx, "all-filtered")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAllAvailable(all-filtered)", allFiltered, "i2")
	chat, err := models.GetAvailable(ctx, "all-filtered")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAvailable(all-filtered)", chat, "c1")
}

// keepC1Provider is a handwritten provider, with neither AllModelsLister nor
// AllModelsFilterer unless wrapped, whose chat filter keeps only "c1".
type keepC1Provider struct {
	*handwrittenProvider
	id string
}

func (p keepC1Provider) ID() string { return p.id }
func (p keepC1Provider) FilterModels(models []*Model, _ *Credential) []*Model {
	var out []*Model
	for _, m := range models {
		if m.ID == "c1" {
			out = append(out, m)
		}
	}
	return out
}

// keepC1ListingProvider adds a GetAllModels, still without FilterAllModels.
type keepC1ListingProvider struct {
	keepC1Provider
	all []*Model
}

func (p keepC1ListingProvider) GetAllModels() []*Model { return p.all }

// GetAllAvailable's Models-level fallback, for a provider without
// AllModelsFilterer that has a chat filter (pi models.ts getAllAvailable):
// every non-chat model is kept, and the chat models whose id the filter keeps
// from GetModels. Wants measured by running a328aa89a's createModels under
// node with the same handwritten providers.
func TestGetAllAvailableFallbackForHandwrittenProviders(t *testing.T) {
	models := CreateModels(nil)
	models.SetProvider(keepC1Provider{id: "no-lister", handwrittenProvider: &handwrittenProvider{models: []*Model{
		typedChatModel("no-lister", "c1"), typedChatModel("no-lister", "c2"), typedImageModel("no-lister", "i1"),
	}}})
	models.SetProvider(keepC1ListingProvider{
		keepC1Provider: keepC1Provider{id: "lister", handwrittenProvider: &handwrittenProvider{models: []*Model{
			typedChatModel("lister", "c1"), typedChatModel("lister", "c2"),
		}}},
		all: []*Model{typedChatModel("lister", "c1"), typedChatModel("lister", "c2"), typedImageModel("lister", "i1")},
	})
	ctx := context.Background()
	for _, id := range []string{"no-lister", "lister"} {
		available, err := models.GetAllAvailable(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		wantIDs(t, "GetAllAvailable("+id+")", available, "c1", "i1")
	}
	wantIDs(t, "GetAllModels(no-lister)", models.GetAllModels("no-lister"), "c1", "c2", "i1")
}

// allModelsProvider wraps a provider with a GetAllModels of its own.
type allModelsProvider struct {
	Provider
	all []*Model
}

func (p allModelsProvider) GetAllModels() []*Model { return p.all }

// pi models-runtime.test.ts "keeps chat reads independent from the all-model
// catalog". pi's provider throws from getAllModels, which pi's Models turns
// into []; a Go provider cannot throw, so this pins the independence half: the
// chat reads never consult GetAllModels, and a nil GetAllModels answer stands
// as pi's [] does rather than falling back to GetModels (see allModelsOf for
// why nil is not pi's undefined, on which `??` would fall back).
func TestChatReadsIgnoreGetAllModels(t *testing.T) {
	base := typedTestProvider("chat-only", noAuthConfigured(), typedChatModel("chat-only", "model-a"))
	models := CreateModels(nil)
	models.SetProvider(allModelsProvider{Provider: base, all: nil})

	wantIDs(t, "GetModels", models.GetModels("chat-only"), "model-a")
	if m := models.GetModel("chat-only", "model-a"); m == nil || m.ID != "model-a" {
		t.Fatalf("GetModel = %+v", m)
	}
	available, err := models.GetAvailable(context.Background(), "chat-only")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "GetAvailable", available, "model-a")
	wantIDs(t, "GetAllModels", models.GetAllModels("chat-only"))
}

// pi classifier-models.test.ts "keeps chat and classifier entries with the same
// provider and id separate" (the read half; classify is queued).
func TestChatAndClassifierEntriesWithOneIDStaySeparate(t *testing.T) {
	chat := typedChatModel("test", "shared")
	classifier := typedClassifierModel("test", "shared")
	models := CreateModels(nil)
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "test", Auth: noAuthConfigured(), Models: []*Model{chat, classifier},
		APIByApi: map[Api]ProviderStreams{"test-chat": *stubAPI(), "test-classifier": {}},
	}))

	if listed := models.GetModel("test", "shared"); listed == nil || GetModelType(listed) != ModelTypeChat {
		t.Fatalf("GetModel(test, shared) = %+v, want the chat entry", listed)
	}
	if m := models.GetModelOfType(ModelTypeClassifier, "test", "shared"); m == nil || m.Type != ModelTypeClassifier {
		t.Fatalf("GetModelOfType(classifier) = %+v, want the classifier entry", m)
	}
	if got := models.GetModelsOfType(ModelTypeClassifier, ""); len(got) != 1 || got[0] != classifier {
		t.Fatalf("GetModelsOfType(classifier) = %+v, want exactly the classifier", got)
	}
	if got := models.GetAllModels(""); len(got) != 2 {
		t.Fatalf("GetAllModels = %d models, want 2", len(got))
	}
	available, err := models.GetAvailableOfType(context.Background(), ModelTypeClassifier, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0] != classifier {
		t.Fatalf("GetAvailableOfType(classifier) = %+v, want exactly the classifier", available)
	}
}

// A dynamic model replaces the baseline entry of the same type and id only (pi
// createProvider currentModels matches on getModelType too): a fetched chat
// model sharing a classifier's id is a separate entry.
func TestDynamicOverlayMergesByTypeAndID(t *testing.T) {
	provider := CreateProvider(CreateProviderOptions{
		ID: "dyn", Auth: noAuthConfigured(), API: stubAPI(),
		Models: []*Model{typedClassifierModel("dyn", "shared"), typedChatModel("dyn", "replaced")},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			fresh := typedChatModel("dyn", "replaced")
			fresh.Name = "fresh"
			return []*Model{typedChatModel("dyn", "shared"), fresh}, nil
		},
	})
	models := CreateModels(nil)
	models.SetProvider(provider)
	if result := models.Refresh(context.Background(), nil); len(result.Errors) != 0 {
		t.Fatalf("refresh errors: %v", result.Errors)
	}
	all := models.GetAllModels("dyn")
	wantIDs(t, "GetAllModels", all, "shared", "replaced", "shared")
	if all[0].Type != ModelTypeClassifier || all[1].Name != "fresh" || GetModelType(all[2]) != ModelTypeChat {
		t.Fatalf("merge = %+v; want the classifier kept, the chat model replaced in place, the chat 'shared' appended", all)
	}
	wantIDs(t, "GetModels", models.GetModels("dyn"), "replaced", "shared")
}

// pi model-types.test.ts "stored and fetched models of unknown types are
// dropped instead of failing the refresh".
func TestUnknownModelTypesAreDroppedFromRefresh(t *testing.T) {
	store := NewInMemoryModelsStore()
	embedding := typedChatModel("dyn", "future-embedding")
	embedding.Type = "embedding"
	video := typedImageModel("dyn", "future-video")
	video.Type = "video"
	ctx := context.Background()
	if err := store.Write(ctx, "dyn", ModelsStoreEntry{Models: []*Model{
		typedChatModel("dyn", "stored-chat"), typedImageModel("dyn", "stored-image"), embedding, video,
	}}); err != nil {
		t.Fatal(err)
	}

	var fetched []*Model
	models := CreateModels(&CreateModelsOptions{ModelsStore: store})
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "dyn", Auth: noAuthConfigured(), API: stubAPI(),
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) { return fetched, nil },
	}))

	offline := false
	restored := models.Refresh(ctx, &ModelsRefreshOptions{Providers: []string{"dyn"}, AllowNetwork: &offline})
	if len(restored.Errors) != 0 {
		t.Fatalf("restore errors: %v", restored.Errors)
	}
	wantIDs(t, "GetAllModels after restore", models.GetAllModels("dyn"), "stored-chat", "stored-image")

	fetchedVideo := typedImageModel("dyn", "fetched-video")
	fetchedVideo.Type = "video"
	fetched = []*Model{typedChatModel("dyn", "fetched-chat"), fetchedVideo}
	refreshed := models.Refresh(ctx, &ModelsRefreshOptions{Providers: []string{"dyn"}})
	if len(refreshed.Errors) != 0 {
		t.Fatalf("refresh errors: %v", refreshed.Errors)
	}
	wantIDs(t, "GetAllModels after refresh", models.GetAllModels("dyn"), "fetched-chat")
	entry, err := store.Read(ctx, "dyn")
	if err != nil || entry == nil {
		t.Fatalf("store read = (%+v, %v)", entry, err)
	}
	wantIDs(t, "persisted models", entry.Models, "fetched-chat")
}

// pi images-models.test.ts "supports dynamic providers listing image models via
// refresh": fetched models of every known type are listed and persisted, in
// fetch order.
func TestDynamicProviderListsFetchedModelsOfEveryType(t *testing.T) {
	store := NewInMemoryModelsStore()
	fetches := 0
	models := CreateModels(&CreateModelsOptions{ModelsStore: store})
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "dyn", Auth: noAuthConfigured(),
		APIByApi: map[Api]ProviderStreams{"test-images": {}},
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) {
			fetches++
			return []*Model{typedImageModel("dyn", "listed"), typedChatModel("dyn", "chat")}, nil
		},
	}))

	wantIDs(t, "GetAllModels before refresh", models.GetAllModels("dyn"))
	if result := models.Refresh(context.Background(), &ModelsRefreshOptions{Providers: []string{"dyn"}}); len(result.Errors) != 0 {
		t.Fatalf("refresh errors: %v", result.Errors)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	if models.GetModelOfType(ModelTypeImage, "dyn", "listed") == nil {
		t.Fatal("the fetched image model must be listed")
	}
	if models.GetModel("dyn", "chat") == nil {
		t.Fatal("the fetched chat model must be listed")
	}
	entry, err := store.Read(context.Background(), "dyn")
	if err != nil || entry == nil {
		t.Fatalf("store read = (%+v, %v)", entry, err)
	}
	wantIDs(t, "persisted models", entry.Models, "listed", "chat")
}

// pi images-models.test.ts "rejects image models at the stream entry points at
// runtime", widened to every chat entry point as pi's coding-agent
// model-runtime-images.test.ts does: each fails with pi's exact message before
// any provider lookup or dispatch.
func TestChatEntryPointsRejectNonChatModels(t *testing.T) {
	dispatches := 0
	reject := func(model *Model) *AssistantMessageEventStream {
		dispatches++
		return ErrorStream(model, errors.New("image reached chat adapter"))
	}
	models := CreateModels(nil)
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "p1", Auth: noAuthConfigured(), Models: []*Model{typedImageModel("p1", "model-a")},
		API: &ProviderStreams{
			Stream: func(_ context.Context, m *Model, _ TranscriptContext, _ *StreamOptions) *AssistantMessageEventStream {
				return reject(m)
			},
			StreamSimple: func(_ context.Context, m *Model, _ TranscriptContext, _ *SimpleStreamOptions) *AssistantMessageEventStream {
				return reject(m)
			},
			FetchDeferred: func(_ context.Context, m *Model, _ DeferredHandle, _ *DeferredFetchOptions) *AssistantMessageEventStream {
				return reject(m)
			},
			CancelDeferred: func(context.Context, *Model, DeferredHandle, *DeferredCancelOptions) error {
				dispatches++
				return errors.New("image reached chat adapter")
			},
		},
	}))
	image := models.GetModelOfType(ModelTypeImage, "p1", "model-a")
	if image == nil {
		t.Fatal("fixture: the image model must be listed")
	}
	const want = "Model p1/model-a is not a chat model"
	ctx := context.Background()
	req := Context{}
	handle := DeferredHandle{Provider: "p1", ModelID: image.ID, Api: image.Api, ID: "response-1"}

	for name, result := range map[string]*AssistantMessage{
		"Stream":         models.Stream(ctx, image, req, nil).Result(),
		"Complete":       models.Complete(ctx, image, req, nil),
		"StreamSimple":   models.StreamSimple(ctx, image, req, nil).Result(),
		"CompleteSimple": models.CompleteSimple(ctx, image, req, nil),
		"StreamDeferred": models.StreamDeferred(ctx, image, handle, nil).Result(),
		"FetchDeferred":  models.FetchDeferred(ctx, image, handle, nil),
	} {
		if result.StopReason != StopError || result.ErrorMessage != want {
			t.Errorf("%s = (%q, %q), want (error, %q)", name, result.StopReason, result.ErrorMessage, want)
		}
	}
	err := models.CancelDeferred(ctx, image, handle, nil)
	var me *ModelsError
	if !errors.As(err, &me) || me.Code != ErrProvider || err.Error() != want {
		t.Errorf("CancelDeferred = %v, want the provider ModelsError %q", err, want)
	}
	if dispatches != 0 {
		t.Errorf("dispatches = %d, want none: the assertion runs before the provider", dispatches)
	}

	// The assertion precedes the provider lookup: an image model of an unknown
	// provider is still "not a chat model", not an unknown provider.
	ghost := typedImageModel("ghost", "m")
	if got := models.StreamSimple(ctx, ghost, req, nil).Result().ErrorMessage; got != "Model ghost/m is not a chat model" {
		t.Errorf("unknown-provider image model = %q, want the chat assertion", got)
	}
	if err := models.CancelDeferred(ctx, ghost, handle, nil); err == nil || err.Error() != "Model ghost/m is not a chat model" {
		t.Errorf("unknown-provider image cancel = %v, want the chat assertion", err)
	}
}

// pi images-models.test.ts "requires at least one concrete operation
// implementation": no implementation, and an api whose functions are all
// unset (pi's `api: {}`), both throw; pi's images/classifiers arms arrive with
// those operations.
func TestCreateProviderRequiresAnImplementation(t *testing.T) {
	const message = `Provider empty: at least one of "api", "images", or "classifiers" is required.`
	for name, input := range map[string]CreateProviderOptions{
		"no implementation": {ID: "empty", Auth: noAuthConfigured()},
		"empty api":         {ID: "empty", Auth: noAuthConfigured(), API: &ProviderStreams{}},
		"empty api map":     {ID: "empty", Auth: noAuthConfigured(), APIByApi: map[Api]ProviderStreams{}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if got := recover(); got != message {
					t.Fatalf("panic = %v, want %q", got, message)
				}
			}()
			CreateProvider(input)
		})
	}
}

// An api whose functions are all unset is no implementation, so it does not
// shadow an APIByApi map given beside it.
func TestEmptyAPIDoesNotShadowAPIByApi(t *testing.T) {
	provider := CreateProvider(CreateProviderOptions{
		ID: "mixed", Auth: noAuthConfigured(), API: &ProviderStreams{},
		APIByApi: map[Api]ProviderStreams{"test-chat": *stubAPI()},
	})
	got := provider.Stream(context.Background(), typedChatModel("mixed", "c"), TranscriptContext{}, nil).Result()
	if !strings.Contains(got.ErrorMessage, "stub test api") {
		t.Fatalf("stream = %q, want the APIByApi implementation to run", got.ErrorMessage)
	}
}

// piStoredTypedModels is a stored entry as pi writes one: the model factories
// of upstream 8676a0dcd's model-types.test.ts and classifier-models.test.ts,
// with an explicit null type and a type this version does not know, passed
// through JSON.stringify as coding-agent's models-store.ts writes an entry.
const piStoredTypedModels = `{"models":[{"id":"untyped-chat","name":"untyped-chat","api":"test-chat","provider":"dyn","baseUrl":"https://example.test/v1","reasoning":false,"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":1000,"maxTokens":100},{"id":"null-typed-chat","name":"null-typed-chat","api":"test-chat","provider":"dyn","baseUrl":"https://example.test/v1","reasoning":false,"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":1000,"maxTokens":100,"type":null},{"type":"image","id":"stored-image","name":"stored-image","api":"test-images","provider":"dyn","baseUrl":"https://example.test/v1","input":["text"],"output":["image"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}},{"type":"classifier","id":"stored-classifier","name":"stored-classifier","api":"test-classifier","provider":"dyn","baseUrl":"https://example.test/v1","input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":1000},{"type":"video","id":"future-video","name":"future-video","api":"test-images","provider":"dyn","baseUrl":"https://example.test/v1","input":["text"],"output":["image"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}]}`

// A pi-written entry decodes with its model types, which is all Model's
// "type" JSON key decides: nothing else reads the JSON, so a renamed tag
// would restore a stored image model as a chat model. Restored through
// Refresh, the entry lists exactly what pi's own createModels lists for the
// same JSON (run under node against 8676a0dcd src): getModels
// untyped-chat,null-typed-chat; getAllModels adds stored-image and
// stored-classifier; the video model is dropped.
func TestPiStoredModelTypesDecode(t *testing.T) {
	var entry ModelsStoreEntry
	if err := json.Unmarshal([]byte(piStoredTypedModels), &entry); err != nil {
		t.Fatal(err)
	}
	wantTypes := []ModelType{"", "", ModelTypeImage, ModelTypeClassifier, "video"}
	if len(entry.Models) != len(wantTypes) {
		t.Fatalf("decoded %d models, want %d", len(entry.Models), len(wantTypes))
	}
	for i, m := range entry.Models {
		if m.Type != wantTypes[i] {
			t.Fatalf("%s: Type = %q, want %q", m.ID, m.Type, wantTypes[i])
		}
	}
	// An absent and a null type both read as chat, as pi's `model.type ?? "chat"`.
	for _, m := range entry.Models[:2] {
		if GetModelType(m) != ModelTypeChat {
			t.Fatalf("%s: GetModelType = %q, want chat", m.ID, GetModelType(m))
		}
	}

	ctx := context.Background()
	store := NewInMemoryModelsStore()
	if err := store.Write(ctx, "dyn", entry); err != nil {
		t.Fatal(err)
	}
	models := CreateModels(&CreateModelsOptions{ModelsStore: store})
	models.SetProvider(CreateProvider(CreateProviderOptions{
		ID: "dyn", Auth: noAuthConfigured(), API: stubAPI(),
		FetchModels: func(context.Context, RefreshModelsContext) ([]*Model, error) { return nil, nil },
	}))
	offline := false
	if result := models.Refresh(ctx, &ModelsRefreshOptions{Providers: []string{"dyn"}, AllowNetwork: &offline}); len(result.Errors) != 0 {
		t.Fatalf("restore errors: %v", result.Errors)
	}
	wantIDs(t, "GetModels", models.GetModels("dyn"), "untyped-chat", "null-typed-chat")
	wantIDs(t, "GetAllModels", models.GetAllModels("dyn"), "untyped-chat", "null-typed-chat", "stored-image", "stored-classifier")
	wantIDs(t, "image models", models.GetModelsOfType(ModelTypeImage, "dyn"), "stored-image")
	wantIDs(t, "classifier models", models.GetModelsOfType(ModelTypeClassifier, "dyn"), "stored-classifier")
}
