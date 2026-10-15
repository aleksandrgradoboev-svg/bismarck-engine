package coding

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
)

// TestSessionEndToEndWithFauxProvider drives a full coding session through the
// real agent loop + faux provider: the model asks to write a file via the write
// tool, then reports done. Verifies the tool ran and the file was created.
func TestSessionEndToEndWithFauxProvider(t *testing.T) {
	dir := t.TempDir()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", Reasoning: false}},
	})
	defer reg.Unregister()

	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			providers.FauxToolCall("write", map[string]any{"path": "hello.txt", "content": "hi from pi\n"}, "call_1"),
		}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{
			ai.TextContent{Text: "Done — wrote hello.txt."},
		}, ai.StopStop)),
	})

	sess := NewSession(SessionOptions{
		Model: reg.GetModel(),
		Cwd:   dir,
		Tools: CreateAllTools(dir),
	})

	var out bytes.Buffer
	final, err := sess.RunPrint(context.Background(), &out, "create hello.txt with a greeting")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "Done") {
		t.Fatalf("unexpected final text: %q", final)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(data) != "hi from pi\n" {
		t.Fatalf("file not written by tool: %v / %q", err, data)
	}
	// Output should show the tool activity.
	if !strings.Contains(out.String(), "write") {
		t.Fatalf("tool activity not rendered: %q", out.String())
	}
}

func TestSessionUsesCodingSystemPrompt(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	var capturedSystem string
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			capturedSystem = ai.GetCurrentSystemPrompt(req.Messages)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
		},
	})
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: "/tmp/proj", Tools: CreateCodingTools("/tmp/proj")})
	if _, err := sess.RunPrint(context.Background(), &bytes.Buffer{}, "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedSystem, "expert coding assistant operating inside pi") {
		t.Fatalf("system prompt not applied: %q", capturedSystem)
	}
}

// A tool's reported usage counts in the run's Usage, as pi's getSessionStats
// counts toolResult usage.
func TestRunUsageCountsToolResultUsage(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("spend", map[string]any{}, "call_1")}, ai.StopToolUse)),
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "done"}}, ai.StopStop)),
	})
	spent := ai.Usage{Input: 100, Output: 20, TotalTokens: 120, Cost: ai.CostBreakdown{Input: 1, Output: 0.5, Total: 1.5}}
	spend := agent.AgentTool{Name: "spend", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		u := spent
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Usage: &u}, nil
	}}
	sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{spend}, NoContextFiles: true, NoSkills: true})
	res, err := sess.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var responses ai.Usage
	for _, m := range res.Messages {
		if am, ok := messageAsAssistant(m); ok {
			addUsage(&responses, am.Usage)
		}
	}
	want := responses
	addUsage(&want, spent)
	if res.Usage != want {
		t.Fatalf("run usage = %+v, want responses %+v plus the tool's %+v", res.Usage, responses, spent)
	}
}

func TestSessionAppendSystemPrompt(t *testing.T) {
	for _, custom := range []string{"", "You are a test assistant."} {
		reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
		var capturedSystem string
		reg.SetResponses([]providers.FauxResponseStep{
			func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
				capturedSystem = ai.GetCurrentSystemPrompt(req.Messages)
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
			},
		})
		sess := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), SystemPrompt: custom, AppendSystemPrompt: "APPENDED RULE",
			NoTools: NoToolsAll, NoContextFiles: true, NoSkills: true})
		if _, err := sess.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		reg.Unregister()
		appended := strings.Index(capturedSystem, "APPENDED RULE")
		if appended < 0 {
			t.Fatalf("custom %q: appended prompt missing: %q", custom, capturedSystem)
		}
		if custom != "" && !(strings.Index(capturedSystem, custom) >= 0 && strings.Index(capturedSystem, custom) < appended) {
			t.Fatalf("custom prompt should come first: %q", capturedSystem)
		}
	}
}

func TestResolveModelFromCatalog(t *testing.T) {
	m, err := ResolveModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "anthropic" || m.Api != ai.APIAnthropicMessages {
		t.Fatalf("unexpected model: %#v", m)
	}
	// Bare id resolution.
	if _, err := ResolveModel("claude-haiku-4-5"); err != nil {
		t.Fatalf("bare id resolution failed: %v", err)
	}
	if _, err := ResolveModel("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

var _ = agent.ThinkOff

// pi resource loader noContextFiles / noSkills / additionalSkillPaths: a
// session can skip AGENTS.md discovery and default skill discovery and still
// load skills from explicit paths.
func TestSessionNoContextFilesNoSkillsSkillPaths(t *testing.T) {
	setTestHome(t, t.TempDir())
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "project rules")
	skillsDir := t.TempDir()
	writeFile(t, filepath.Join(skillsDir, "data-1c", "SKILL.md"), "---\nname: data-1c\ndescription: Work with 1C data.\n---\nbody")

	sess := NewSession(SessionOptions{
		Model:          &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: "openai", ContextWindow: 1000, MaxTokens: 100},
		Cwd:            cwd,
		ToolNames:      []string{"read"},
		NoContextFiles: true,
		NoSkills:       true,
		SkillPaths:     []string{skillsDir},
	})
	opts := sess.systemPromptOptions
	if len(opts.ContextFiles) != 0 {
		t.Fatalf("NoContextFiles: context files = %v, want none", opts.ContextFiles)
	}
	if len(opts.Skills) != 1 || opts.Skills[0].Name != "data-1c" {
		t.Fatalf("skills = %v, want only data-1c from SkillPaths", opts.Skills)
	}
}
