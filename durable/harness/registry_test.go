package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pi packages/durable/test/harness-registry.test.ts.

func stubTask(t *testing.T, name string, version int) Task {
	t.Helper()
	task, err := DefineTask(TaskSpec[any, map[string]string, any]{
		Name:    name,
		Version: version,
		Initial: func(any) map[string]string { return map[string]string{"phase": "run"} },
		Phases: map[string]func(context.Context, *Running[any, map[string]string], *TaskRuntime) error{
			"run": func(context.Context, *Running[any, map[string]string], *TaskRuntime) error { return nil },
		},
		Abort: func(context.Context, *Running[any, map[string]string], *TaskRuntime) error { return nil },
	})
	must(t, err)
	return task
}

// withBuiltins stands in for the built-in tasks of later stages for one test.
func withBuiltins(t *testing.T, tasks ...Task) {
	t.Helper()
	saved := builtinTasks
	builtinTasks = tasks
	t.Cleanup(func() { builtinTasks = saved })
}

func constSection(key, text string) *PromptSection {
	return Section(key, func(context.Context, PromptInput) (string, bool, error) { return text, true, nil })
}

func toolWith(name, description string) *ToolRegistration {
	tool := testTool(name)
	tool.Description = description
	return tool
}

func TestRegistryInstallsReplacesInPlaceAndUninstallsByName(t *testing.T) {
	registry := NewRegistry()
	var published []string
	registry.Subscribe(func() {
		published = append(published, strings.Join(extensionNames(registry.Snapshot().Installed()), ","))
	})
	a := &Extension{Name: "a", Tools: []*ToolRegistration{toolWith("read", "Read files")}}
	b := &Extension{Name: "b", Tools: []*ToolRegistration{testTool("read"), testTool("bash")}}
	must(t, registry.Install(a))
	must(t, registry.Install(b))
	before := registry.Snapshot()
	// A new object with an installed name replaces it at its position.
	a2 := &Extension{Name: "a", Tools: []*ToolRegistration{testTool("grep")}}
	must(t, registry.Install(a2))
	if got := extensionNames(registry.Snapshot().Installed()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("installed = %v", got)
	}
	if registry.Snapshot().Extension("a") != a2 {
		t.Fatal("the replacement is not installed")
	}
	var tools []string
	for _, each := range registry.Snapshot().Tools() {
		tools = append(tools, each.Tool.Name+"@"+each.Extension.Name)
	}
	if want := []string{"grep@a", "read@b", "bash@b"}; !reflect.DeepEqual(tools, want) {
		t.Fatalf("tools = %v, want %v", tools, want)
	}
	// Old snapshots stay as they were.
	if before.Extension("a") != a || before.Tools()[0].Tool.Description != "Read files" {
		t.Fatal("an old snapshot changed")
	}
	// Uninstall matches the name, whichever object; a later install appends.
	registry.Uninstall(a)
	registry.Uninstall(a)
	if got := extensionNames(registry.Snapshot().Installed()); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("installed = %v", got)
	}
	must(t, registry.Install(a))
	if got := extensionNames(registry.Snapshot().Installed()); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("installed = %v", got)
	}
	if want := []string{"a", "a,b", "a,b", "b", "b,a"}; !reflect.DeepEqual(published, want) {
		t.Fatalf("published = %v, want %v", published, want)
	}
}

func TestRegistryValidatesTheStateAfterInstallAndPublishesNothingWhenInvalid(t *testing.T) {
	withBuiltins(t, stubTask(t, "pi.generation", 1))
	registry := NewRegistry()
	must(t, registry.Install(&Extension{Name: "tasks", Tasks: []Task{stubTask(t, "app.index", 1)}}))
	published := 0
	registry.Subscribe(func() { published++ })
	before := registry.Snapshot()
	invalid := []struct {
		extension *Extension
		message   string
	}{
		{&Extension{Name: "x", Tools: []*ToolRegistration{testTool("read"), testTool("read")}}, "two tools named read"},
		{&Extension{Name: "x", Sections: []*PromptSection{constSection("a", "1"), constSection("a", "2")}}, "two sections"},
		{&Extension{Name: "x", Sections: []*PromptSection{constSection("Bad Key", "")}}, "must match"},
		{&Extension{Name: "x", Sections: []*PromptSection{constSection("instructions", "")}}, "reserved"},
		{&Extension{Name: "x", Tasks: []Task{stubTask(t, "pi.generation", 1)}}, "already installed"},
		{&Extension{Name: "x", Tasks: []Task{stubTask(t, "app.index", 1)}}, "already installed"},
	}
	for _, each := range invalid {
		err := registry.Install(each.extension)
		if err == nil || !strings.Contains(err.Error(), each.message) {
			t.Fatalf("install = %v, want an error with %q", err, each.message)
		}
	}
	if registry.Snapshot() != before || published != 0 {
		t.Fatal("an invalid install published")
	}
	// Replacing the extension that holds a task name is valid: the check runs
	// on the state after replacement.
	must(t, registry.Install(&Extension{Name: "tasks", Tasks: []Task{stubTask(t, "app.index", 2)}}))
	if got := registry.Snapshot().Task("app.index").Version(); got != 2 {
		t.Fatalf("app.index version = %d, want 2", got)
	}
}

