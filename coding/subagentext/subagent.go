// Package subagentext is pi's subagent extension
// (coding-agent examples/extensions/subagent at v1.2.0): a subagent tool that
// delegates tasks to agents defined in markdown files, each run in a session
// of its own with an isolated context — one task, several in parallel, or a
// chain where each step gets the previous step's output.
//
// pi runs every subagent as a separate pi process and reads its JSON event
// stream. Here a subagent is a session in the same process, built by the host
// (Options.NewSession) as it builds its own, so the host decides what a
// subagent can reach: its tools, MCP servers and hooks. The terminal rendering
// of the call and result is not ported; Details carries everything it shows.
//
// Two decisions differ from pi. Project agents are repo-controlled prompts, so
// an untrusted project's agents run only when the user approves them through
// Options.Confirm: without it they are refused, where pi runs them unasked
// when it has no UI to ask with; and the tool has no confirmProjectAgents
// parameter, with which pi lets the model itself skip the question.
package subagentext

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// ToolName is the tool's name.
const ToolName = "subagent"

// Limits of the tool (pi MAX_PARALLEL_TASKS, MAX_CONCURRENCY,
// PER_TASK_OUTPUT_CAP).
const (
	MaxParallelTasks  = 8
	MaxConcurrency    = 4
	PerTaskOutputCap  = 50 * 1024
	canceledByConfirm = "Canceled: project-local agents not approved."
)

// ChildSpec is what a subagent's session is built from.
type ChildSpec struct {
	Agent Agent
	// Cwd is the working directory of the subagent.
	Cwd string
	// Model is the model to run; ThinkingLevel is set when it is inherited
	// from the dispatching session or given with the agent's model spec.
	Model         *ai.Model
	ThinkingLevel agent.ThinkingLevel
	// AppendSystemPrompt is the agent's system prompt, for
	// SessionOptions.AppendSystemPrompt.
	AppendSystemPrompt string
}

// Options configure Definition.
type Options struct {
	// NewSession builds the session a subagent runs in. Build it as the
	// dispatching session was built — the same tools, MCP servers and hooks —
	// with spec's model, thinking level, cwd and AppendSystemPrompt, and no
	// session recording (pi runs subagents with --no-session). The agent's
	// tool allowlist is applied to the result. Required.
	NewSession func(ctx context.Context, spec ChildSpec) (*coding.Session, error)
	// ResolveModel resolves an agent's model spec to a model and, when the
	// spec names one, a thinking level. Nil reports an agent with a model as
	// a failed task.
	ResolveModel func(spec string) (*ai.Model, agent.ThinkingLevel, error)
	// UserAgentsDir is the user agent directory; UserAgentsDir() is pi's.
	UserAgentsDir string
	// ProjectAgentsDir returns the project agent directory for cwd, or "";
	// NearestProjectAgentsDir is pi's. Nil means no project agents.
	ProjectAgentsDir func(cwd string) string
	// ProjectTrusted reports whether the project is trusted; nil means not.
	ProjectTrusted func() bool
	// Confirm asks the user whether to run an untrusted project's agents;
	// nil means there is no one to ask, and they are refused.
	Confirm func(ctx context.Context, title, message string) bool
}

// UsageStats is a subagent's usage (pi UsageStats).
type UsageStats struct {
	Input         int     `json:"input"`
	Output        int     `json:"output"`
	CacheRead     int     `json:"cacheRead"`
	CacheWrite    int     `json:"cacheWrite"`
	Cost          float64 `json:"cost"`
	ContextTokens int     `json:"contextTokens"`
	Turns         int     `json:"turns"`
}

// Result is one subagent run (pi SingleResult). ExitCode keeps pi's meaning
// without a process: 0 finished, 1 failed to start or run, -1 still running.
type Result struct {
	Agent        string       `json:"agent"`
	AgentSource  Source       `json:"agentSource"`
	Task         string       `json:"task"`
	ExitCode     int          `json:"exitCode"`
	Messages     []ai.Message `json:"messages"`
	Stderr       string       `json:"stderr"`
	Usage        UsageStats   `json:"usage"`
	Model        string       `json:"model,omitempty"`
	StopReason   string       `json:"stopReason,omitempty"`
	ErrorMessage string       `json:"errorMessage,omitempty"`
	Step         int          `json:"step,omitempty"`

	// cost is everything the run spent — its responses and what its tools
	// report, a nested subagent's included — for the tool result's Usage.
	// Usage above is pi's display summary and leaves tool usage out.
	cost ai.Usage
}

