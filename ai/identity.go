package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	engine "github.com/aleksandrgradoboev-svg/bismarck-engine"
)

// Client identity (pi utils/identity.ts, upstream adc5fa9f with the 04a93bad
// follow-ups): how requests identify the calling app to providers — the
// User-Agent and the provider attribution headers. pi-ai's defaults are a
// detailed User-Agent and no attribution; agent loops, the durable Harness and
// the coding agent choose theirs (agent.DefaultAgentIdentity,
// harness.DefaultHarnessIdentity, coding's ClientIdentity).

// UserAgentMode chooses the User-Agent sent to providers (pi UserAgentMode):
// UserAgentDetailed is `pi/<version> (<platform>; go/<go version>; <arch>)`,
// UserAgentShort is `pi`, and CustomUserAgent sends the value given. The zero
// value is UserAgentDetailed.
type UserAgentMode struct {
	short  bool
	custom *string
}

var (
	UserAgentDetailed = UserAgentMode{}
	UserAgentShort    = UserAgentMode{short: true}
)

// CustomUserAgent is a User-Agent mode that sends value as it is (pi's
// `{ custom }`).
func CustomUserAgent(value string) UserAgentMode { return UserAgentMode{custom: &value} }

// MarshalJSON writes pi's form: "detailed", "short", or {"custom": value}.
func (m UserAgentMode) MarshalJSON() ([]byte, error) {
	switch {
	case m.custom != nil:
		return json.Marshal(struct {
			Custom string `json:"custom"`
		}{*m.custom})
	case m.short:
		return []byte(`"short"`), nil
	}
	return []byte(`"detailed"`), nil
}

// UnmarshalJSON reads pi's form.
func (m *UserAgentMode) UnmarshalJSON(data []byte) error {
	var mode string
	if err := json.Unmarshal(data, &mode); err == nil {
		switch mode {
		case "short":
			*m = UserAgentShort
			return nil
		case "detailed":
			*m = UserAgentDetailed
			return nil
		}
		return errors.New("ai: unknown user agent mode " + mode)
	}
	var custom struct {
		Custom *string `json:"custom"`
	}
	if err := json.Unmarshal(data, &custom); err != nil || custom.Custom == nil {
		return errors.New("ai: a user agent mode is \"short\", \"detailed\" or {\"custom\": string}")
	}
	*m = CustomUserAgent(*custom.Custom)
	return nil
}

// ClientAttribution is who requests are attributed to (pi
// ClientAttribution); AttributionHeaders maps the fields to each provider's
// headers.
type ClientAttribution struct {
	// Name is the app name: OpenRouter app title, NVIDIA invoke origin (first
	// letter upper-cased), OpenCode client.
	Name string `json:"name"`
	// URL is the app URL: OpenRouter HTTP-Referer, the app's identity in
	// OpenRouter's rankings, grouped by origin.
	URL string `json:"url"`
	// Categories are the app categories, e.g. ["cli-agent"]: OpenRouter
	// X-OpenRouter-Categories, comma-separated.
	Categories []string `json:"categories,omitempty"`
}

// PIAttribution is pi's attribution (pi PI_ATTRIBUTION). In pi, agent loops,
// the durable Harness and the coding agent use it by default; here none does
// unless asked (see agent.DefaultAgentIdentity).
var PIAttribution = ClientAttribution{Name: "pi", URL: "https://pi.dev", Categories: []string{"cli-agent"}}

// ClientIdentity is how pi-ai identifies the calling app to providers (pi
// ClientIdentity). Every field is optional; absent fields use pi-ai's
// defaults: a detailed User-Agent with the engine's version and no
// attribution headers.
type ClientIdentity struct {
	// UserAgent is the User-Agent mode; nil is UserAgentDetailed.
	UserAgent *UserAgentMode
	// Attribution is who requests are attributed to; nil is absent.
	Attribution *ClientAttribution
	// NoAttribution is pi's `attribution: null`: no attribution headers, also
	// over attribution a merged identity set. It outranks Attribution in the
	// same identity.
	NoAttribution bool
	// Version is the version in the detailed User-Agent; empty is the
	// engine's.
	Version string
}

