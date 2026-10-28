package coding

import (
	"context"
	"reflect"
	"testing"

	engine "github.com/aleksandrgradoboev-svg/bismarck-engine"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
)

// Ported from pi packages/coding-agent/test/sdk-client-identity.test.ts
// (upstream 04a93bad). DIVERGENCE: attribution follows a truthy PI_TELEMETRY
// (ClientIdentity), where pi's follows its install telemetry setting, on by
// default. n/a: the two OpenCode session cases, which pi checks on its
// ModelRuntime's request options; the session streams through ai.StreamSimple
// here, and the adapters add the header for OpenCode targets
// (ai/providers/attribution_test.go). Added: a host's SessionOptions.Identity.

// sessionIdentity runs one prompt through a session and returns the identity
// its request carried.
func sessionIdentity(t *testing.T, identity *ai.ClientIdentity) *ai.ClientIdentity {
	t.Helper()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	var seen *ai.ClientIdentity
	reg.SetResponses([]providers.FauxResponseStep{
		func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			seen = opts.Identity
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
		},
	})
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll, Identity: identity})
	if _, err := sess.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("the request carried no identity")
	}
	return seen
}

func TestSessionIdentifiesPiWithAttributionWhileTelemetryIsOn(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	identity := sessionIdentity(t, nil)
	detailed := ai.UserAgentDetailed
	attribution := ai.PIAttribution
	if want := (&ai.ClientIdentity{UserAgent: &detailed, Version: engine.Version(), Attribution: &attribution}); !reflect.DeepEqual(identity, want) {
		t.Fatalf("identity = %+v, want %+v", identity, want)
	}
	openRouter := &ai.Model{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1"}
	if got, want := ai.AttributionHeaders(openRouter, identity), (map[string]string{
		"HTTP-Referer": "https://pi.dev", "X-OpenRouter-Title": "pi", "X-OpenRouter-Categories": "cli-agent",
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("attribution = %v, want %v", got, want)
	}
}

func TestSessionTurnsAttributionOffWithoutTelemetry(t *testing.T) {
	for _, value := range []string{"", "0", "false"} {
		t.Run("PI_TELEMETRY="+value, func(t *testing.T) {
			t.Setenv("PI_TELEMETRY", value)
			identity := sessionIdentity(t, nil)
			detailed := ai.UserAgentDetailed
			if want := (&ai.ClientIdentity{UserAgent: &detailed, Version: engine.Version(), NoAttribution: true}); !reflect.DeepEqual(identity, want) {
				t.Fatalf("identity = %+v, want %+v", identity, want)
			}
			if got := ai.AttributionHeaders(&ai.Model{Provider: "openrouter"}, identity); len(got) != 0 {
				t.Fatalf("attribution = %v, want none", got)
			}
		})
	}
}

func TestSessionSendsTheHostsIdentity(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	custom := ai.CustomUserAgent("acme/1.0")
	host := &ai.ClientIdentity{UserAgent: &custom, NoAttribution: true}
	if identity := sessionIdentity(t, host); !reflect.DeepEqual(identity, host) {
		t.Fatalf("identity = %+v, want the host's %+v", identity, host)
	}
}
