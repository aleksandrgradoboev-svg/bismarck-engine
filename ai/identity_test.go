package ai

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"

	engine "github.com/aleksandrgradoboev-svg/bismarck-engine"
)

// Ported from pi packages/ai/test/identity.test.ts (upstream 04a93bad), the
// "client identity" block; the request cases need the providers and are in
// ai/providers/identity_test.go. Adapted: the version is the engine's (pi
// checks PI_AI_VERSION against package.json; here engine.Version is the
// source, see version_test.go), and the runtime is Go's.

var acmeAttribution = ClientAttribution{Name: "acme", URL: "https://acme.example"}

func attributionOf(a ClientAttribution) *ClientIdentity { return &ClientIdentity{Attribution: &a} }

func identityTarget(provider, baseURL string) *Model {
	return &Model{Provider: provider, BaseURL: baseURL}
}

func TestFormatUserAgent(t *testing.T) {
	platform, arch := runtime.GOOS, runtime.GOARCH
	if platform == "windows" {
		platform = "win32"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	goRuntime := "go/" + goVersion()
	if strings.ContainsAny(goRuntime, " ") || goRuntime == "go/unknown" {
		t.Fatalf("go runtime token = %q", goRuntime)
	}
	if got, want := FormatUserAgent(nil), "pi/"+engine.Version()+" ("+platform+"; "+goRuntime+"; "+arch+")"; got != want {
		t.Errorf("FormatUserAgent() = %q, want %q", got, want)
	}
	if got, want := FormatUserAgent(&ClientIdentity{Version: "9.9.9"}), "pi/9.9.9 ("+platform+"; "+goRuntime+"; "+arch+")"; got != want {
		t.Errorf("version 9.9.9 = %q, want %q", got, want)
	}
	short := UserAgentShort
	if got := FormatUserAgent(&ClientIdentity{UserAgent: &short, Version: "9.9.9"}); got != "pi" {
		t.Errorf("short = %q, want pi", got)
	}
	custom := CustomUserAgent("acme/1.0")
	if got := FormatUserAgent(&ClientIdentity{UserAgent: &custom}); got != "acme/1.0" {
		t.Errorf("custom = %q, want acme/1.0", got)
	}
}

func TestMergeClientIdentityFieldByField(t *testing.T) {
	if got := MergeClientIdentity(nil, nil); got != nil {
		t.Fatalf("merge of nothing = %+v, want nil", got)
	}
	short := UserAgentShort
	pi := PIAttribution
	got := MergeClientIdentity(&ClientIdentity{Attribution: &pi, UserAgent: &short}, &ClientIdentity{Version: "1.0.0"})
	if want := (&ClientIdentity{Attribution: &PIAttribution, UserAgent: &short, Version: "1.0.0"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %+v, want %+v", got, want)
	}
	got = MergeClientIdentity(&ClientIdentity{Attribution: &pi}, &ClientIdentity{NoAttribution: true})
	if want := (&ClientIdentity{NoAttribution: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge with null attribution = %+v, want %+v", got, want)
	}
	// Added: a later attribution turns attribution back on, and the result
	// shares nothing with its sources.
	source := &ClientIdentity{Attribution: &ClientAttribution{Name: "a", URL: "u", Categories: []string{"x"}}}
	got = MergeClientIdentity(&ClientIdentity{NoAttribution: true}, source)
	if got.NoAttribution || got.Attribution == nil || got.Attribution.Name != "a" {
		t.Fatalf("attribution after null = %+v", got)
	}
	got.Attribution.Categories[0] = "changed"
	if source.Attribution.Categories[0] != "x" {
		t.Fatal("merge shares categories with its source")
	}
}

func TestAttributionIsOffUnlessEnabled(t *testing.T) {
	openRouter := identityTarget("openrouter", "https://openrouter.ai/api/v1")
	if got := AttributionHeaders(openRouter, nil); len(got) != 0 {
		t.Errorf("no identity = %v", got)
	}
	if got := AttributionHeaders(openRouter, &ClientIdentity{NoAttribution: true}); len(got) != 0 {
		t.Errorf("null attribution = %v", got)
	}
	if got, want := IdentityHeaders(openRouter, nil), (ProviderHeaders{"User-Agent": HeaderValue(FormatUserAgent(nil))}); !reflect.DeepEqual(got, want) {
		t.Errorf("identity headers = %v, want %v", got, want)
	}
}

func TestAttributionOfOpenRouterRequests(t *testing.T) {
	openRouter := identityTarget("openrouter", "https://openrouter.ai/api/v1")
	for _, tc := range []struct {
		name        string
		attribution ClientAttribution
		want        map[string]string
	}{
		{"pi", PIAttribution, map[string]string{"HTTP-Referer": "https://pi.dev", "X-OpenRouter-Title": "pi", "X-OpenRouter-Categories": "cli-agent"}},
		{"categories", ClientAttribution{Name: "acme", URL: "https://acme.example", Categories: []string{"cli-agent", "cloud-agent"}},
			map[string]string{"HTTP-Referer": "https://acme.example", "X-OpenRouter-Title": "acme", "X-OpenRouter-Categories": "cli-agent,cloud-agent"}},
		{"no categories", ClientAttribution{Name: "acme", URL: "https://acme.example", Categories: []string{}},
			map[string]string{"HTTP-Referer": "https://acme.example", "X-OpenRouter-Title": "acme"}},
	} {
		if got := AttributionHeaders(openRouter, attributionOf(tc.attribution)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// Custom providers routed through OpenRouter, legacy base URLs that are
	// not valid URLs included.
	if got := AttributionHeaders(identityTarget("custom", "not-a-url-openrouter.ai"), attributionOf(PIAttribution)); got["X-OpenRouter-Title"] != "pi" {
		t.Errorf("legacy OpenRouter base URL = %v", got)
	}
}

func TestAttributionOfNvidiaAndOpenCodeRequests(t *testing.T) {
	for _, tc := range []struct {
		target      *Model
		attribution ClientAttribution
		want        map[string]string
	}{
		{identityTarget("nvidia", "https://example.test/v1"), PIAttribution, map[string]string{"X-BILLING-INVOKE-ORIGIN": "Pi"}},
		{identityTarget("custom-nim", "https://integrate.api.nvidia.com/v1"), acmeAttribution, map[string]string{"X-BILLING-INVOKE-ORIGIN": "Acme"}},
		{identityTarget("opencode", "https://opencode.ai/zen/v1"), PIAttribution, map[string]string{"x-opencode-client": "pi"}},
		{identityTarget("custom", "https://opencode.ai/zen/v1"), acmeAttribution, map[string]string{"x-opencode-client": "acme"}},
		// Added: a name outside ASCII is upper-cased as JavaScript does.
		{identityTarget("nvidia", ""), ClientAttribution{Name: "ёлка", URL: "u"}, map[string]string{"X-BILLING-INVOKE-ORIGIN": "Ёлка"}},
	} {
		if got := AttributionHeaders(tc.target, attributionOf(tc.attribution)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s: %v, want %v", tc.target.Provider, tc.target.BaseURL, got, tc.want)
		}
	}
}

func TestNvidiaModelsThroughGatewaysAreNotAttributedToNvidia(t *testing.T) {
	if got := AttributionHeaders(identityTarget("openrouter", "https://openrouter.ai/api/v1"), attributionOf(PIAttribution)); got["X-BILLING-INVOKE-ORIGIN"] != "" {
		t.Errorf("OpenRouter = %v", got)
	}
	if got := AttributionHeaders(identityTarget("vercel-ai-gateway", "https://ai-gateway.vercel.sh/v1"), attributionOf(PIAttribution)); len(got) != 0 {
		t.Errorf("Vercel AI Gateway = %v", got)
	}
}

// Added: OpenCode's session header goes to OpenCode targets with a session.
func TestOpenCodeSessionHeaders(t *testing.T) {
	if got := OpenCodeSessionHeaders(identityTarget("openai", ""), "s"); got != nil {
		t.Errorf("openai = %v", got)
	}
	if got := OpenCodeSessionHeaders(identityTarget("opencode", ""), ""); got != nil {
		t.Errorf("no session = %v", got)
	}
	for _, target := range []*Model{identityTarget("opencode-go", ""), identityTarget("custom", "https://opencode.ai/zen/v1")} {
		if got := OpenCodeSessionHeaders(target, "s"); got["x-opencode-session"] == nil || *got["x-opencode-session"] != "s" {
			t.Errorf("%s = %v", target.Provider, got)
		}
	}
}

// Added: an identity round-trips through pi's plain JSON form.
func TestClientIdentityJSON(t *testing.T) {
	short, custom := UserAgentShort, CustomUserAgent("acme/1.0")
	for _, tc := range []struct {
		identity ClientIdentity
		json     string
	}{
		{ClientIdentity{}, `{}`},
		{ClientIdentity{UserAgent: &short, NoAttribution: true}, `{"userAgent":"short","attribution":null}`},
		{ClientIdentity{UserAgent: &custom, Attribution: &PIAttribution, Version: "1.2.3"},
			`{"userAgent":{"custom":"acme/1.0"},"attribution":{"name":"pi","url":"https://pi.dev","categories":["cli-agent"]},"version":"1.2.3"}`},
	} {
		data, err := json.Marshal(tc.identity)
		if err != nil || string(data) != tc.json {
			t.Errorf("marshal %+v = %s, %v; want %s", tc.identity, data, err, tc.json)
		}
		var back ClientIdentity
		if err := json.Unmarshal([]byte(tc.json), &back); err != nil || !reflect.DeepEqual(back, tc.identity) {
			t.Errorf("unmarshal %s = %+v, %v; want %+v", tc.json, back, err, tc.identity)
		}
	}
	var detailed UserAgentMode
	if err := json.Unmarshal([]byte(`"detailed"`), &detailed); err != nil || detailed != UserAgentDetailed {
		t.Errorf("detailed = %+v, %v", detailed, err)
	}
	if err := json.Unmarshal([]byte(`"long"`), &detailed); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

// Added: WithClientIdentity forwards the merged identity on every request
// path, keeps a MutableModels mutable, and leaves a plain Models immutable.
func TestWithClientIdentityAppliesItsIdentityOnEveryRequestPath(t *testing.T) {
	var seen []*ClientIdentity
	m := modelsWithEnv(map[string]string{"K": "key"}, &CreateModelsOptions{Identity: &ClientIdentity{Version: "models"}})
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     "p",
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth("p", "K")},
		Models: []*Model{{Provider: "p", ID: "m", Api: "identity-test"}},
		API: ptrStreams(ProviderStreams{
			Stream: func(_ context.Context, model *Model, _ TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
				seen = append(seen, opts.Identity)
				return ErrorStream(model, context.Canceled)
			},
			StreamSimple: func(_ context.Context, model *Model, _ TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
				seen = append(seen, opts.Identity)
				return ErrorStream(model, context.Canceled)
			},
			FetchDeferred: func(_ context.Context, model *Model, _ DeferredHandle, opts *DeferredFetchOptions) *AssistantMessageEventStream {
				seen = append(seen, opts.Identity)
				return ErrorStream(model, context.Canceled)
			},
			CancelDeferred: func(_ context.Context, _ *Model, _ DeferredHandle, opts *DeferredCancelOptions) error {
				seen = append(seen, opts.Identity)
				return nil
			},
		}),
	}))
	wrapped := WithClientIdentity(m, ClientIdentity{Attribution: &acmeAttribution})
	model := m.GetModel("p", "m")
	handle := DeferredHandle{Provider: "p", ModelID: "m", Api: "identity-test", ID: "h"}
	ctx := context.Background()
	wrapped.Complete(ctx, model, Context{}, nil)
	wrapped.CompleteSimple(ctx, model, Context{}, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: ProviderRequestOptions{Identity: &ClientIdentity{NoAttribution: true}}}}})
	wrapped.FetchDeferred(ctx, model, handle, nil)
	if err := wrapped.CancelDeferred(ctx, model, handle, nil); err != nil {
		t.Fatal(err)
	}
	want := []*ClientIdentity{
		{Attribution: &acmeAttribution, Version: "models"},
		{NoAttribution: true, Version: "models"},
		{Attribution: &acmeAttribution, Version: "models"},
		{Attribution: &acmeAttribution, Version: "models"},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("identities = %+v, want %+v", seen, want)
	}
	mutable, ok := wrapped.(MutableModels)
	if !ok {
		t.Fatal("a MutableModels did not stay mutable")
	}
	mutable.DeleteProvider("p")
	if m.GetProvider("p") != nil {
		t.Fatal("DeleteProvider did not reach the wrapped Models")
	}
	if _, ok := WithClientIdentity(identityModels{Models: m}, ClientIdentity{}).(MutableModels); ok {
		t.Fatal("a plain Models became mutable")
	}
}