// addUsage adds u to dst, the token splits included.
func addUsage(dst *ai.Usage, u ai.Usage) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.CacheRead += u.CacheRead
	dst.CacheWrite += u.CacheWrite
	dst.CacheWrite1h += u.CacheWrite1h
	dst.Reasoning += u.Reasoning
	dst.TotalTokens += u.TotalTokens
	dst.Cost.Input += u.Cost.Input
	dst.Cost.Output += u.Cost.Output
	dst.Cost.CacheRead += u.Cost.CacheRead
	dst.Cost.CacheWrite += u.Cost.CacheWrite
	dst.Cost.Total += u.Cost.Total
}

// Mode is the call's mode.
type Mode string

const (
	ModeSingle   Mode = "single"
	ModeParallel Mode = "parallel"
	ModeChain    Mode = "chain"
)

// Details are the tool result's details (pi SubagentDetails).
type Details struct {
	Mode             Mode     `json:"mode"`
	AgentScope       Scope    `json:"agentScope"`
	ProjectAgentsDir *string  `json:"projectAgentsDir"`
	Results          []Result `json:"results"`
}

// Failed reports whether a run failed (pi isFailedResult).
func (r Result) Failed() bool {
	return r.ExitCode != 0 || r.StopReason == string(ai.StopError) || r.StopReason == string(ai.StopAborted)
}

// FinalOutput is the text of the last assistant message that has text (pi
// getFinalOutput: the first text part of the last such message).
func FinalOutput(messages []ai.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		am, ok := asAssistant(messages[i])
		if !ok {
			continue
		}
		for _, part := range am.Content {
			if t, ok := part.(ai.TextContent); ok {
				return t.Text
			}
		}
	}
	return ""
}

// Output is what the model gets for a run (pi getResultOutput).
func (r Result) Output() string {
	if r.Failed() {
		for _, s := range []string{r.ErrorMessage, r.Stderr, FinalOutput(r.Messages)} {
			if s != "" {
				return s
			}
		}
		return "(no output)"
	}
	if out := FinalOutput(r.Messages); out != "" {
		return out
	}
	return "(no output)"
}

func asToolResult(m ai.Message) (*ai.ToolResultMessage, bool) {
	switch v := m.(type) {
	case *ai.ToolResultMessage:
		return v, true
	case ai.ToolResultMessage:
		return &v, true
	}
	return nil, false
}

func asAssistant(m ai.Message) (*ai.AssistantMessage, bool) {
	switch v := m.(type) {
	case *ai.AssistantMessage:
		return v, true
	case ai.AssistantMessage:
		return &v, true
	}
	return nil, false
}

// truncateParallelOutput caps a parallel task's output at PerTaskOutputCap
// bytes without splitting a character (pi truncateParallelOutput).
func truncateParallelOutput(output string) string {
	if len(output) <= PerTaskOutputCap {
		return output
	}
	cut := PerTaskOutputCap
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n\n[Output truncated: %d bytes omitted. Full output preserved in tool details.]", output[:cut], len(output)-cut)
}

// Definition is the subagent tool of the session parent.
func Definition(parent *coding.Session, opts Options) coding.ToolDefinition {
	taskItem := ai.Object(
		ai.Prop("agent", ai.String("Name of the agent to invoke")),
		ai.Prop("task", ai.String("Task to delegate to the agent")),
		ai.Opt("cwd", ai.String("Working directory for the agent process")),
	)
	chainItem := ai.Object(
		ai.Prop("agent", ai.String("Name of the agent to invoke")),
		ai.Prop("task", ai.String("Task with optional {previous} placeholder for prior output")),
		ai.Opt("cwd", ai.String("Working directory for the agent process")),
	)
	scope := ai.EnumOf(string(ScopeUser), string(ScopeProject), string(ScopeBoth)).
		Describe(`Which agent directories to use. Default: "user". Use "both" to include project-local agents.`).
		WithDefault(string(ScopeUser))
	params := ai.Object(
		ai.Opt("agent", ai.String("Name of the agent to invoke (for single mode)")),
		ai.Opt("task", ai.String("Task to delegate (for single mode)")),
		ai.Opt("tasks", ai.ArrayOf(taskItem, "Array of {agent, task} for parallel execution")),
		ai.Opt("chain", ai.ArrayOf(chainItem, "Array of {agent, task} for sequential execution")),
		ai.Opt("agentScope", scope),
		ai.Opt("cwd", ai.String("Working directory for the agent process (single mode)")),
	)
	userDir := opts.UserAgentsDir
	if userDir == "" {
		userDir = "(none)"
	}
	description := strings.Join([]string{
		"Delegate tasks to specialized subagents with isolated context.",
		"Modes: single (agent + task), parallel (tasks array), chain (sequential with {previous} placeholder).",
		fmt.Sprintf(`Default agent scope is "user" (from %s).`, userDir),
		`To enable project-local agents in .pi/agents, set agentScope: "both" (or "project").`,
	}, " ")
	r := &runner{parent: parent, opts: opts}
	return coding.ToolDefinition{
		Tool: agent.AgentTool{
			Name:        ToolName,
			Label:       "Subagent",
			Description: description,
			Parameters:  params,
			Execute:     r.execute,
		},
		Source: "extension",
	}
}