func TestRegistryAlwaysHoldsTheBuiltInTasksWhichAreNotAnExtension(t *testing.T) {
	generation, tool, compaction := stubTask(t, "pi.generation", 1), stubTask(t, "pi.tool", 1), stubTask(t, "pi.compaction", 1)
	withBuiltins(t, generation, tool, compaction)
	registry := NewRegistry()
	if len(registry.Snapshot().Installed()) != 0 {
		t.Fatal("a new registry has extensions")
	}
	if got := registry.Snapshot().Tasks(); !reflect.DeepEqual(got, []Task{generation, tool, compaction}) {
		t.Fatalf("tasks = %v", got)
	}
	custom := stubTask(t, "app.custom", 1)
	must(t, registry.Install(&Extension{Name: "custom", Tasks: []Task{custom}}))
	if got := registry.Snapshot().Tasks(); !reflect.DeepEqual(got, []Task{generation, tool, compaction, custom}) {
		t.Fatalf("tasks = %v", got)
	}
	if registry.Snapshot().Task("app.custom") != custom {
		t.Fatal("the custom task does not resolve")
	}
	registry.Uninstall(&Extension{Name: "custom"})
	if registry.Snapshot().Task("app.custom") != nil {
		t.Fatal("an uninstalled task still resolves")
	}
}

func TestSettingsKeepTheDefaultOfAnUnsetProgressInterval(t *testing.T) {
	var settings HarnessSettings
	settings.Progress.OutputInterval = ptr(250 * time.Millisecond)
	got := resolveSettings(settings).Progress
	if want := (ProgressPolicy{PartialInterval: 100 * time.Millisecond, OutputInterval: 250 * time.Millisecond}); got != want {
		t.Fatalf("progress = %+v, want %+v", got, want)
	}
}

// resolve resolves state against snapshot and settings, collecting reports.
func resolve(state *AgentState, snapshot *RegistrySnapshot, settings HarnessSettings, reports *[]error) *Agent {
	return resolveAgent(state, snapshot, resolveSettings(settings), func(err error) {
		if reports != nil {
			*reports = append(*reports, err)
		}
	})
}

type renderedSection struct {
	key, text string
	ok        bool
}

func rendered(t *testing.T, agent *Agent) []renderedSection {
	t.Helper()
	out := make([]renderedSection, len(agent.Sections))
	for i, section := range agent.Sections {
		text, ok, err := section.Render(bg, PromptInput{ConversationID: 1, Agent: agent, Shown: map[string]string{}})
		must(t, err)
		out[i] = renderedSection{section.Key, text, ok}
	}
	return out
}

type codingFixture struct {
	read, bash, edit         *ToolRegistration
	coding, skills, reviewer *Extension
	snapshot                 *RegistrySnapshot
}

func newCodingFixture(t *testing.T) codingFixture {
	t.Helper()
	f := codingFixture{read: testTool("read"), bash: testTool("bash"), edit: testTool("edit")}
	preamble := constSection("preamble", "You code.")
	preamble.Untagged = true
	f.coding = &Extension{
		Name:     "coding",
		Tools:    []*ToolRegistration{f.read, f.bash, f.edit},
		Sections: []*PromptSection{preamble, constSection("cwd", "/repo")},
	}
	f.skills = &Extension{Name: "skills", Sections: []*PromptSection{constSection("skills", "S")}}
	f.reviewer = &Extension{Name: "reviewer", Sections: []*PromptSection{constSection("role", "Review.")}}
	registry := NewRegistry()
	for _, extension := range []*Extension{f.coding, f.skills, f.reviewer} {
		must(t, registry.Install(extension))
	}
	f.snapshot = registry.Snapshot()
	return f
}

