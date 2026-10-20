package subagentext

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// Agent discovery (pi examples/extensions/subagent/agents.ts).

// Scope is which agent directories a call uses.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeBoth    Scope = "both"
)

// Source is where an agent was defined.
type Source string

const (
	SourceUser    Source = "user"
	SourceProject Source = "project"
	// SourceUnknown marks a result for an agent that was not found or has not
	// started yet.
	SourceUnknown Source = "unknown"
)

// Agent is an agent definition: a markdown file whose frontmatter names and
// describes the agent and whose body is its system prompt.
type Agent struct {
	Name        string
	Description string
	// Tools is the agent's tool allowlist; nil keeps the session's tools.
	Tools []string
	// Model is a model spec; empty inherits the dispatching session's model
	// and thinking level.
	Model        string
	SystemPrompt string
	Source       Source
	FilePath     string
	// NoBackground is set by "background: never": the agent is not run in
	// the background (Options.Background), as its caller cannot go on
	// without its result. AlwaysBackground is set by "background: always":
	// with a host that runs calls in the background, a call of the agent runs
	// there whatever the model asks, unless it also calls an agent that is
	// never. Not in pi.
	NoBackground     bool
	AlwaysBackground bool
}

// Discovery is the result of DiscoverAgents.
type Discovery struct {
	Agents []Agent
	// ProjectAgentsDir is the project agent directory found for the call, or
	// "" when there is none.
	ProjectAgentsDir string
}

// UserAgentsDir is pi's user agent directory, <agent dir>/agents; "" when
// there is no agent directory.
func UserAgentsDir() string {
	dir := coding.AgentDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "agents")
}

// NearestProjectAgentsDir is pi's project agent directory: the nearest .pi/agents
// at or above cwd, or "" when there is none.
func NearestProjectAgentsDir(cwd string) string {
	dir := cwd
	for {
		candidate := filepath.Join(dir, ".pi", "agents")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// parseToolList reads a frontmatter tools value. pi accepts a comma-separated
// string and a YAML array; ParseFrontmatter returns a flow array such as
// [read, bash] as its text, so the brackets are taken off here. Anything else
// yields no tools rather than an error: one bad file must not hide the other
// agents in its directory.
func parseToolList(v coding.FrontmatterValue) []string {
	text := v.Value
	if !v.IsString {
		return nil
	}
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = text[1 : len(text)-1]
	}
	var tools []string
	for _, t := range strings.Split(text, ",") {
		t = strings.Trim(strings.TrimSpace(t), `"'`)
		if t != "" {
			tools = append(tools, t)
		}
	}
	return tools
}

func loadAgentsFromDir(dir string, source Source) []Agent {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var agents []Agent
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		// pi takes files and symlinks; a symlink to a directory fails the read.
		if !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fm, body := coding.ParseFrontmatter(string(data))
		name, description := fm["name"], fm["description"]
		if !name.IsString || !description.IsString {
			continue
		}
		agent := Agent{
			Name:         name.Value,
			Description:  description.Value,
			Tools:        parseToolList(fm["tools"]),
			SystemPrompt: body,
			Source:       source,
			FilePath:     path,
		}
		if model := fm["model"]; model.IsString {
			agent.Model = model.Value
		}
		if bg := fm["background"]; bg.IsString {
			switch strings.TrimSpace(bg.Value) {
			case "never":
				agent.NoBackground = true
			case "always":
				agent.AlwaysBackground = true
			}
		}
		agents = append(agents, agent)
	}
	return agents
}

// DiscoverAgents loads the agents of scope from userDir and projectDir. With
// ScopeBoth a project agent replaces a user agent of the same name. Agents are
// read on every call, so they can be edited while a session runs.
func DiscoverAgents(userDir, projectDir string, scope Scope) Discovery {
	var user, project []Agent
	if scope != ScopeProject {
		user = loadAgentsFromDir(userDir, SourceUser)
	}
	if scope != ScopeUser {
		project = loadAgentsFromDir(projectDir, SourceProject)
	}
	var agents []Agent
	index := map[string]int{}
	add := func(list []Agent) {
		for _, a := range list {
			if i, ok := index[a.Name]; ok {
				agents[i] = a
				continue
			}
			index[a.Name] = len(agents)
			agents = append(agents, a)
		}
	}
	add(user)
	add(project)
	return Discovery{Agents: agents, ProjectAgentsDir: projectDir}
}