type runner struct {
	parent *coding.Session
	opts   Options
}

type taskParam struct {
	agent, task, cwd string
}

func taskList(v any) []taskParam {
	items, _ := v.([]any)
	out := make([]taskParam, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]any)
		t := taskParam{}
		t.agent, _ = m["agent"].(string)
		t.task, _ = m["task"].(string)
		t.cwd, _ = m["cwd"].(string)
		out = append(out, t)
	}
	return out
}

func textResult(text string, details Details) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}, Details: details}
}

func availableAgents(agents []Agent) string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = fmt.Sprintf("%s (%s)", a.Name, a.Source)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// execute runs the call and reports what its subagents spent, failed runs
// included, as the result's Usage: the session counts a tool result's usage
// with its own, as pi's session stats do, so a host's quota sees what
// subagents spend. pi's example leaves it out.
func (r *runner) execute(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
	res, err := r.run(ctx, id, params, onUpdate)
	if d, ok := res.Details.(Details); ok && err == nil {
		var total ai.Usage
		spent := false
		for _, result := range d.Results {
			if result.cost != (ai.Usage{}) {
				addUsage(&total, result.cost)
				spent = true
			}
		}
		if spent {
			res.Usage = &total
		}
	}
	return res, err
}

func (r *runner) run(ctx context.Context, _ string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
	scope := ScopeUser
	if s, ok := params["agentScope"].(string); ok && s != "" {
		scope = Scope(s)
	}
	cwd := r.parent.Cwd
	projectDir := ""
	if r.opts.ProjectAgentsDir != nil {
		projectDir = r.opts.ProjectAgentsDir(cwd)
	}
	discovery := DiscoverAgents(r.opts.UserAgentsDir, projectDir, scope)
	agents := discovery.Agents

	chain, tasks := taskList(params["chain"]), taskList(params["tasks"])
	single := taskParam{}
	single.agent, _ = params["agent"].(string)
	single.task, _ = params["task"].(string)
	single.cwd, _ = params["cwd"].(string)
	hasChain, hasTasks, hasSingle := len(chain) > 0, len(tasks) > 0, single.agent != "" && single.task != ""

	details := func(mode Mode, results []Result) Details {
		d := Details{Mode: mode, AgentScope: scope, Results: results}
		if d.Results == nil {
			d.Results = []Result{}
		}
		if discovery.ProjectAgentsDir != "" {
			dir := discovery.ProjectAgentsDir
			d.ProjectAgentsDir = &dir
		}
		return d
	}
	mode := ModeSingle
	switch {
	case hasChain:
		mode = ModeChain
	case hasTasks:
		mode = ModeParallel
	}

	modes := 0
	for _, has := range []bool{hasChain, hasTasks, hasSingle} {
		if has {
			modes++
		}
	}
	if modes != 1 {
		return textResult("Invalid parameters. Provide exactly one mode.\nAvailable agents: "+availableAgents(agents), details(ModeSingle, nil)), nil
	}

	if (scope == ScopeProject || scope == ScopeBoth) && !r.projectTrusted() {
		var requested []string
		for _, list := range [][]taskParam{chain, tasks, {single}} {
			for _, t := range list {
				if t.agent != "" && !slices.Contains(requested, t.agent) {
					requested = append(requested, t.agent)
				}
			}
		}
		var projectAgents []string
		for _, name := range requested {
			if i := slices.IndexFunc(agents, func(a Agent) bool { return a.Name == name }); i >= 0 && agents[i].Source == SourceProject {
				projectAgents = append(projectAgents, name)
			}
		}
		if len(projectAgents) > 0 {
			// Only the user approves them; with no one to ask, they are refused.
			approved := false
			if r.opts.Confirm != nil {
				dir := discovery.ProjectAgentsDir
				if dir == "" {
					dir = "(unknown)"
				}
				approved = r.opts.Confirm(ctx, "Run project-local agents?",
					fmt.Sprintf("Agents: %s\nSource: %s\n\nProject agents are repo-controlled. Only continue for trusted repositories.", strings.Join(projectAgents, ", "), dir))
			}
			if !approved {
				return textResult(canceledByConfirm, details(mode, nil)), nil
			}
		}
	}

	switch mode {
	case ModeChain:
		return r.runChain(ctx, agents, chain, onUpdate, details)
	case ModeParallel:
		return r.runParallel(ctx, agents, tasks, onUpdate, details)
	}
	result, err := r.runAgent(ctx, agents, single, 0, func(res Result) {
		if onUpdate != nil {
			onUpdate(progress(res, details(ModeSingle, []Result{res})))
		}
	})
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	d := details(ModeSingle, []Result{result})
	if result.Failed() {
		reason := result.StopReason
		if reason == "" {
			reason = "failed"
		}
		res := textResult(fmt.Sprintf("Agent %s: %s", reason, result.Output()), d)
		res.IsError = true
		return res, nil
	}
	return textResult(finalOrNone(result.Messages), d), nil
}