func TestAgentSelectsTheDefaultAnExactListOrTheDefaultEdited(t *testing.T) {
	f := newCodingFixture(t)
	check := func(got []*Extension, want ...string) {
		t.Helper()
		if names := extensionNames(got); !reflect.DeepEqual(names, want) {
			t.Fatalf("extensions = %v, want %v", names, want)
		}
	}
	check(resolve(nil, f.snapshot, HarnessSettings{}, nil).Extensions, "coding", "skills", "reviewer")
	settings := HarnessSettings{Extensions: []*Extension{f.coding, f.skills}}
	check(resolve(&AgentState{}, f.snapshot, settings, nil).Extensions, "coding", "skills")
	exact := Exactly("reviewer", "coding")
	check(resolve(&AgentState{Extensions: &exact}, f.snapshot, settings, nil).Extensions, "reviewer", "coding")
	// Add appends, remove drops, duplicates keep their first position,
	// uninstalled names are skipped.
	edited := Edit([]string{"reviewer", "coding", "gone"}, []string{"skills"})
	check(resolve(&AgentState{Extensions: &edited}, f.snapshot, settings, nil).Extensions, "coding", "reviewer")
	// An old object stands for its name: the installed extension is selected.
	stale := HarnessSettings{Extensions: []*Extension{{Name: "skills"}, f.skills}}
	got := resolve(&AgentState{}, f.snapshot, stale, nil).Extensions
	if len(got) != 1 || got[0] != f.skills {
		t.Fatalf("extensions = %v, want the installed skills", extensionNames(got))
	}
}

func TestAgentSkipsUninstalledNamesAndResolvesThemOnceInstalled(t *testing.T) {
	f := newCodingFixture(t)
	local := NewRegistry()
	must(t, local.Install(f.coding))
	exact := Exactly("coding", "skills")
	state := &AgentState{Extensions: &exact}
	if got := extensionNames(resolve(state, local.Snapshot(), HarnessSettings{}, nil).Extensions); !reflect.DeepEqual(got, []string{"coding"}) {
		t.Fatalf("extensions = %v", got)
	}
	must(t, local.Install(f.skills))
	if got := extensionNames(resolve(state, local.Snapshot(), HarnessSettings{}, nil).Extensions); !reflect.DeepEqual(got, []string{"coding", "skills"}) {
		t.Fatalf("extensions = %v", got)
	}
}

func TestAgentReplacesSameNameToolsInPlaceWrapsTheWinnerThenFilters(t *testing.T) {
	f := newCodingFixture(t)
	local := NewRegistry()
	venv := &Extension{Name: "venv", Tools: []*ToolRegistration{toolWith("bash", "venv bash")}}
	var calls []string
	retitle := func(suffix string) func(*ToolRegistration) (*ToolRegistration, error) {
		return func(inner *ToolRegistration) (*ToolRegistration, error) {
			wrapped := *inner
			wrapped.Description += suffix
			return &wrapped, nil
		}
	}
	timing := &Extension{Name: "timing", Wraps: []Wrap{
		WrapTool("bash", retitle(" (timed)")),
		WrapTool("bash", retitle(" [2]")),
		// No grep is selected: the wrapper does nothing and reports nothing.
		WrapTool("grep", func(*ToolRegistration) (*ToolRegistration, error) {
			calls = append(calls, "grep")
			return testTool("grep"), nil
		}),
	}}
	for _, extension := range []*Extension{f.coding, venv, timing} {
		must(t, local.Install(extension))
	}
	var reports []error
	agent := resolve(nil, local.Snapshot(), HarnessSettings{}, &reports)
	var got [][2]string
	for _, tool := range agent.Tools {
		got = append(got, [2]string{tool.Name, tool.Description})
	}
	want := [][2]string{{"read", "read tool"}, {"bash", "venv bash (timed) [2]"}, {"edit", "edit tool"}}
	if !reflect.DeepEqual(got, want) || len(reports) != 0 || len(calls) != 0 {
		t.Fatalf("tools = %v, reports = %v, calls = %v", got, reports, calls)
	}
	// An exact list keeps exactly these names in its order, a repeated name at
	// its first position.
	exact := Exactly("edit", "missing", "read", "edit")
	if got := toolNames(resolve(&AgentState{Tools: &exact}, local.Snapshot(), HarnessSettings{}, nil).Tools); !reflect.DeepEqual(got, []string{"edit", "read"}) {
		t.Fatalf("tools = %v", got)
	}
	removed := Edit(nil, []string{"bash"})
	if got := toolNames(resolve(&AgentState{Tools: &removed}, local.Snapshot(), HarnessSettings{}, nil).Tools); !reflect.DeepEqual(got, []string{"read", "edit"}) {
		t.Fatalf("tools = %v", got)
	}
}

