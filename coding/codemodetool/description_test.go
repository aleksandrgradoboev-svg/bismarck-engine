package codemodetool

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// fixture is testdata/description/tools.json, the input capture.mjs runs pi
// on.
type fixture struct {
	Tools []struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Parameters   json.RawMessage `json:"parameters"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	} `json:"tools"`
	Namespaces       map[string]coding.ToolNamespace `json:"namespaces"`
	Guidelines       map[string][]string             `json:"guidelines"`
	Exposures        map[string]coding.ToolExposure  `json:"exposures"`
	DescriptionCases []struct {
		Name       string   `json:"name"`
		Tools      []string `json:"tools"`
		Deferred   []string `json:"deferred"`
		Guidelines bool     `json:"guidelines"`
		Budget     *int     `json:"budget"`
	} `json:"descriptionCases"`
	LoadoutCases []struct {
		Name     string   `json:"name"`
		Mode     Mode     `json:"mode"`
		Budget   *int     `json:"budget"`
		Declared []string `json:"declared"`
		Callable []string `json:"callable"`
	} `json:"loadoutCases"`
}

// oracle is testdata/description/pi-v1.2.0.json, written by capture.mjs.
type oracle struct {
	Descriptions map[string]string `json:"descriptions"`
	Loadouts     map[string]struct {
		Descriptions       map[string]string `json:"descriptions"`
		HiddenDeclarations []string          `json:"hiddenDeclarations"`
	} `json:"loadouts"`
}

func load(t *testing.T) (fixture, oracle, map[string]agent.AgentTool) {
	t.Helper()
	var f fixture
	var o oracle
	for path, v := range map[string]any{"testdata/description/tools.json": &f, "testdata/description/pi-v1.2.0.json": &o} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	tools := map[string]agent.AgentTool{}
	for _, raw := range f.Tools {
		tool := agent.AgentTool{Name: raw.Name, Description: raw.Description}
		for _, s := range []struct {
			raw json.RawMessage
			dst **ai.Schema
		}{{raw.Parameters, &tool.Parameters}, {raw.OutputSchema, &tool.OutputSchema}} {
			if s.raw == nil {
				continue
			}
			var schema ai.Schema
			if err := json.Unmarshal(s.raw, &schema); err != nil {
				t.Fatal(err)
			}
			*s.dst = &schema
		}
		tools[raw.Name] = tool
	}
	return f, o, tools
}

func pick(tools map[string]agent.AgentTool, names []string) []agent.AgentTool {
	out := make([]agent.AgentTool, 0, len(names))
	for _, n := range names {
		out = append(out, tools[n])
	}
	return out
}

func namespaces(f fixture) map[string]*coding.ToolNamespace {
	out := map[string]*coding.ToolNamespace{}
	for name, ns := range f.Namespaces {
		out[name] = &ns
	}
	return out
}

func guidelines(f fixture) map[string][]string {
	out := map[string][]string{}
	for name, list := range f.Guidelines {
		var normalized []string
		for _, g := range list {
			if g = strings.TrimSpace(g); g != "" && !slices.Contains(normalized, g) {
				normalized = append(normalized, g)
			}
		}
		out[name] = normalized
	}
	return out
}

// TestDescriptionMatchesPi compares Description with pi's
// createCodemodeDescription on the same tools, byte for byte. The cases cover
// pi's "codemode description catalog" tests — no budget, the round-robin
// budget, deferred tools, namespace instructions, a zero budget — plus
// guidelines, output types and identifiers.
func TestDescriptionMatchesPi(t *testing.T) {
	f, o, tools := load(t)
	for _, c := range f.DescriptionCases {
		opts := DescriptionOptions{Namespaces: namespaces(f), InlineBudget: c.Budget}
		if c.Deferred != nil {
			opts.Deferred = map[string]bool{}
			for _, n := range c.Deferred {
				opts.Deferred[n] = true
			}
		}
		if c.Guidelines {
			opts.Guidelines = guidelines(f)
		}
		want, ok := o.Descriptions[c.Name]
		if !ok {
			t.Fatalf("%s: no oracle", c.Name)
		}
		if got := Description(pick(tools, c.Tools), opts); got != want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.Name, got, want)
		}
	}
}

// TestPrepareLoadoutMatchesPi compares PrepareLoadout with pi's
// prepareCodemodeLoadout in both modes: the rewritten descriptions of declared
// callable tools, the codemode description, and the hidden declarations.
func TestPrepareLoadoutMatchesPi(t *testing.T) {
	f, o, tools := load(t)
	ns := namespaces(f)
	gl := guidelines(f)
	registered := make([]agent.AgentTool, 0, len(tools))
	for _, raw := range f.Tools {
		registered = append(registered, tools[raw.Name])
	}
	for _, c := range f.LoadoutCases {
		loadout := coding.ToolLoadout{
			Declared:   pick(tools, c.Declared),
			Callable:   pick(tools, c.Callable),
			Registered: registered,
			Exposure: func(name string) coding.ToolExposure {
				if e, ok := f.Exposures[name]; ok {
					return e
				}
				return coding.ToolExposureDirect
			},
			Namespace:        func(name string) *coding.ToolNamespace { return ns[name] },
			PromptGuidelines: func(name string) []string { return gl[name] },
		}
		opts := LoadoutOptions{Mode: func() Mode { return c.Mode }}
		if c.Budget != nil {
			budget := *c.Budget
			opts.InlineBudget = func() *int { return &budget }
		}
		got := PrepareLoadout(loadout, opts)
		want := o.Loadouts[c.Name]
		for name, text := range want.Descriptions {
			if got.Descriptions[name] != text {
				t.Errorf("%s: description of %s:\n--- got\n%s\n--- want\n%s", c.Name, name, got.Descriptions[name], text)
			}
		}
		if len(got.Descriptions) != len(want.Descriptions) {
			t.Errorf("%s: descriptions for %d tools, want %d", c.Name, len(got.Descriptions), len(want.Descriptions))
		}
		if !slices.Equal(got.HiddenDeclarations, want.HiddenDeclarations) && len(got.HiddenDeclarations)+len(want.HiddenDeclarations) > 0 {
			t.Errorf("%s: hidden %v, want %v", c.Name, got.HiddenDeclarations, want.HiddenDeclarations)
		}
	}
}

// TestLoadoutInSession: the codemode tool's hook runs in a session — in only
// mode the active direct tools leave requests and the prompt's tool list, and
// codemode's declared description lists them.
func TestLoadoutInSession(t *testing.T) {
	sess := coding.NewSession(coding.SessionOptions{
		Model: &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: 100000, MaxTokens: 8192},
		Cwd:   t.TempDir(), NoContextFiles: true, NoSkills: true,
	})
	inactive := false
	codemodeTool := agent.AgentTool{Name: ToolName, Description: Description(nil, DescriptionOptions{})}
	sess.RegisterTools(coding.ToolDefinition{
		Tool: codemodeTool, Exposure: coding.ToolExposureModelOnly, DefaultActive: &inactive, PromptSnippet: Snippet,
		PrepareLoadout: func(l coding.ToolLoadout) (coding.ToolLoadoutChanges, error) {
			return PrepareLoadout(l, LoadoutOptions{Mode: func() Mode { return ModeOnly }}), nil
		},
	})
	sess.SetActiveTools([]string{ToolName, "read", "bash"})
	var description string
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == ToolName {
			description = tool.Description
		}
	}
	if !strings.Contains(description, "### `read`") || !strings.Contains(description, "### `bash`") {
		t.Fatalf("codemode description does not list read and bash:\n%s", description)
	}
	prompt, err := sess.SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "- read:") || !strings.Contains(prompt, "- codemode: "+Snippet) {
		t.Fatalf("prompt:\n%s", prompt)
	}
}
