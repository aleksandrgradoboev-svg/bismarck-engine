package subagentext

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	return func(_ context.Context, req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
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

// TestChildSpecNamesTheCall: every child knows the call that runs it and its
// place there, so a host can tell two tasks of one parallel call apart.
func TestChildSpecNamesTheCall(t *testing.T) {
	positions := func(f *fixture) map[string]int {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]int{}
		for _, s := range f.specs {
			if s.ToolCallID != "call-1" {
				t.Errorf("%s: ToolCallID = %q, want call-1", s.Agent.Name, s.ToolCallID)
			}
			out[s.Agent.Name] = s.Index
		}
		return out
	}
	newWithAgents := func(t *testing.T, n int) *fixture {
		f := newFixture(t)
		for _, name := range []string{"a", "b", "c"} {
			writeAgent(t, f.userDir, name+".md", "---\nname: "+name+"\ndescription: "+name+"\n---\nbody")
		}
		f.reg.SetResponses(steps(n, answer(func(string) string { return "ok" })))
		return f
	}

	t.Run("single", func(t *testing.T) {
		f := newWithAgents(t, 1)
		if _, err := f.run(t, map[string]any{"agent": "a", "task": "x"}); err != nil {
			t.Fatal(err)
		}
		if got := positions(f); !maps.Equal(got, map[string]int{"a": 0}) {
			t.Fatalf("positions = %v", got)
		}
	})
	t.Run("chain", func(t *testing.T) {
		f := newWithAgents(t, 2)
		if _, err := f.run(t, map[string]any{"chain": []any{
			map[string]any{"agent": "a", "task": "x"}, map[string]any{"agent": "b", "task": "y"},
		}}); err != nil {
			t.Fatal(err)
		}
		if got := positions(f); !maps.Equal(got, map[string]int{"a": 1, "b": 2}) {
			t.Fatalf("positions = %v", got)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		f := newWithAgents(t, 3)
		if _, err := f.run(t, map[string]any{"tasks": []any{
			map[string]any{"agent": "a", "task": "x"}, map[string]any{"agent": "b", "task": "y"}, map[string]any{"agent": "c", "task": "z"},
		}}); err != nil {
			t.Fatal(err)
		}
		if got := positions(f); !maps.Equal(got, map[string]int{"a": 1, "b": 2, "c": 3}) {
			t.Fatalf("positions = %v", got)
		}
	})
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

// The tool reports what its subagents spent as its result's Usage, so the
// dispatching session counts it; an agent that never ran adds nothing.
func TestResultUsageSumsSubagents(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "a.md", "---\nname: a\ndescription: A\n---\nbody")
	f.reg.SetResponses(steps(2, answer(func(task string) string { return strings.Repeat(task, 50) })))
	res, err := f.run(t, map[string]any{"tasks": []any{
		map[string]any{"agent": "a", "task": "one"},
		map[string]any{"agent": "a", "task": "two"},
		map[string]any{"agent": "ghost", "task": "three"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var want ai.Usage
	for _, r := range res.Details.(Details).Results {
		for _, m := range r.Messages {
			if am, ok := asAssistant(m); ok {
				addUsage(&want, am.Usage)
			}
		}
	}
	if want.Input == 0 || want.Output == 0 {
		t.Fatalf("the faux responses reported no usage: %+v", want)
	}
	if res.Usage == nil || *res.Usage != want {
		t.Fatalf("result usage = %+v, want the subagents' %+v", res.Usage, want)
	}

	// Nothing ran: no usage.
	res, err = f.run(t, map[string]any{"agent": "ghost", "task": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != nil {
		t.Fatalf("an unknown agent reported usage %+v", *res.Usage)
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

// TestBaseToolsGoOnTopOfAnAgentsList: the host's base tools stay active in an
// agent whatever its tools list, run after run, and a name the child lacks is
// left out.
func TestBaseToolsGoOnTopOfAnAgentsList(t *testing.T) {
	f := newFixture(t)
	writeAgent(t, f.userDir, "reader.md", "---\nname: reader\ndescription: Reads\ntools: read\n---\nbody")
	f.reg.SetResponses(steps(2, answer(func(string) string { return "ok" })))
	f.opts.BaseTools = []string{"edit", "nonexistent"}
	f.opts.NewSession = func(_ context.Context, spec ChildSpec) (*coding.Session, error) {
		child := coding.NewSession(coding.SessionOptions{Model: spec.Model, Cwd: spec.Cwd, ToolNames: []string{"read", "bash", "edit", "grep"}, NoContextFiles: true, NoSkills: true})
		f.children = append(f.children, child)
		return child, nil
	}
	for range 2 {
		if _, err := f.run(t, map[string]any{"agent": "reader", "task": "t"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range f.children {
		got := c.ActiveToolNames()
		slices.Sort(got)
		if want := []string{"edit", "read"}; !slices.Equal(got, want) {
			t.Fatalf("run %d: active tools = %v, want %v", i, got, want)
		}
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

// backgroundFixture is a fixture whose host takes background calls: it keeps
// the run it is handed, or refuses with refuse when set.
func backgroundFixture(t *testing.T) (*fixture, *[]BackgroundRun, *error) {
	f := newFixture(t)
	var runs []BackgroundRun
	var refuse error
	f.opts.Background = func(callID string, run BackgroundRun) error {
		if callID != "call-1" {
			t.Errorf("Background got call %q", callID)
		}
		if refuse != nil {
			return refuse
		}
		runs = append(runs, run)
		return nil
	}
	return f, &runs, &refuse
}

func TestBackgroundParameterOnlyWithHost(t *testing.T) {
	f := newFixture(t)
	def := Definition(f.parent, f.opts)
	if _, ok := def.Tool.Parameters.Properties["background"]; ok || strings.Contains(def.Tool.Description, "background") {
		t.Fatal("background offered by a host that cannot run it")
	}
	f.opts.Background = func(string, BackgroundRun) error { return nil }
	def = Definition(f.parent, f.opts)
	if p := def.Tool.Parameters.Properties["background"]; p == nil || p.Type != "boolean" || slices.Contains(def.Tool.Parameters.Required, "background") {
		t.Fatalf("background parameter = %+v", p)
	}
}

func TestBackground(t *testing.T) {
	f, runs, _ := backgroundFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(task string) string { return "found: " + task })))

	res, err := f.run(t, map[string]any{"agent": "scout", "task": "auth", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	// Returned at once: nothing ran, and the model is told not to guess.
	if len(f.children) != 0 || len(*runs) != 1 {
		t.Fatalf("children %d, runs %d", len(f.children), len(*runs))
	}
	text := resultText(res)
	if res.IsError || !strings.Contains(text, "Started in the background (call call-1): scout.") || !strings.Contains(text, "do not describe it, guess it") {
		t.Fatalf("started result = %q", text)
	}
	d := res.Details.(Details)
	if !d.Background || len(d.Results) != 1 || d.Results[0].ExitCode != -1 || d.Results[0].Task != "auth" || d.Results[0].AgentSource != SourceUser {
		t.Fatalf("started details = %+v", d)
	}
	if res.Usage != nil {
		t.Fatalf("started result spent %+v", res.Usage)
	}

	updates := 0
	done, err := (*runs)[0](context.Background(), func(agent.AgentToolResult) { updates++ })
	if err != nil {
		t.Fatal(err)
	}
	if resultText(done) != "found: auth" || done.Details.(Details).Background || updates == 0 {
		t.Fatalf("run result = %q, updates %d", resultText(done), updates)
	}
	if done.Usage == nil || done.Usage.Output == 0 {
		t.Fatalf("run result usage = %+v", done.Usage)
	}
}

func TestBackgroundFalseWaits(t *testing.T) {
	f, runs, _ := backgroundFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(task string) string { return "found: " + task })))
	res, err := f.run(t, map[string]any{"agent": "scout", "task": "auth", "background": false})
	if err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 0 || resultText(res) != "found: auth" {
		t.Fatalf("runs %d, result %q", len(*runs), resultText(res))
	}
}

func TestBackgroundRefusals(t *testing.T) {
	f, runs, refuse := backgroundFixture(t)
	writeAgent(t, f.userDir, "scout.md", "---\nname: scout\ndescription: Recon\n---\nbody")
	writeAgent(t, f.userDir, "judge.md", "---\nname: judge\ndescription: Judge\nbackground: never\n---\nbody")
	many := make([]any, MaxParallelTasks+1)
	for i := range many {
		many[i] = map[string]any{"agent": "scout", "task": "t"}
	}
	for _, c := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"agent": "ghost", "task": "t"}, `Unknown agent: "ghost". Available agents: "judge", "scout".`},
		{map[string]any{"agent": "judge", "task": "t"}, `Agent "judge" does not run in the background: call it without background.`},
		{map[string]any{"chain": []any{map[string]any{"agent": "scout", "task": "t"}, map[string]any{"agent": "judge", "task": "{previous}"}}}, `Agent "judge" does not run in the background`},
		{map[string]any{"tasks": many}, "Too many parallel tasks (9). Max is 8."},
	} {
		c.params["background"] = true
		res, err := f.run(t, c.params)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.HasPrefix(resultText(res), c.want) {
			t.Fatalf("%v: %q (error %v), want %q", c.params, resultText(res), res.IsError, c.want)
		}
	}
	if len(*runs) != 0 || len(f.children) != 0 {
		t.Fatalf("refused calls ran: runs %d, children %d", len(*runs), len(f.children))
	}

	// The host refuses: the model gets why.
	*refuse = errors.New("two roles already run in the background")
	res, err := f.run(t, map[string]any{"agent": "scout", "task": "t", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || resultText(res) != "two roles already run in the background" {
		t.Fatalf("host refusal: %q (error %v)", resultText(res), res.IsError)
	}
}

func TestNoBackgroundFromFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "a.md", "---\nname: a\ndescription: A\nbackground: never\n---\nbody")
	writeAgent(t, dir, "b.md", "---\nname: b\ndescription: B\nbackground: allowed\n---\nbody")
	got := map[string]bool{}
	for _, a := range DiscoverAgents(dir, "", ScopeUser).Agents {
		got[a.Name] = a.NoBackground
	}
	if !got["a"] || got["b"] {
		t.Fatalf("NoBackground = %v", got)
	}
}

// detachFixture is a background fixture whose host also lets the user move a
// call to the background: detach closes the channel Detach hands out, and
// released counts the releases. Its agent "slow" answers once gate is closed,
// after it has signalled started.
type detachFixture struct {
	*fixture
	runs     *[]BackgroundRun
	refuse   *error
	asked    chan struct{}
	released atomic.Int32
	started  chan struct{}
	gate     chan struct{}
	requests atomic.Int32
}

func newDetachFixture(t *testing.T) *detachFixture {
	f, runs, refuse := backgroundFixture(t)
	d := &detachFixture{fixture: f, runs: runs, refuse: refuse, asked: make(chan struct{}), started: make(chan struct{}), gate: make(chan struct{})}
	f.opts.Detach = func(callID string) (<-chan struct{}, func()) {
		if callID != "call-1" {
			t.Errorf("Detach got call %q", callID)
		}
		return d.asked, func() { d.released.Add(1) }
	}
	writeAgent(t, f.userDir, "slow.md", "---\nname: slow\ndescription: Slow\n---\nbody")
	var once sync.Once
	f.reg.SetResponses(steps(1, func(ctx context.Context, req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
		d.requests.Add(1)
		once.Do(func() { close(d.started) })
		select {
		case <-d.gate:
		case <-ctx.Done():
		}
		return answer(func(task string) string { return "done: " + task })(ctx, req, opts, st, m)
	}))
	return d
}

type callOutcome struct {
	res agent.AgentToolResult
	err error
}

func (d *detachFixture) start(t *testing.T, ctx context.Context) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		res, err := Definition(d.parent, d.opts).Tool.Execute(ctx, "call-1", map[string]any{"agent": "slow", "task": "t"}, nil)
		out <- callOutcome{res, err}
	}()
	select {
	case <-d.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the subagent never asked the model")
	}
	return out
}

func wait(t *testing.T, out <-chan callOutcome) callOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not return")
	}
	return callOutcome{}
}

// TestDetachMovesARunningCall: asked while the subagent works, the call returns
// at once and hands the same run to the host — nothing restarts; the parent's
// abort no longer reaches it, the host's context does not either until it
// is cancelled, and the result is the run's.
func TestDetachMovesARunningCall(t *testing.T) {
	d := newDetachFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := d.start(t, ctx)
	close(d.asked)
	o := wait(t, out)
	if o.err != nil || o.res.IsError || !strings.Contains(resultText(o.res), "The user moved this call to the background (call call-1): slow.") {
		t.Fatalf("moved result = %q (%v)", resultText(o.res), o.err)
	}
	if det := o.res.Details.(Details); !det.Background || len(det.Results) != 1 || det.Results[0].ExitCode != -1 {
		t.Fatalf("moved details = %+v", det)
	}
	if len(*d.runs) != 1 || d.released.Load() != 1 {
		t.Fatalf("runs %d, releases %d", len(*d.runs), d.released.Load())
	}
	cancel() // the answer that made the call is stopped: the role is not
	close(d.gate)
	updates := 0
	res, err := (*d.runs)[0](context.Background(), func(agent.AgentToolResult) { updates++ })
	if err != nil || resultText(res) != "done: t" || updates == 0 || res.Usage == nil {
		t.Fatalf("run = %q (%v), updates %d, usage %v", resultText(res), err, updates, res.Usage)
	}
	if n := d.requests.Load(); n != 1 || len(d.children) != 1 {
		t.Fatalf("the run restarted: %d requests, %d children", n, len(d.children))
	}
}

// TestDetachHandsTheHostTheUpdateMadeBeforeItRanTheCall: a moved call whose
// subagent answered before the host ran it keeps that partial result for the
// host, which gets it first.
func TestDetachHandsTheHostTheUpdateMadeBeforeItRanTheCall(t *testing.T) {
	d := newDetachFixture(t)
	out := d.start(t, context.Background())
	close(d.asked)
	if o := wait(t, out); o.err != nil || !o.res.Details.(Details).Background {
		t.Fatalf("moved = %+v (%v)", o.res, o.err)
	}
	close(d.gate)
	// The subagent answers and reports while no host runs the call; were it
	// still running, the update would reach the host live instead.
	time.Sleep(100 * time.Millisecond)
	var updates []string
	res, err := (*d.runs)[0](context.Background(), func(p agent.AgentToolResult) { updates = append(updates, resultText(p)) })
	if err != nil || resultText(res) != "done: t" {
		t.Fatalf("run = %q (%v)", resultText(res), err)
	}
	if len(updates) != 1 || updates[0] != "done: t" {
		t.Fatalf("updates = %q", updates)
	}
}

// TestDetachedRunStopsWithTheHost: the host's context stops a moved run.
func TestDetachedRunStopsWithTheHost(t *testing.T) {
	d := newDetachFixture(t)
	out := d.start(t, context.Background())
	close(d.asked)
	wait(t, out)
	hostCtx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := (*d.runs)[0](hostCtx, nil); err == nil {
		t.Fatal("a run the host stopped finished")
	}
}

// TestDetachRefusedStaysInTheForeground: when the host refuses, the call waits
// for its run as before and returns its result.
func TestDetachRefusedStaysInTheForeground(t *testing.T) {
	d := newDetachFixture(t)
	*d.refuse = errors.New("full")
	out := d.start(t, context.Background())
	close(d.asked)
	time.Sleep(50 * time.Millisecond)
	close(d.gate)
	o := wait(t, out)
	if o.err != nil || resultText(o.res) != "done: t" || o.res.Usage == nil {
		t.Fatalf("result = %q (%v)", resultText(o.res), o.err)
	}

	// Refused, the call's abort stops the run again and its updates come back.
	d = newDetachFixture(t)
	*d.refuse = errors.New("full")
	ctx, cancel := context.WithCancel(context.Background())
	var updates atomic.Int32
	aborted := make(chan callOutcome, 1)
	go func() {
		res, err := Definition(d.parent, d.opts).Tool.Execute(ctx, "call-1", map[string]any{"agent": "slow", "task": "t"}, func(agent.AgentToolResult) { updates.Add(1) })
		aborted <- callOutcome{res, err}
	}()
	<-d.started
	close(d.asked)
	time.Sleep(50 * time.Millisecond)
	before := updates.Load()
	cancel()
	if o := wait(t, aborted); o.err == nil {
		t.Fatalf("a refused move then aborted finished: %q", resultText(o.res))
	}
	if updates.Load() <= before {
		t.Fatal("updates did not come back after a refused move")
	}
}

// TestNotDetachedEndsAsUsual: a call nobody moves returns its result, its
// abort stops it, and Detach is released either way.
func TestNotDetachedEndsAsUsual(t *testing.T) {
	d := newDetachFixture(t)
	out := d.start(t, context.Background())
	close(d.gate)
	if o := wait(t, out); o.err != nil || resultText(o.res) != "done: t" || len(*d.runs) != 0 || d.released.Load() != 1 {
		t.Fatalf("result = %q (%v), runs %d, releases %d", resultText(o.res), o.err, len(*d.runs), d.released.Load())
	}

	d = newDetachFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	out = d.start(t, ctx)
	cancel()
	if o := wait(t, out); o.err == nil || len(*d.runs) != 0 {
		t.Fatalf("an aborted call: %v, runs %d", o.err, len(*d.runs))
	}
}

// TestNoDetachForAgentsThatStayInTheForeground: a call of an agent marked
// "background: never" does not ask Detach.
func TestNoDetachForAgentsThatStayInTheForeground(t *testing.T) {
	f, _, _ := backgroundFixture(t)
	f.opts.Detach = func(string) (<-chan struct{}, func()) {
		t.Error("Detach asked for an agent that stays in the foreground")
		return nil, func() {}
	}
	writeAgent(t, f.userDir, "judge.md", "---\nname: judge\ndescription: Judge\nbackground: never\n---\nbody")
	f.reg.SetResponses(steps(1, answer(func(string) string { return "ok" })))
	if res, err := f.run(t, map[string]any{"agent": "judge", "task": "t"}); err != nil || resultText(res) != "ok" {
		t.Fatalf("result = %q (%v)", resultText(res), err)
	}
}

// TestAlwaysBackground: an agent marked "background: always" runs in the
// background without the model asking, given a host that runs calls there;
// with an agent marked never in the same call, or without such a host, it
// runs in the foreground.
func TestAlwaysBackground(t *testing.T) {
	f, runs, _ := backgroundFixture(t)
	writeAgent(t, f.userDir, "long.md", "---\nname: long\ndescription: Long\nbackground: always\n---\nbody")
	writeAgent(t, f.userDir, "judge.md", "---\nname: judge\ndescription: Judge\nbackground: never\n---\nbody")
	f.reg.SetResponses(steps(4, answer(func(task string) string { return "done: " + task })))

	res, err := f.run(t, map[string]any{"agent": "long", "task": "t"})
	if err != nil || len(*runs) != 1 || !res.Details.(Details).Background || !strings.HasPrefix(resultText(res), "Started in the background") {
		t.Fatalf("always: %q (%v), runs %d", resultText(res), err, len(*runs))
	}
	res, err = f.run(t, map[string]any{"chain": []any{map[string]any{"agent": "long", "task": "a"}, map[string]any{"agent": "judge", "task": "b"}}})
	if err != nil || len(*runs) != 1 || resultText(res) != "done: b" {
		t.Fatalf("always with never: %q (%v), runs %d", resultText(res), err, len(*runs))
	}
	f.opts.Background = nil
	if res, err = f.run(t, map[string]any{"agent": "long", "task": "c"}); err != nil || resultText(res) != "done: c" {
		t.Fatalf("always without a host: %q (%v)", resultText(res), err)
	}
	all := DiscoverAgents(f.userDir, "", ScopeUser).Agents
	for _, a := range all {
		if (a.Name == "long") != a.AlwaysBackground {
			t.Fatalf("AlwaysBackground of %s = %v", a.Name, a.AlwaysBackground)
		}
	}
}