func TestAgentDropsAToolOrSectionWhoseWrapperFailsOrRenamesIt(t *testing.T) {
	f := newCodingFixture(t)
	local := NewRegistry()
	broken := &Extension{Name: "broken", Wraps: []Wrap{
		WrapTool("read", func(*ToolRegistration) (*ToolRegistration, error) { return nil, errors.New("wrapper failed") }),
		WrapTool("edit", func(inner *ToolRegistration) (*ToolRegistration, error) {
			renamed := *inner
			renamed.Name = "renamed"
			return &renamed, nil
		}),
		// A panic is a throw.
		WrapSection("cwd", func(*PromptSection) (*PromptSection, error) { panic(errors.New("section wrapper failed")) }),
	}}
	must(t, local.Install(f.coding))
	must(t, local.Install(broken))
	var reports []error
	agent := resolve(nil, local.Snapshot(), HarnessSettings{}, &reports)
	if got := toolNames(agent.Tools); !reflect.DeepEqual(got, []string{"bash"}) {
		t.Fatalf("tools = %v", got)
	}
	if got := names(agent.Sections, func(s *PromptSection) string { return s.Key }); !reflect.DeepEqual(got, []string{"preamble"}) {
		t.Fatalf("sections = %v", got)
	}
	messages := names(reports, func(err error) string { return err.Error() })
	if want := []string{"wrapper failed", "Wrapper renamed edit to renamed", "section wrapper failed"}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("reports = %v, want %v", messages, want)
	}
}

func TestAgentOrdersSectionsByExtensionAndRendersInstructionsLastAndUnwrapped(t *testing.T) {
	f := newCodingFixture(t)
	local := NewRegistry()
	review := constSection("preamble", "You review.")
	review.Untagged = true
	override := &Extension{
		Name:     "override",
		Sections: []*PromptSection{review},
		Wraps: []Wrap{
			WrapSection("cwd", func(inner *PromptSection) (*PromptSection, error) {
				wrapped := *inner
				wrapped.Render = func(ctx context.Context, input PromptInput) (string, bool, error) {
					text, ok, err := inner.Render(ctx, input)
					return text + "!", ok, err
				}
				return &wrapped, nil
			}),
			// Instructions are not wrapped.
			WrapSection("instructions", func(*PromptSection) (*PromptSection, error) { panic("never") }),
		},
	}
	for _, extension := range []*Extension{f.coding, f.skills, override} {
		must(t, local.Install(extension))
	}
	var reports []error
	agent := resolve(&AgentState{Instructions: ptr("Be terse.")}, local.Snapshot(), HarnessSettings{}, &reports)
	want := []renderedSection{
		{"preamble", "You review.", true},
		{"cwd", "/repo!", true},
		{"skills", "S", true},
		{"instructions", "Be terse.", true},
	}
	if got := rendered(t, agent); !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	if agent.Sections[len(agent.Sections)-1].Untagged || len(reports) != 0 {
		t.Fatalf("instructions untagged or reports %v", reports)
	}
}

