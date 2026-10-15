package subagentext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

func writeAgent(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverAgents(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	writeAgent(t, user, "scout.md", "---\nname: scout\ndescription: Recon\ntools: read, grep\nmodel: faux/fast\n---\n\nYou scout.\n")
	writeAgent(t, user, "worker.md", "---\nname: worker\ndescription: Works\n---\nYou work.")
	writeAgent(t, user, "flow.md", "---\nname: flow\ndescription: Flow list\ntools: [read, \"bash\"]\n---\nbody")
	writeAgent(t, user, "nameless.md", "---\ndescription: No name\n---\nbody")
	writeAgent(t, user, "boolean.md", "---\nname: true\ndescription: Not a string name\n---\nbody")
	writeAgent(t, user, "notes.txt", "---\nname: notes\ndescription: Not markdown\n---\nbody")
	writeAgent(t, project, "scout.md", "---\nname: scout\ndescription: Project recon\n---\nProject scout.")

	names := func(d Discovery) []string {
		var out []string
		for _, a := range d.Agents {
			out = append(out, a.Name+":"+string(a.Source))
		}
		slices.Sort(out)
		return out
	}
	if got, want := names(DiscoverAgents(user, project, ScopeUser)), []string{"flow:user", "scout:user", "worker:user"}; !slices.Equal(got, want) {
		t.Fatalf("user scope = %v, want %v", got, want)
	}
	if got, want := names(DiscoverAgents(user, project, ScopeProject)), []string{"scout:project"}; !slices.Equal(got, want) {
		t.Fatalf("project scope = %v, want %v", got, want)
	}
	both := DiscoverAgents(user, project, ScopeBoth)
	if got, want := names(both), []string{"flow:user", "scout:project", "worker:user"}; !slices.Equal(got, want) {
		t.Fatalf("both = %v, want %v", got, want)
	}
	if both.ProjectAgentsDir != project {
		t.Fatalf("ProjectAgentsDir = %q", both.ProjectAgentsDir)
	}

	byName := map[string]Agent{}
	for _, a := range DiscoverAgents(user, "", ScopeUser).Agents {
		byName[a.Name] = a
	}
	scout := byName["scout"]
	if !slices.Equal(scout.Tools, []string{"read", "grep"}) || scout.Model != "faux/fast" || scout.SystemPrompt != "You scout." || scout.Description != "Recon" {
		t.Fatalf("scout = %+v", scout)
	}
	if !slices.Equal(byName["flow"].Tools, []string{"read", "bash"}) {
		t.Fatalf("flow tools = %v", byName["flow"].Tools)
	}
	if byName["worker"].Tools != nil || byName["worker"].Model != "" {
		t.Fatalf("worker = %+v", byName["worker"])
	}
}

func TestNearestProjectAgentsDir(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, ".pi", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := NearestProjectAgentsDir(deep); got != agents {
		t.Fatalf("NearestProjectAgentsDir = %q, want %q", got, agents)
	}
}

// fixture is a parent session on a faux model, a user agent directory and a
// session factory that records the subagent sessions it builds.
type fixture struct {
	reg      *providers.FauxProviderRegistration
	parent   *coding.Session
	userDir  string
	project  string
	opts     Options
	mu       sync.Mutex
	children []*coding.Session
	specs    []ChildSpec
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-main", ContextWindow: 100000, MaxTokens: 8192}, {ID: "faux-fast", ContextWindow: 100000, MaxTokens: 8192}},
	})
	t.Cleanup(reg.Unregister)
	cwd := t.TempDir()
	f := &fixture{reg: reg, userDir: t.TempDir(), project: filepath.Join(cwd, ".pi", "agents")}
	f.parent = coding.NewSession(coding.SessionOptions{Model: reg.GetModel("faux-main"), Cwd: cwd, NoTools: coding.NoToolsAll, NoContextFiles: true, NoSkills: true})
	f.opts = Options{
		UserAgentsDir:    f.userDir,
		ProjectAgentsDir: NearestProjectAgentsDir,
		ResolveModel: func(spec string) (*ai.Model, agent.ThinkingLevel, error) {
			if m := reg.GetModel(strings.TrimPrefix(spec, "faux/")); m != nil && strings.HasPrefix(spec, "faux/") {
				return m, "", nil
			}
			return nil, "", errors.New("unknown model " + spec)
		},
		NewSession: func(_ context.Context, spec ChildSpec) (*coding.Session, error) {
			child := coding.NewSession(coding.SessionOptions{
				Model: spec.Model, ThinkingLevel: spec.ThinkingLevel, Cwd: spec.Cwd,
				AppendSystemPrompt: spec.AppendSystemPrompt, NoContextFiles: true, NoSkills: true,
			})
			f.mu.Lock()
			f.children = append(f.children, child)
			f.specs = append(f.specs, spec)
			f.mu.Unlock()
			return child, nil
		},
	}
	return f
}