// methodRecorder records which request method WithClientIdentity called.
type methodRecorder struct {
	Models
	calls []string
}

func (r *methodRecorder) record(name string, identity *ClientIdentity) {
	if identity == nil || identity.Attribution == nil || identity.Attribution.Name != "acme" {
		panic(name + " got no wrapper identity")
	}
	r.calls = append(r.calls, name)
}

func (r *methodRecorder) Stream(_ context.Context, model *Model, _ Context, o *ModelsStreamOptions) *AssistantMessageEventStream {
	r.record("Stream", o.Identity)
	return ErrorStream(model, context.Canceled)
}

func (r *methodRecorder) Complete(_ context.Context, _ *Model, _ Context, o *ModelsStreamOptions) *AssistantMessage {
	r.record("Complete", o.Identity)
	return &AssistantMessage{}
}

func (r *methodRecorder) StreamSimple(_ context.Context, model *Model, _ Context, o *ModelsSimpleStreamOptions) *AssistantMessageEventStream {
	r.record("StreamSimple", o.Identity)
	return ErrorStream(model, context.Canceled)
}

func (r *methodRecorder) CompleteSimple(_ context.Context, _ *Model, _ Context, o *ModelsSimpleStreamOptions) *AssistantMessage {
	r.record("CompleteSimple", o.Identity)
	return &AssistantMessage{}
}