// MarshalJSON writes pi's plain JSON form, so an identity can be persisted:
// absent fields are left out and NoAttribution is `"attribution": null`.
func (c ClientIdentity) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	field := func(name string, value any) error {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		b.WriteString(`"` + name + `":`)
		b.Write(data)
		return nil
	}
	if c.UserAgent != nil {
		if err := field("userAgent", c.UserAgent); err != nil {
			return nil, err
		}
	}
	switch {
	case c.NoAttribution:
		if err := field("attribution", nil); err != nil {
			return nil, err
		}
	case c.Attribution != nil:
		if err := field("attribution", c.Attribution); err != nil {
			return nil, err
		}
	}
	if c.Version != "" {
		if err := field("version", c.Version); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads pi's plain JSON form.
func (c *ClientIdentity) UnmarshalJSON(data []byte) error {
	var raw struct {
		UserAgent   *UserAgentMode  `json:"userAgent"`
		Attribution json.RawMessage `json:"attribution"`
		Version     string          `json:"version"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = ClientIdentity{UserAgent: raw.UserAgent, Version: raw.Version}
	switch trimmed := bytes.TrimSpace(raw.Attribution); {
	case len(trimmed) == 0:
	case bytes.Equal(trimmed, []byte("null")):
		c.NoAttribution = true
	default:
		var attribution ClientAttribution
		if err := json.Unmarshal(trimmed, &attribution); err != nil {
			return err
		}
		c.Attribution = &attribution
	}
	return nil
}

// MergeClientIdentity merges identities field by field; later sources win and
// absent fields are skipped (pi mergeClientIdentity). It returns nil when every
// source is nil, and never one of the sources itself.
func MergeClientIdentity(sources ...*ClientIdentity) *ClientIdentity {
	var merged *ClientIdentity
	for _, source := range sources {
		if source == nil {
			continue
		}
		if merged == nil {
			merged = &ClientIdentity{}
		}
		if source.UserAgent != nil {
			mode := *source.UserAgent
			merged.UserAgent = &mode
		}
		switch {
		case source.NoAttribution:
			merged.Attribution, merged.NoAttribution = nil, true
		case source.Attribution != nil:
			attribution := *source.Attribution
			attribution.Categories = append([]string(nil), source.Attribution.Categories...)
			merged.Attribution, merged.NoAttribution = &attribution, false
		}
		if source.Version != "" {
			merged.Version = source.Version
		}
	}
	return merged
}

// ClientVersion is the version in the detailed User-Agent (pi
// getClientVersion): the identity's, or the engine's (pi: pi-ai's).
func ClientVersion(identity *ClientIdentity) string {
	if identity != nil && identity.Version != "" {
		return identity.Version
	}
	return engine.Version()
}

// FormatUserAgent is the User-Agent value of an identity (pi
// formatUserAgent). The detailed form names the Go runtime where pi names
// node or bun: `pi/<version> (<platform>; go/<go version>; <arch>)`, with
// Node's platform and arch names.
func FormatUserAgent(identity *ClientIdentity) string {
	mode := UserAgentDetailed
	if identity != nil && identity.UserAgent != nil {
		mode = *identity.UserAgent
	}
	switch {
	case mode.custom != nil:
		return *mode.custom
	case mode.short:
		return "pi"
	}
	return "pi/" + ClientVersion(identity) + " (" + nodePlatform() + "; go/" + goVersion() + "; " + nodeArch() + ")"
}

// goVersion is the Go release without its "go" prefix, "1.26.7". A
// development toolchain reports "devel go1.27-<hash> <date>"; its "go" word
// gives "1.27-<hash>", so the User-Agent never carries the spaces.
func goVersion() string {
	for _, field := range strings.Fields(runtime.Version()) {
		if strings.HasPrefix(field, "go") {
			return strings.TrimPrefix(field, "go")
		}
	}
	return "unknown"
}

// nodePlatform maps runtime.GOOS to Node's process.platform name. They agree
// everywhere the engine builds except Windows.
func nodePlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// nodeArch maps runtime.GOARCH to Node's process.arch name (documented
// values: arm, arm64, ia32, loong64, mips, mipsel, ppc64, riscv64, s390x,
// x64).
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "mipsle":
		return "mipsel"
	case "ppc64le":
		return "ppc64"
	}
	return runtime.GOARCH
}

const (
	nvidiaNIMHost = "integrate.api.nvidia.com"
	openCodeHost  = "opencode.ai"
)

// matchesHost reports whether baseURL's hostname is expectedHost; a base URL
// that does not parse matches nothing (pi matchesHost).
func matchesHost(baseURL, expectedHost string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return u.Hostname() == expectedHost
}

// isOpenRouterTarget matches the base URL by substring, as pi does.
func isOpenRouterTarget(model *Model) bool {
	return model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai")
}

func isNvidiaNIMTarget(model *Model) bool {
	return model.Provider == "nvidia" || matchesHost(model.BaseURL, nvidiaNIMHost)
}

// IsOpenCodeTarget reports whether a request to model goes to OpenCode: an
// OpenCode provider, or any provider whose base URL is opencode.ai (pi
// isOpenCode).
func IsOpenCodeTarget(model *Model) bool {
	return model.Provider == "opencode" || model.Provider == "opencode-go" || matchesHost(model.BaseURL, openCodeHost)
}

// AttributionHeaders are the provider attribution headers of an identity for
// a request to model, its provider and base URL (pi getAttributionHeaders):
// nil when attribution is off.
func AttributionHeaders(model *Model, identity *ClientIdentity) map[string]string {
	if identity == nil || identity.NoAttribution || identity.Attribution == nil {
		return nil
	}
	attribution := identity.Attribution
	switch {
	case isOpenRouterTarget(model):
		headers := map[string]string{"HTTP-Referer": attribution.URL, "X-OpenRouter-Title": attribution.Name}
		if len(attribution.Categories) > 0 {
			headers["X-OpenRouter-Categories"] = strings.Join(attribution.Categories, ",")
		}
		return headers
	case isNvidiaNIMTarget(model):
		// NVIDIA expects a capitalized origin, such as "Pi".
		return map[string]string{"X-BILLING-INVOKE-ORIGIN": upperFirst(attribution.Name)}
	case IsOpenCodeTarget(model):
		return map[string]string{"x-opencode-client": attribution.Name}
	}
	return nil
}

// upperFirst upper-cases the first character, as pi's
// `charAt(0).toUpperCase() + slice(1)` does for a name that starts in the
// Basic Multilingual Plane.
func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// OpenCodeSessionHeaders is OpenCode's per-conversation routing header for
// requests to OpenCode, custom providers pointing at it included (pi
// getOpenCodeSessionHeaders): nil without a session or for another target.
func OpenCodeSessionHeaders(model *Model, sessionID string) ProviderHeaders {
	if sessionID == "" || !IsOpenCodeTarget(model) {
		return nil
	}
	return ProviderHeaders{"x-opencode-session": HeaderValue(sessionID)}
}

// IdentityHeaders are the default headers an identity adds to a request to
// model: User-Agent and provider attribution (pi getIdentityHeaders).
// Providers apply them below model, provider and request headers, so
// configured headers override them and a deletion marker in a request's
// headers removes them.
func IdentityHeaders(model *Model, identity *ClientIdentity) ProviderHeaders {
	headers := ProviderHeaders{"User-Agent": HeaderValue(FormatUserAgent(identity))}
	for name, value := range AttributionHeaders(model, identity) {
		headers[name] = HeaderValue(value)
	}
	return headers
}

// WithClientIdentity returns models with identity as the default identity of
// its requests (pi withClientIdentity): a request's Identity overrides it
// field by field, and it overrides the identity models was created with.
// Each request method calls the same method of models, and everything else is
// delegated unchanged; a MutableModels stays mutable.
func WithClientIdentity(models Models, identity ClientIdentity) Models {
	wrapped := identityModels{Models: models, identity: identity}
	if mutable, ok := models.(MutableModels); ok {
		return mutableIdentityModels{identityModels: wrapped, mutable: mutable}
	}
	return wrapped
}

type identityModels struct {
	Models
	identity ClientIdentity
}

func (m identityModels) with(request *ClientIdentity) *ClientIdentity {
	return MergeClientIdentity(&m.identity, request)
}

func (m identityModels) Stream(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessageEventStream {
	o := ModelsStreamOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.Stream(ctx, model, req, &o)
}

func (m identityModels) Complete(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessage {
	o := ModelsStreamOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.Complete(ctx, model, req, &o)
}

func (m identityModels) StreamSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessageEventStream {
	o := ModelsSimpleStreamOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.StreamSimple(ctx, model, req, &o)
}

func (m identityModels) CompleteSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessage {
	o := ModelsSimpleStreamOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.CompleteSimple(ctx, model, req, &o)
}

func (m identityModels) StreamDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessageEventStream {
	o := ModelsDeferredFetchOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.StreamDeferred(ctx, model, handle, &o)
}

func (m identityModels) FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessage {
	o := ModelsDeferredFetchOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.FetchDeferred(ctx, model, handle, &o)
}

func (m identityModels) CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredCancelOptions) error {
	o := ModelsDeferredCancelOptions{}
	if opts != nil {
		o = *opts
	}
	o.Identity = m.with(o.Identity)
	return m.Models.CancelDeferred(ctx, model, handle, &o)
}

type mutableIdentityModels struct {
	identityModels
	mutable MutableModels
}

func (m mutableIdentityModels) SetProvider(provider Provider) { m.mutable.SetProvider(provider) }
func (m mutableIdentityModels) DeleteProvider(id string)      { m.mutable.DeleteProvider(id) }
func (m mutableIdentityModels) ClearProviders()               { m.mutable.ClearProviders() }