func (f *fixture) run(t *testing.T, params map[string]any) (agent.AgentToolResult, error) {
	t.Helper()
	return Definition(f.parent, f.opts).Tool.Execute(context.Background(), "call-1", params, nil)
}

// answer is a faux step that answers each task with reply(task).
func answer(reply func(task string) string) providers.FauxResponseStep {
	return func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		task := ""
		for _, m := range req.Messages {
			if u, ok := m.(ai.UserMessage); ok {
				task = strings.TrimPrefix(ai.ContentText(u.Content), "Task: ")
			}
		}
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: reply(task)}}, ai.StopStop)
	}
}

func steps(n int, step providers.FauxResponseStep) []providers.FauxResponseStep {
	out := make([]providers.FauxResponseStep, n)
	for i := range out {
		out[i] = step
	}
	return out
}

func resultText(r agent.AgentToolResult) string { return ai.ContentText(r.Content) }

func TestSingle(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nYou are the scout.")
	f.reg.SetResponses(steps(1, answer(func(task string) string { return "found: " + task })))

	res, err := f.run(t, map[string]any{"agent": "scout", "task": "auth code"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); got != "found: auth code" || res.IsError {
		t.Fatalf("result = %q (error %v)", got, res.IsError)
	}
	d := res.Details.(Details)
	if d.Mode != ModeSingle || d.AgentScope != ScopeUser || len(d.Results) != 1 {
		t.Fatalf("details = %+v", d)
	}
	r := d.Results[0]
	if r.AgentSource != SourceUser || r.ExitCode != 0 || r.Usage.Turns != 1 || r.Model != f.reg.GetModel("faux-main").Provider+"/faux-main" {
		t.Fatalf("result = %+v", r)
	}
	// The child inherits the dispatching model and gets the agent's prompt.
	if f.specs[0].Model != f.parent.Model || f.specs[0].Cwd != f.parent.Cwd {
		t.Fatalf("spec = %+v", f.specs[0])
	}
	prompt, err := f.children[0].SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "You are the scout.") {
		t.Fatalf("child prompt lacks the agent prompt:\n%s", prompt)
	}
	// Details serialize with pi's field names.
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mode":"single"`, `"agentScope":"user"`, `"projectAgentsDir":null`, `"agentSource":"user"`, `"exitCode":0`, `"turns":1`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("details JSON lacks %s: %s", key, data)
		}
	}
}