func (r *methodRecorder) StreamDeferred(_ context.Context, model *Model, _ DeferredHandle, o *ModelsDeferredFetchOptions) *AssistantMessageEventStream {
	r.record("StreamDeferred", o.Identity)
	return ErrorStream(model, context.Canceled)
}

func (r *methodRecorder) FetchDeferred(_ context.Context, _ *Model, _ DeferredHandle, o *ModelsDeferredFetchOptions) *AssistantMessage {
	r.record("FetchDeferred", o.Identity)
	return &AssistantMessage{}
}

func (r *methodRecorder) CancelDeferred(_ context.Context, _ *Model, _ DeferredHandle, o *ModelsDeferredCancelOptions) error {
	r.record("CancelDeferred", o.Identity)
	return nil
}

// Added: each request method of the wrapper calls the same method of the
// Models it wraps (pi's wrapper forwards method to method), so an override of
// one of them, a host's Complete say, is not bypassed.
func TestWithClientIdentityCallsTheSameMethodOfTheWrappedModels(t *testing.T) {
	recorder := &methodRecorder{}
	wrapped := WithClientIdentity(recorder, ClientIdentity{Attribution: &acmeAttribution})
	ctx, model, handle := context.Background(), &Model{}, DeferredHandle{}
	wrapped.Stream(ctx, model, Context{}, nil)
	wrapped.Complete(ctx, model, Context{}, nil)
	wrapped.StreamSimple(ctx, model, Context{}, nil)
	wrapped.CompleteSimple(ctx, model, Context{}, nil)
	wrapped.StreamDeferred(ctx, model, handle, nil)
	wrapped.FetchDeferred(ctx, model, handle, nil)
	_ = wrapped.CancelDeferred(ctx, model, handle, nil)
	want := []string{"Stream", "Complete", "StreamSimple", "CompleteSimple", "StreamDeferred", "FetchDeferred", "CancelDeferred"}
	if !reflect.DeepEqual(recorder.calls, want) {
		t.Fatalf("calls = %v, want %v", recorder.calls, want)
	}
}