// Not a pi case of its own: the resolution half of harness-callers.test.ts,
// whose cases also run nested calls, which come with the tool stage, plus
// deferred tools, which pi covers through tool_search.
func TestAgentOffersAndLetsCallOnlyEnabledToolsByCallersModelToolsAndDeferral(t *testing.T) {
	tool := func(name string, edit func(*ToolRegistration)) *ToolRegistration {
		registration := testTool(name)
		if edit != nil {
			edit(registration)
		}
		return registration
	}
	script := tool("script", func(r *ToolRegistration) { r.Callers = []string{CallerModel} })
	hidden := tool("hidden", func(r *ToolRegistration) { r.Callers = []string{CallerTools} })
	both := tool("both", nil)
	lazy := tool("lazy", func(r *ToolRegistration) { r.Deferred = true })
	later := tool("later", func(r *ToolRegistration) { r.Deferred = true })
	local := NewRegistry()
	must(t, local.Install(&Extension{Name: "tools", Tools: []*ToolRegistration{script, hidden, both, lazy, later}}))
	check := func(state *AgentState, offered, callable, deferred []string) {
		t.Helper()
		agent := resolve(state, local.Snapshot(), HarnessSettings{}, nil)
		got := [3][]string{toolNames(agent.Tools), toolNames(agent.Callable), toolNames(agent.Deferred)}
		if want := [3][]string{offered, callable, deferred}; !reflect.DeepEqual(got, want) {
			t.Fatalf("offered, callable, deferred = %v, want %v", got, want)
		}
	}
	list := func(names ...string) []string { return names }
	check(nil, list("script", "both"), list("hidden", "both", "lazy", "later"), list("lazy", "later"))
	// An exact list offers deferred tools too, in its order.
	exact := Exactly("lazy", "both", "hidden")
	check(&AgentState{ModelTools: &exact}, list("lazy", "both"), list("hidden", "both", "lazy", "later"), list("later"))
	// An edit appends loaded deferred tools in add order; add has no effect on
	// tools that are not deferred or not registered.
	edit := Edit([]string{"later", "missing", "both", "later"}, []string{"script"})
	check(&AgentState{ModelTools: &edit}, list("both", "later"), list("hidden", "both", "lazy", "later"), list("lazy"))
	// ModelTools cannot bring back what Tools disabled; later stays enabled,
	// so it is still callable by tools and loadable.
	disabled := Edit(nil, []string{"both", "lazy"})
	all := Exactly("both", "lazy", "script")
	check(&AgentState{Tools: &disabled, ModelTools: &all}, list("script"), list("hidden", "later"), list("later"))
}

func TestAgentCollectsHooksOfTheSelectedExtensionsAndAppliesFieldDefaults(t *testing.T) {
	toolTask, generationTask := stubTask(t, "pi.tool", 1), stubTask(t, "pi.generation", 1)
	local := NewRegistry()
	first, second, onYield := &struct{ n int }{1}, &struct{ n int }{2}, &struct{ n int }{3}
	must(t, local.Install(&Extension{Name: "a", Hooks: []HookRegistration{Hook(toolTask, first), Hook(generationTask, onYield)}}))
	must(t, local.Install(&Extension{Name: "b", Hooks: []HookRegistration{Hook(toolTask, second)}}))
	if got := agentHooks(resolve(nil, local.Snapshot(), HarnessSettings{}, nil), "pi.tool"); !reflect.DeepEqual(got, []any{first, second}) {
		t.Fatalf("hooks = %v", got)
	}
	ba := Exactly("b", "a")
	if got := agentHooks(resolve(&AgentState{Extensions: &ba}, local.Snapshot(), HarnessSettings{}, nil), "pi.tool"); !reflect.DeepEqual(got, []any{second, first}) {
		t.Fatalf("hooks = %v", got)
	}
	b := Exactly("b")
	if got := agentHooks(resolve(&AgentState{Extensions: &b}, local.Snapshot(), HarnessSettings{}, nil), "pi.generation"); len(got) != 0 {
		t.Fatalf("hooks = %v", got)
	}

	defaults := resolve(nil, local.Snapshot(), HarnessSettings{}, nil)
	if defaults.Model != nil || defaults.ThinkingLevel != "off" || defaults.Cwd != nil {
		t.Fatalf("defaults = %+v", defaults)
	}
	set := resolve(&AgentState{Model: &ModelRef{Provider: "p", ModelID: "m"}, ThinkingLevel: "high", Cwd: ptr("/w")}, local.Snapshot(), HarnessSettings{}, nil)
	if *set.Model != (ModelRef{Provider: "p", ModelID: "m"}) || set.ThinkingLevel != "high" || *set.Cwd != "/w" {
		t.Fatalf("resolved = %+v", set)
	}
}