func (r *runner) projectTrusted() bool {
	return r.opts.ProjectTrusted != nil && r.opts.ProjectTrusted()
}

func finalOrNone(messages []ai.Message) string {
	if out := FinalOutput(messages); out != "" {
		return out
	}
	return "(no output)"
}

// progress is the partial result a running subagent reports.
func progress(res Result, d Details) agent.AgentToolResult {
	text := FinalOutput(res.Messages)
	if text == "" {
		text = "(running...)"
	}
	return textResult(text, d)
}

func (r *runner) runChain(ctx context.Context, agents []Agent, chain []taskParam, onUpdate agent.ToolUpdateFunc, details func(Mode, []Result) Details) (agent.AgentToolResult, error) {
	var results []Result
	previous := ""
	for i, step := range chain {
		step.task = strings.ReplaceAll(step.task, "{previous}", previous)
		result, err := r.runAgent(ctx, agents, step, i+1, func(res Result) {
			if onUpdate != nil {
				all := append(slices.Clone(results), res)
				onUpdate(progress(res, details(ModeChain, all)))
			}
		})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		results = append(results, result)
		if result.Failed() {
			res := textResult(fmt.Sprintf("Chain stopped at step %d (%s): %s", i+1, step.agent, result.Output()), details(ModeChain, results))
			res.IsError = true
			return res, nil
		}
		previous = FinalOutput(result.Messages)
	}
	return textResult(finalOrNone(results[len(results)-1].Messages), details(ModeChain, results)), nil
}

func (r *runner) runParallel(ctx context.Context, agents []Agent, tasks []taskParam, onUpdate agent.ToolUpdateFunc, details func(Mode, []Result) Details) (agent.AgentToolResult, error) {
	if len(tasks) > MaxParallelTasks {
		return textResult(fmt.Sprintf("Too many parallel tasks (%d). Max is %d.", len(tasks), MaxParallelTasks), details(ModeParallel, nil)), nil
	}
	var mu sync.Mutex
	all := make([]Result, len(tasks))
	for i, t := range tasks {
		all[i] = Result{Agent: t.agent, AgentSource: SourceUnknown, Task: t.task, ExitCode: -1, Messages: []ai.Message{}}
	}
	emit := func() {
		if onUpdate == nil {
			return
		}
		snapshot := slices.Clone(all)
		running := 0
		for _, res := range snapshot {
			if res.ExitCode == -1 {
				running++
			}
		}
		onUpdate(textResult(fmt.Sprintf("Parallel: %d/%d done, %d running...", len(snapshot)-running, len(snapshot), running), details(ModeParallel, snapshot)))
	}

	var errMu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	next := 0
	workers := min(MaxConcurrency, len(tasks))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				index := next
				next++
				mu.Unlock()
				if index >= len(tasks) {
					return
				}
				result, err := r.runAgent(ctx, agents, tasks[index], 0, func(res Result) {
					mu.Lock()
					all[index] = res
					emit()
					mu.Unlock()
				})
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				mu.Lock()
				all[index] = result
				emit()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return agent.AgentToolResult{}, firstErr
	}

	succeeded := 0
	summaries := make([]string, len(all))
	for i, res := range all {
		status := "completed"
		if res.Failed() {
			status = "failed"
			if res.StopReason != "" && res.StopReason != "end" {
				status += " (" + res.StopReason + ")"
			}
		} else {
			succeeded++
		}
		summaries[i] = fmt.Sprintf("### [%s] %s\n\n%s", res.Agent, status, truncateParallelOutput(res.Output()))
	}
	text := fmt.Sprintf("Parallel: %d/%d succeeded\n\n%s", succeeded, len(all), strings.Join(summaries, "\n\n---\n\n"))
	return textResult(text, details(ModeParallel, all)), nil
}