func TestAgentModel(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "fast.md", "---\nname: fast\ndescription: Fast\nmodel: faux/faux-fast\n---\nbody")
	writeAgent(t, f.userDir, "odd.md", "---\nname: odd\ndescription: Odd\nmodel: elsewhere/x\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "ok" })))

	res, err := f.run(t, map[string]any{"agent": "fast", "task": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if f.specs[0].Model != f.reg.GetModel("faux-fast") || res.Details.(Details).Results[0].Model != "faux/faux-fast" {
		t.Fatalf("spec model = %v, result = %+v", f.specs[0].Model, res.Details.(Details).Results[0])
	}

	res, err = f.run(t, map[string]any{"agent": "odd", "task": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "unknown model elsewhere/x") {
		t.Fatalf("unresolvable model: %q", resultText(res))
	}
}

func TestUnknownAgent(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	res, err := f.run(t, map[string]any{"agent": "ghost", "task": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `Agent failed: Unknown agent: "ghost". Available agents: "scout".`; resultText(res) != want || !res.IsError {
		t.Fatalf("result = %q, want %q", resultText(res), want)
	}
}

func TestInvalidMode(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	for _, params := range []map[string]any{
		{},
		{"agent": "scout"},
		{"agent": "scout", "task": "t", "tasks": []any{map[string]any{"agent": "scout", "task": "t"}}},
	} {
		res, err := f.run(t, params)
		if err != nil {
			t.Fatal(err)
		}
		if want := "Invalid parameters. Provide exactly one mode.\nAvailable agents: scout (user)"; resultText(res) != want {
			t.Fatalf("%v: %q", params, resultText(res))
		}
	}
}

func TestChain(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "a.md", "---\nname: a\ndescription: A\n---\nbody")
	var tasks []string
	var mu sync.Mutex
	f.reg.SetResponses(steps(2, answer(func(task string) string {
		mu.Lock()
		tasks = append(tasks, task)
		mu.Unlock()
		return "out(" + task + ")"
	})))
	var updates int
	res, err := Definition(f.parent, f.opts).Tool.Execute(context.Background(), "c", map[string]any{"chain": []any{
		map[string]any{"agent": "a", "task": "first"},
		map[string]any{"agent": "a", "task": "then {previous}"},
	}}, func(agent.AgentToolResult) { updates++ })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"first", "then out(first)"}; !slices.Equal(tasks, want) {
		t.Fatalf("tasks = %v, want %v", tasks, want)
	}
	if resultText(res) != "out(then out(first))" {
		t.Fatalf("result = %q", resultText(res))
	}
	d := res.Details.(Details)
	if d.Mode != ModeChain || len(d.Results) != 2 || d.Results[0].Step != 1 || d.Results[1].Step != 2 || updates == 0 {
		t.Fatalf("details = %+v, updates %d", d, updates)
	}
}

func TestChainStopsAtFailure(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "a.md", "---\nname: a\ndescription: A\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "ok" })))
	res, err := f.run(t, map[string]any{"chain": []any{
		map[string]any{"agent": "a", "task": "one"},
		map[string]any{"agent": "ghost", "task": "two"},
		map[string]any{"agent": "a", "task": "three"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(resultText(res), "Chain stopped at step 2 (ghost): Unknown agent") || len(res.Details.(Details).Results) != 2 {
		t.Fatalf("result = %q, details %+v", resultText(res), res.Details)
	}
}

func TestParallel(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "a.md", "---\nname: a\ndescription: A\n---\nbody")
	f.reg.SetResponses(steps(5, answer(func(task string) string { return strings.ToUpper(task) })))
	var tasks []any
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		tasks = append(tasks, map[string]any{"agent": "a", "task": name})
	}
	tasks = append(tasks, map[string]any{"agent": "ghost", "task": "six"})
	res, err := f.run(t, map[string]any{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(res)
	if !strings.HasPrefix(text, "Parallel: 5/6 succeeded\n\n### [a] completed\n\nONE\n\n---\n\n### [a] completed\n\nTWO") ||
		!strings.Contains(text, "### [ghost] failed\n\nUnknown agent") {
		t.Fatalf("result = %q", text)
	}
	d := res.Details.(Details)
	if d.Mode != ModeParallel || len(d.Results) != 6 || d.Results[4].Task != "five" {
		t.Fatalf("details = %+v", d)
	}
}

func TestParallelLimit(t *testing.T) {
	f := newFixture(t)
	tasks := make([]any, MaxParallelTasks+1)
	for i := range tasks {
		tasks[i] = map[string]any{"agent": "a", "task": "t"}
	}
	res, err := f.run(t, map[string]any{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	if resultText(res) != "Too many parallel tasks (9). Max is 8." {
		t.Fatalf("result = %q", resultText(res))
	}
}

func TestTruncateParallelOutput(t *testing.T) {
	short := "short"
	if truncateParallelOutput(short) != short {
		t.Fatal("short output changed")
	}
	long := strings.Repeat("я", PerTaskOutputCap) // two bytes each
	got := truncateParallelOutput(long)
	kept := got[:strings.Index(got, "\n\n[Output truncated")]
	if len(kept) != PerTaskOutputCap || !strings.Contains(got, "bytes omitted") {
		t.Fatalf("kept %d bytes", len(kept))
	}
}

// TestProjectTrust is pi's regression #8261 with this port's rules: hosts that
// cannot ask refuse, and the model cannot skip the question — a call that
// sends pi's confirmProjectAgents: false is asked all the same.
func TestProjectTrust(t *testing.T) {
	if _, ok := Definition(nil, Options{}).Tool.Parameters.Properties["confirmProjectAgents"]; ok {
		t.Fatal("the tool still offers confirmProjectAgents")
	}
	cases := []struct {
		name         string
		trusted      bool
		confirm      *bool // nil: no Confirm
		skipConfirm  bool  // confirmProjectAgents: false
		wantCalls    int
		wantCanceled bool
	}{
		{name: "trusted skips confirmation", trusted: true, confirm: ptr(false), wantCalls: 0},
		{name: "untrusted asks and is refused", confirm: ptr(false), wantCalls: 1, wantCanceled: true},
		{name: "untrusted asks and is approved", confirm: ptr(true), wantCalls: 1},
		{name: "with UI the call cannot skip the question", confirm: ptr(false), skipConfirm: true, wantCalls: 1, wantCanceled: true},
		{name: "without UI untrusted is refused", wantCanceled: true},
		{name: "without UI the call cannot skip", skipConfirm: true, wantCanceled: true},
		{name: "without UI trusted runs", trusted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			writeAgent(t, f.project, "project-agent.md", "---\nname: project-agent\ndescription: Project test agent\n---\nHandle the delegated task.")
			f.reg.SetResponses(steps(1, answer(func(string) string { return "done" })))
			calls := 0
			f.opts.ProjectTrusted = func() bool { return tc.trusted }
			if tc.confirm != nil {
				f.opts.Confirm = func(context.Context, string, string) bool { calls++; return *tc.confirm }
			}
			params := map[string]any{"agent": "project-agent", "task": "Test project trust", "agentScope": "project"}
			if tc.skipConfirm {
				params["confirmProjectAgents"] = false
			}
			res, err := f.run(t, params)
			if err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("confirm calls = %d, want %d", calls, tc.wantCalls)
			}
			if canceled := resultText(res) == canceledByConfirm; canceled != tc.wantCanceled {
				t.Fatalf("result = %q, want canceled %v", resultText(res), tc.wantCanceled)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestUserAgentsNeedNoTrust(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "done" })))
	res, err := f.run(t, map[string]any{"agent": "scout", "task": "t", "agentScope": "both"})
	if err != nil || resultText(res) != "done" {
		t.Fatalf("result = %q, err %v", resultText(res), err)
	}
}

func TestAgentTools(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "reader.md", "---\nname: reader\ndescription: Reads\ntools: read, gr*\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "ok" })))
	f.opts.NewSession = func(_ context.Context, spec ChildSpec) (*coding.Session, error) {
		child := coding.NewSession(coding.SessionOptions{Model: spec.Model, Cwd: spec.Cwd, ToolNames: []string{"read", "bash", "edit", "grep"}, NoContextFiles: true, NoSkills: true})
		f.children = append(f.children, child)
		return child, nil
	}
	if _, err := f.run(t, map[string]any{"agent": "reader", "task": "t"}); err != nil {
		t.Fatal(err)
	}
	got := f.children[0].ActiveToolNames()
	slices.Sort(got)
	if want := []string{"grep", "read"}; !slices.Equal(got, want) {
		t.Fatalf("active tools = %v, want %v", got, want)
	}
}

func TestAbort(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "late" })))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Definition(f.parent, f.opts).Tool.Execute(ctx, "c", map[string]any{"agent": "scout", "task": "t"}, nil)
	if !errors.Is(err, errAborted) {
		t.Fatalf("err = %v, want %v", err, errAborted)
	}
}

func TestNewSessionError(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	f.opts.NewSession = func(context.Context, ChildSpec) (*coding.Session, error) { return nil, errors.New("no seats") }
	res, err := f.run(t, map[string]any{"agent": "scout", "task": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if resultText(res) != "Agent failed: no seats" || !res.IsError {
		t.Fatalf("result = %q", resultText(res))
	}
}
