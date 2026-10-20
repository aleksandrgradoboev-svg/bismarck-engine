package harness

import (
	"context"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-callers.test.ts: the three
// cases that probe executeTool.

// prober is a tool that calls each of names as a nested call and records
// what it got back.
type prober struct {
	registration *ToolRegistration
	mu           sync.Mutex
	received     map[string]NestedToolExecutionResult
}

func newProber(names ...string) *prober {
	p := &prober{received: map[string]NestedToolExecutionResult{}}
	p.registration = &ToolRegistration{
		Tool: ai.Tool{Name: "probe", Description: "probe", Parameters: objectSchema()},
		Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			for _, name := range names {
				result, err := api.ExecuteTool(ctx, name, map[string]any{}, NestedCallOptions{})
				if err != nil {
					return ToolExecutionResult{}, err
				}
				p.mu.Lock()
				p.received[name] = result
				p.mu.Unlock()
			}
			return ToolExecutionResult{Output: ai.ContentList{}}, nil
		},
	}
	return p
}

func (p *prober) result(name string) NestedToolExecutionResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.received[name]
}

func callableNames(t *testing.T, conversation *Conversation) []string {
	t.Helper()
	agent, err := conversation.Agent(bg)
	must(t, err)
	return toolNames(agent.Callable)
}

func setupWithTools(t *testing.T, tools ...*ToolRegistration) *chatSetup {
	t.Helper()
	setup := newChatSetup(t)
	for _, tool := range tools {
		installTool(t, setup.registry, tool)
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"))), fauxText("done")})
	return setup
}

func firstCode(result NestedToolExecutionResult) string {
	if len(result.Diagnostics) == 0 {
		return ""
	}
	return result.Diagnostics[0].Code
}

func TestCallersOfferTheModelOnlyToolsItMayCallAndLetToolsCallOnlyToolsTheyMayCall(t *testing.T) {
	script, hidden, both := callerTool("script"), callerTool("hidden"), callerTool("both")
	script.Callers = []string{CallerModel}
	hidden.Callers = []string{CallerTools}
	probe := newProber("script", "hidden", "both")
	setup := setupWithTools(t, script, hidden, both, probe.registration)
	_, root := openChat(t, memory.New(), setup)
	expectStrings(t, "offered", offeredNames(t, root), []string{"script", "both", "probe"})
	expectStrings(t, "callable", callableNames(t, root), []string{"hidden", "both", "probe"})
	waitFor(t, submit(t, root, inputDraft("go")))
	if code := firstCode(probe.result("script")); code != "tool_unavailable" {
		t.Fatalf("script = %s", jsonOf(t, probe.result("script")))
	}
	for _, name := range []string{"hidden", "both"} {
		if result := probe.result(name); result.IsError || string(result.StructuredOutput) != `"`+name+` ran"` {
			t.Fatalf("%s = %s", name, jsonOf(t, result))
		}
	}
}

func TestCallersNarrowWhatTheModelIsOfferedWithModelToolsLeavingTheRestCallableByTools(t *testing.T) {
	read, bash := callerTool("read"), callerTool("bash")
	probe := newProber("read", "bash")
	setup := setupWithTools(t, read, bash, probe.registration)
	h, root := openChat(t, memory.New(), setup)
	must(t, root.Configure(bg, AgentChange{ModelTools: Set(Exactly(probe.registration))}))
	expectStrings(t, "offered", offeredNames(t, root), []string{"probe"})
	expectStrings(t, "callable", callableNames(t, root), []string{"read", "bash", "probe"})
	waitFor(t, submit(t, root, inputDraft("go")))
	if probe.result("read").IsError || probe.result("bash").IsError {
		t.Fatalf("read = %s, bash = %s", jsonOf(t, probe.result("read")), jsonOf(t, probe.result("bash")))
	}

	must(t, root.Configure(bg, AgentChange{ModelTools: Set(Edit(nil, []*ToolRegistration{bash}))}))
	expectStrings(t, "offered", offeredNames(t, root), []string{"read", "probe"})
	agent, err := h.Snapshot(bg, AgentDoc.Conversation(root.ID))
	must(t, err)
	if got := jsonOf(t, agent["modelTools"]); got != `{"remove":["bash"]}` {
		t.Fatalf("stored modelTools = %s", got)
	}
	must(t, root.Configure(bg, AgentChange{ModelTools: Clear[Selection[*ToolRegistration]]()}))
	expectStrings(t, "offered", offeredNames(t, root), []string{"read", "bash", "probe"})
}

func TestCallersNeverOfferAToolThatIsNotEnabledNorLetToolsCallIt(t *testing.T) {
	read, bash := callerTool("read"), callerTool("bash")
	probe := newProber("bash")
	setup := setupWithTools(t, read, bash, probe.registration)
	_, root := openChat(t, memory.New(), setup)
	// modelTools cannot bring back what tools disabled.
	must(t, root.Configure(bg, AgentChange{
		Tools:      Set(Edit(nil, []*ToolRegistration{bash})),
		ModelTools: Set(Exactly(bash, probe.registration)),
	}))
	expectStrings(t, "offered", offeredNames(t, root), []string{"probe"})
	expectStrings(t, "callable", callableNames(t, root), []string{"read", "probe"})
	waitFor(t, submit(t, root, inputDraft("go")))
	if code := firstCode(probe.result("bash")); code != "tool_unavailable" {
		t.Fatalf("bash = %s", jsonOf(t, probe.result("bash")))
	}
}