// errAborted is returned when the call is aborted while a subagent runs (pi
// throws "Subagent was aborted").
var errAborted = errors.New("Subagent was aborted")

// runAgent runs one task (pi runSingleAgent). Failures to find the agent,
// resolve its model or build its session are failed results; only an abort is
// an error, which fails the whole call.
func (r *runner) runAgent(ctx context.Context, agents []Agent, t taskParam, step int, update func(Result)) (Result, error) {
	result := Result{Agent: t.agent, AgentSource: SourceUnknown, Task: t.task, Messages: []ai.Message{}, Step: step}
	fail := func(stderr string) (Result, error) {
		result.ExitCode = 1
		result.Stderr = stderr
		return result, nil
	}
	i := slices.IndexFunc(agents, func(a Agent) bool { return a.Name == t.agent })
	if i < 0 {
		names := make([]string, len(agents))
		for j, a := range agents {
			names[j] = `"` + a.Name + `"`
		}
		available := strings.Join(names, ", ")
		if available == "" {
			available = "none"
		}
		return fail(fmt.Sprintf("Unknown agent: %q. Available agents: %s.", t.agent, available))
	}
	def := agents[i]
	result.AgentSource = def.Source

	spec := ChildSpec{Agent: def, Cwd: t.cwd, AppendSystemPrompt: def.SystemPrompt}
	if spec.Cwd == "" {
		spec.Cwd = r.parent.Cwd
	}
	if def.Model == "" {
		state := r.parent.Agent.State()
		spec.Model, spec.ThinkingLevel = state.Model, state.ThinkingLevel
	} else {
		if r.opts.ResolveModel == nil {
			return fail(fmt.Sprintf("Agent %q names model %q, and this host cannot resolve models.", def.Name, def.Model))
		}
		model, thinking, err := r.opts.ResolveModel(def.Model)
		if err != nil {
			return fail(err.Error())
		}
		spec.Model, spec.ThinkingLevel = model, thinking
	}
	// pi passes the agent's spec as given, or the dispatching model as
	// provider/id, and reports that.
	result.Model = def.Model
	if result.Model == "" && spec.Model != nil {
		result.Model = string(spec.Model.Provider) + "/" + spec.Model.ID
	}
	if strings.TrimSpace(spec.AppendSystemPrompt) == "" {
		spec.AppendSystemPrompt = ""
	}
	if r.opts.NewSession == nil {
		return fail("This host cannot start subagent sessions.")
	}
	child, err := r.opts.NewSession(ctx, spec)
	if err != nil {
		return fail(err.Error())
	}
	if len(def.Tools) > 0 {
		match := coding.ToolNameMatcher(def.Tools)
		var active []string
		for _, d := range child.AllTools() {
			if match(d.Tool.Name) {
				active = append(active, d.Tool.Name)
			}
		}
		child.SetActiveTools(active)
	}

	var mu sync.Mutex
	unsubscribe := child.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
		if e.Type != agent.EvMessageEnd || e.Message == nil {
			return nil
		}
		mu.Lock()
		result.Messages = append(result.Messages, e.Message)
		if tr, ok := asToolResult(e.Message); ok && tr.Usage != nil {
			addUsage(&result.cost, *tr.Usage)
		}
		if am, ok := asAssistant(e.Message); ok {
			addUsage(&result.cost, am.Usage)
			result.Usage.Turns++
			result.Usage.Input += am.Usage.Input
			result.Usage.Output += am.Usage.Output
			result.Usage.CacheRead += am.Usage.CacheRead
			result.Usage.CacheWrite += am.Usage.CacheWrite
			result.Usage.Cost += am.Usage.Cost.Total
			result.Usage.ContextTokens = am.Usage.TotalTokens
			if result.Model == "" && am.Model != "" {
				result.Model = am.Model
			}
			if am.StopReason != "" {
				result.StopReason = string(am.StopReason)
			}
			if am.ErrorMessage != "" {
				result.ErrorMessage = am.ErrorMessage
			}
		}
		snapshot := result
		snapshot.Messages = slices.Clone(result.Messages)
		mu.Unlock()
		if update != nil {
			update(snapshot)
		}
		return nil
	})
	defer unsubscribe()
	stop := context.AfterFunc(ctx, child.Abort)
	defer stop()

	_, runErr := child.Run(ctx, "Task: "+t.task)
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil {
		return result, errAborted
	}
	if runErr != nil && result.ErrorMessage == "" && result.StopReason != string(ai.StopError) {
		// The run failed before any assistant message said why.
		result.ExitCode = 1
		result.Stderr = runErr.Error()
	}
	return result, nil
}
