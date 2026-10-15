package coding

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// The session's tool registry, ported from pi AgentSession (agent-session.ts
// v1.2.0: getActiveToolNames .. _applyToolLoadout, _rebuildSystemPrompt,
// _refreshToolRegistry, _isDeclarable, _isActivatedOnRegistration,
// _installHiddenDeclarationsProjection, _restoreToolsFromTranscript). Every
// registered tool has an exposure; the active tools are the ones declared to the
// model, and the callable tools — the ones ExecuteTool reaches — are the active
// direct tools plus every codemode and deferred tool. Active tools with a
// PrepareLoadout hook can change the declared descriptions and hide
// declarations from requests.
//
// pi re-applies the loadout before every request; here every change to it goes
// through applyLocked, which runs the hooks, so the result is the same.

// ToolExposure says how a registered tool is reached (pi ToolExposure).
type ToolExposure string

// Tool exposures.
const (
	// ToolExposureDirect tools are declared to the model when active and
	// callable from other tools while active. The default.
	ToolExposureDirect ToolExposure = "direct"
	// ToolExposureModelOnly tools are declared to the model when active and
	// never callable from other tools.
	ToolExposureModelOnly ToolExposure = "model-only"
	// ToolExposureCodemode tools are never declared; codemode scripts call them.
	ToolExposureCodemode ToolExposure = "codemode"
	// ToolExposureDeferred tools are callable from other tools and declared once
	// tool_search loads them.
	ToolExposureDeferred ToolExposure = "deferred"
	// ToolExposureHidden tools are registered but never declared or callable.
	ToolExposureHidden ToolExposure = "hidden"
)

// ToolDefinition is a tool registered with a Session (pi ToolDefinition's
// registry half).
type ToolDefinition struct {
	Tool agent.AgentTool
	// Exposure is how the tool is reached; empty means direct.
	Exposure ToolExposure
	// DefaultActive false keeps a direct or model-only tool inactive when it
	// registers; nil means true.
	DefaultActive *bool
	// PromptSnippet is the tool's one-line entry in the tools section of the
	// default prompt; a tool without one is not listed there, even when it
	// replaces a built-in of the same name.
	PromptSnippet string
	// Source names where the tool came from, for AllTools.
	Source string
	// Namespace is the group the tool belongs to, for example its MCP server.
	Namespace *ToolNamespace
	// PrepareLoadout adjusts how the loadout is presented to the model while
	// the tool is active; it runs whenever the active tools change. Tools that
	// orchestrate other tools use it, for example to list the callable tools in
	// their own description. It runs with the registry locked, so it must not
	// call back into the Session; what it needs is in the ToolLoadout. A
	// returned error drops its changes and goes to
	// SessionOptions.OnPrepareLoadoutError.
	PrepareLoadout func(ToolLoadout) (ToolLoadoutChanges, error)
	// ReachesTools, for tools that reach other tools — codemode calls them,
	// tool_search loads them — says which tools a call with args may reach:
	// all of them, or the words of the input that may name them, where a word
	// refers to a tool or namespace whose name it contains (pi
	// ToolDefinition.reachesTools). Hosts that register tools late, such as MCP
	// servers still connecting, use it to finish registering them first.
	ReachesTools func(args map[string]any) (all bool, words []string)
}

// ToolGate runs before a tool executes, outside the agent loop's locks (see
// Session.AddToolGate). An error fails the call with its message.
type ToolGate func(ctx context.Context, tool ToolDefinition, args map[string]any) error

// ToolNamespace is a group of related tools, such as the tools of one MCP
// server (pi ToolNamespace).
type ToolNamespace struct {
	// Name is, for example, mcp__docs.
	Name string
	// Aliases are other names that refer to the namespace where tools look it
	// up by name, for example docs.
	Aliases []string
	// Description is a short summary shown once with the group in model-facing
	// tool listings.
	Description string
	// Instructions are longer usage guidance, such as MCP server instructions,
	// returned by tools that describe the namespace on request.
	Instructions string
}

// ToolLoadout is the session's tools as a PrepareLoadout hook sees them (pi
// ToolLoadout).
type ToolLoadout struct {
	// Declared are the active tools, in order, with their original
	// descriptions.
	Declared []agent.AgentTool
	// Callable are the tools ExecuteTool reaches with this loadout.
	Callable []agent.AgentTool
	// Registered is every registered tool.
	Registered []agent.AgentTool
	Exposure   func(name string) ToolExposure
	Namespace  func(name string) *ToolNamespace
	// PromptGuidelines are a tool's normalized guidelines. Hidden declarations
	// leave them out of the system prompt.
	PromptGuidelines func(name string) []string
}

// ToolLoadoutChanges are what a PrepareLoadout hook changes in what the model
// sees (pi ToolLoadoutChanges).
type ToolLoadoutChanges struct {
	// Descriptions are model-facing descriptions of declared tools, by name.
	Descriptions map[string]string
	// HiddenDeclarations are declared tools whose declarations requests leave
	// out. They stay active and callable, and the transcript still declares
	// them, so the active set survives a resume.
	HiddenDeclarations []string
}

func (d ToolDefinition) exposure() ToolExposure {
	if d.Exposure == "" {
		return ToolExposureDirect
	}
	return d.Exposure
}

// toolRegistry is the Session's registry state, guarded by Session.toolsMu.
type toolRegistry struct {
	defs  map[string]ToolDefinition
	order []string
	// active are the active tool names in loadout order.
	active []string
	// isAllowed is the --tools/--exclude-tools filter registrations pass
	// (pi _isAllowedTool); allowlisted is nil without an allowlist.
	isAllowed   func(string) bool
	allowlisted func(string) bool
	// pending are tools of a restored loadout that are not registered yet,
	// such as tools of MCP servers still connecting (pi _pendingToolNames).
	// They are activated when they register, and dropped when SetActiveTools
	// deactivates a tool or the next run starts.
	pending map[string]bool
	// hidden are the declared tools whose declarations requests leave out,
	// from PrepareLoadout hooks (pi _hiddenDeclarations).
	hidden map[string]bool
	// onError is SessionOptions.OnPrepareLoadoutError.
	onError func(tool string, err error)
	// gates run before every tool executes (AddToolGate); beforeRun before
	// every prompt (AddBeforeRun). Keyed so a host can remove its own.
	gates     map[int]ToolGate
	beforeRun map[int]func(context.Context)
	nextHook  int
}

func (r *toolRegistry) exposureOf(name string) ToolExposure {
	if d, ok := r.defs[name]; ok {
		return d.exposure()
	}
	return ToolExposureDirect
}

// isDeclarable is pi _isDeclarable: activating the tool declares it.
func (r *toolRegistry) isDeclarable(name string) bool {
	e := r.exposureOf(name)
	return e == ToolExposureDirect || e == ToolExposureModelOnly
}

// isActivatedOnRegistration is pi _isActivatedOnRegistration.
func (r *toolRegistry) isActivatedOnRegistration(name string) bool {
	d, ok := r.defs[name]
	return ok && r.isDeclarable(name) && (d.DefaultActive == nil || *d.DefaultActive)
}

// isActivatable is pi _isActivatable: whether the tool may be active, which
// declares it. MCP tools the allowlist keeps without matching them are only
// for codemode and tool_search: they may be declared only when tool_search can
// load them — their exposure is not direct and tool_search is registered. This
// also applies to tools restored from the transcript or set with
// SetActiveTools.
func (r *toolRegistry) isActivatable(name string) bool {
	if r.allowlisted == nil || r.allowlisted(name) || !IsMCPToolName(name) {
		return true
	}
	_, hasToolSearch := r.defs["tool_search"]
	return r.exposureOf(name) != ToolExposureDirect && hasToolSearch
}

// loadout is pi _applyToolLoadout without prepareLoadout hooks: the
// registered, non-hidden, activatable tools among names, deduplicated in
// first-occurrence order.
func (r *toolRegistry) loadout(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := r.defs[name]; !ok || slices.Contains(out, name) || r.exposureOf(name) == ToolExposureHidden || !r.isActivatable(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// newToolRegistry seeds the registry from the session's initial tools, all
// direct, with the initial loadout.
func newToolRegistry(resolved resolvedTools) *toolRegistry {
	r := &toolRegistry{defs: map[string]ToolDefinition{}, isAllowed: resolved.isAllowed, allowlisted: resolved.allowlisted, pending: map[string]bool{}}
	for _, d := range resolved.defs {
		r.defs[d.Tool.Name] = d
		r.order = append(r.order, d.Tool.Name)
	}
	r.active = r.loadout(resolved.active)
	for _, name := range resolved.pending {
		r.pending[name] = true
	}
	return r
}

// AddToolGate adds a gate that runs before every tool of the session
// executes — model calls and nested ones alike — outside the agent loop's
// locks, after argument validation and the BeforeToolCall hook. pi's MCP
// extension waits in its tool_call hook for the servers a call needs; the
// engine's hooks run under the loop's tool lock, so waiting there would stall
// the other tools of a parallel batch. It returns a function that removes the
// gate.
func (s *Session) AddToolGate(gate ToolGate) (remove func()) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if s.tools.gates == nil {
		s.tools.gates = map[int]ToolGate{}
	}
	id := s.tools.nextHook
	s.tools.nextHook++
	s.tools.gates[id] = gate
	return func() {
		s.toolsMu.Lock()
		defer s.toolsMu.Unlock()
		delete(s.tools.gates, id)
	}
}

// AddBeforeRun adds a function that runs before each prompt the session runs
// (Run, RunMessages, RunPrint), before the prompt's system prompt is declared
// — where pi's before_agent_start handlers run, for example to wait for MCP
// servers and set the mcp_servers section. It returns a function that removes
// it.
func (s *Session) AddBeforeRun(fn func(ctx context.Context)) (remove func()) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if s.tools.beforeRun == nil {
		s.tools.beforeRun = map[int]func(context.Context){}
	}
	id := s.tools.nextHook
	s.tools.nextHook++
	s.tools.beforeRun[id] = fn
	return func() {
		s.toolsMu.Lock()
		defer s.toolsMu.Unlock()
		delete(s.tools.beforeRun, id)
	}
}

// runBeforeRun runs the AddBeforeRun functions in the order they were added.
func (s *Session) runBeforeRun(ctx context.Context) {
	s.toolsMu.RLock()
	ids := slices.Sorted(maps.Keys(s.tools.beforeRun))
	fns := make([]func(context.Context), len(ids))
	for i, id := range ids {
		fns[i] = s.tools.beforeRun[id]
	}
	s.toolsMu.RUnlock()
	for _, fn := range fns {
		fn(ctx)
	}
}

// gated is the tool of d with the session's gates run before it executes.
func (s *Session) gated(d ToolDefinition) agent.AgentTool {
	tool := d.Tool
	execute := tool.Execute
	if execute == nil {
		return tool
	}
	tool.Execute = func(ctx context.Context, id string, args map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		s.toolsMu.RLock()
		ids := slices.Sorted(maps.Keys(s.tools.gates))
		gates := make([]ToolGate, len(ids))
		for i, k := range ids {
			gates[i] = s.tools.gates[k]
		}
		s.toolsMu.RUnlock()
		for _, gate := range gates {
			if err := gate(ctx, d, args); err != nil {
				return agent.AgentToolResult{}, err
			}
		}
		return execute(ctx, id, args, onUpdate)
	}
	return tool
}

// callable is pi _getCallableTools for the active set active: the active direct
// tools and every codemode or deferred tool, in registration order.
func (r *toolRegistry) callable(active []string) []agent.AgentTool {
	var out []agent.AgentTool
	for _, name := range r.order {
		switch r.exposureOf(name) {
		case ToolExposureCodemode, ToolExposureDeferred:
		case ToolExposureDirect:
			if !slices.Contains(active, name) {
				continue
			}
		default:
			continue
		}
		out = append(out, r.defs[name].Tool)
	}
	return out
}

// prepare runs the PrepareLoadout hooks of the active tools, in loadout order,
// and returns the declared tools — the active ones with the descriptions the
// hooks set — and the hidden declarations (the hook half of pi
// _applyToolLoadout). A hook that fails or panics contributes nothing.
func (r *toolRegistry) prepare(tools []agent.AgentTool) ([]agent.AgentTool, map[string]bool) {
	hidden := map[string]bool{}
	var hooks []string
	for _, tool := range tools {
		if r.defs[tool.Name].PrepareLoadout != nil {
			hooks = append(hooks, tool.Name)
		}
	}
	if len(hooks) == 0 {
		return tools, hidden
	}
	registered := make([]agent.AgentTool, 0, len(r.order))
	for _, name := range r.order {
		registered = append(registered, r.defs[name].Tool)
	}
	loadout := ToolLoadout{
		Declared:   slices.Clone(tools),
		Callable:   r.callable(r.active),
		Registered: registered,
		Exposure:   r.exposureOf,
		Namespace: func(name string) *ToolNamespace {
			return r.defs[name].Namespace
		},
		PromptGuidelines: func(name string) []string {
			return normalizePromptGuidelines(r.defs[name].Tool.PromptGuidelines)
		},
	}
	descriptions := map[string]string{}
	for _, name := range hooks {
		changes, err := runPrepareLoadout(r.defs[name].PrepareLoadout, loadout)
		if err != nil {
			if r.onError != nil {
				r.onError(name, err)
			}
			continue
		}
		maps.Copy(descriptions, changes.Descriptions)
		for _, h := range changes.HiddenDeclarations {
			hidden[h] = true
		}
	}
	declared := make([]agent.AgentTool, len(tools))
	for i, tool := range tools {
		if description, ok := descriptions[tool.Name]; ok {
			tool.Description = description
		}
		declared[i] = tool
	}
	return declared, hidden
}

// runPrepareLoadout calls a hook, turning a panic into an error as pi's
// try/catch turns a throw into an extension error.
func runPrepareLoadout(hook func(ToolLoadout) (ToolLoadoutChanges, error), loadout ToolLoadout) (changes ToolLoadoutChanges, err error) {
	defer func() {
		if p := recover(); p != nil {
			changes, err = ToolLoadoutChanges{}, fmt.Errorf("prepareLoadout panicked: %v", p)
		}
	}()
	return hook(loadout)
}

// applyLocked sets the active tools to the loadout of names and rebuilds the
// prompt options from the registry (pi _setActiveTools → _applyToolLoadout +
// _rebuildSystemPrompt). Tools that become active are no longer pending. The
// caller holds toolsMu for writing.
func (s *Session) applyLocked(names []string) {
	r := s.tools
	r.active = r.loadout(names)
	tools := make([]agent.AgentTool, 0, len(r.active))
	for _, name := range r.active {
		tools = append(tools, s.gated(r.defs[name]))
		delete(r.pending, name)
	}
	declared, hidden := r.prepare(tools)
	r.hidden = hidden
	s.Agent.SetTools(declared)

	// _rebuildSystemPrompt: snippets and guidelines of every registered tool;
	// the builder shows only the selected ones. Everything else the options
	// carry — context, skills, custom prompt and sections — stays as it is.
	snippets := map[string]string{}
	guidelines := map[string][]string{}
	for _, name := range r.order {
		d := r.defs[name]
		if snippet := normalizePromptSnippet(d.PromptSnippet); snippet != "" {
			snippets[name] = snippet
		}
		if g := normalizePromptGuidelines(d.Tool.PromptGuidelines); len(g) > 0 {
			guidelines[name] = g
		}
	}
	opts := s.systemPromptOptions
	opts.SelectedTools = slices.Clone(r.active)
	// The tool list and rules must match the declarations requests carry
	// (upstream 028c0ec5).
	opts.HiddenTools = nil
	for _, name := range r.active {
		if hidden[name] {
			opts.HiddenTools = append(opts.HiddenTools, name)
		}
	}
	opts.ToolSnippets = snippets
	opts.ToolGuidelines = guidelines
	s.systemPromptOptions = NormalizeBuildSystemPromptOptions(opts)
}

// normalizePromptSnippet is pi _normalizePromptSnippet: line breaks become
// spaces, runs of JS whitespace (\s) collapse to one space, and the result is
// trimmed.
func normalizePromptSnippet(text string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range text {
		if r == '\r' || r == '\n' || jstext.IsWhitespace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return jstext.Trim(b.String())
}

// RegisterTools adds tools to the registry, replacing same-named ones, and
// refreshes the loadout as pi's _refreshToolRegistry does without options:
// the active tools stay active; a tool that is now activated on registration
// and was not before is activated — a new one, or one whose exposure changed
// to direct or model-only; with an allowlist, every allowlisted declarable tool
// is activated instead. Tools the --tools/--exclude-tools filter rejects are
// not registered. Changes reach the model on the next turn.
func (s *Session) RegisterTools(defs ...ToolDefinition) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	r := s.tools
	previous := map[string]bool{}
	for _, name := range r.order {
		if r.isActivatedOnRegistration(name) {
			previous[name] = true
		}
	}
	for _, d := range defs {
		name := d.Tool.Name
		if name == "" || !r.isAllowed(name) {
			continue
		}
		if _, ok := r.defs[name]; !ok {
			r.order = append(r.order, name)
		}
		r.defs[name] = d
	}
	s.refreshLocked(previous)
}

// UnregisterTools removes tools from the registry; an active one stops being
// declared on the next turn.
func (s *Session) UnregisterTools(names ...string) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	r := s.tools
	previous := map[string]bool{}
	for _, name := range r.order {
		if r.isActivatedOnRegistration(name) {
			previous[name] = true
		}
	}
	for _, name := range names {
		delete(r.defs, name)
	}
	r.order = slices.DeleteFunc(r.order, func(name string) bool { _, ok := r.defs[name]; return !ok })
	s.refreshLocked(previous)
}

// refreshLocked is the activation half of pi _refreshToolRegistry.
func (s *Session) refreshLocked(previouslyActivated map[string]bool) {
	r := s.tools
	next := slices.Clone(r.active)
	if r.allowlisted != nil {
		for _, name := range r.order {
			if r.allowlisted(name) && r.isDeclarable(name) {
				next = append(next, name)
			}
		}
	} else {
		for _, name := range r.order {
			if !previouslyActivated[name] && r.isActivatedOnRegistration(name) {
				next = append(next, name)
			}
		}
	}
	// Pending tools that are registered now become active.
	for _, name := range r.order {
		if r.pending[name] {
			next = append(next, name)
		}
	}
	s.applyLocked(next)
}

// SetActiveTools sets the tools declared to the model, in order (pi
// setActiveToolsByName). Unknown and hidden names are ignored. The system
// prompt follows the new set; both reach the model on the next turn. A loadout
// that deactivates a tool replaces a restored one, whose pending tools are
// dropped; one that only adds tools keeps them.
func (s *Session) SetActiveTools(names []string) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	previous := s.tools.active
	s.applyLocked(names)
	for _, name := range previous {
		if !slices.Contains(s.tools.active, name) {
			clear(s.tools.pending)
			return
		}
	}
}

// RestoreToolsFromTranscript activates the loadout the transcript declares,
// if it declares one (pi _restoreToolsFromTranscript, which pi runs after
// /tree navigation). Declared tools that are not registered yet stay pending
// and are activated when they register, until the next run starts. Tools
// reachable only from other tools are never declared, but they do not depend on
// the active set, so the declarations are the whole loadout.
//
// A resumed session does not call it: pi's createAgentSession always passes
// the initial tools, which take precedence over the transcript's.
func (s *Session) RestoreToolsFromTranscript() {
	messages := s.Agent.State().Messages
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	r := s.tools
	clear(r.pending)
	current, found := ai.GetCurrentSystemMessage(messages)
	if !found {
		return
	}
	names := make([]string, 0, len(current.ToolsAdded))
	for _, tool := range current.ToolsAdded {
		names = append(names, tool.Name)
		if r.isAllowed(tool.Name) {
			r.pending[tool.Name] = true
		}
	}
	s.applyLocked(names)
}

// PendingToolNames are the restored tools still waiting to register.
func (s *Session) PendingToolNames() []string {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return slices.Sorted(maps.Keys(s.tools.pending))
}

// dropPendingTools drops the restored tools that did not register by the
// time a run starts, so a tool that never registers does not stay pending
// (pi _startRun).
func (s *Session) dropPendingTools() {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	clear(s.tools.pending)
}

// ActiveToolNames are the tools declared to the model (pi getActiveToolNames).
func (s *Session) ActiveToolNames() []string {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return slices.Clone(s.tools.active)
}

// CallableTools are the tools ExecuteTool can call (pi _getCallableTools): the
// active direct tools and every registered codemode or deferred tool, in
// registration order.
func (s *Session) CallableTools() []agent.AgentTool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	tools := s.tools.callable(s.tools.active)
	for i, t := range tools {
		tools[i] = s.gated(s.tools.defs[t.Name])
	}
	return tools
}

// projectHiddenDeclarations is pi _installHiddenDeclarationsProjection: the
// declarations PrepareLoadout hooks hide are removed from every request. The
// whole transcript is filtered with the current set, so the projected
// declarations stay consistent across requests and only change when the
// loadout does. The transcript itself keeps them.
func (s *Session) projectHiddenDeclarations(messages []agent.AgentMessage) []agent.AgentMessage {
	s.toolsMu.RLock()
	hidden := s.tools.hidden
	s.toolsMu.RUnlock()
	if len(hidden) == 0 {
		return messages
	}
	out := make([]agent.AgentMessage, len(messages))
	for i, message := range messages {
		out[i] = message
		var system ai.SystemMessage
		switch m := message.(type) {
		case ai.SystemMessage:
			system = m
		case *ai.SystemMessage:
			if m == nil {
				continue
			}
			system = *m
		default:
			continue
		}
		if len(system.ToolsAdded) == 0 && len(system.ToolsRemoved) == 0 {
			continue
		}
		var added []ai.Tool
		for _, tool := range system.ToolsAdded {
			if !hidden[tool.Name] {
				added = append(added, tool)
			}
		}
		var removed []ai.ToolReference
		for _, tool := range system.ToolsRemoved {
			if !hidden[tool.Name] {
				removed = append(removed, tool)
			}
		}
		system.ToolsAdded, system.ToolsRemoved = added, removed
		out[i] = system
	}
	return out
}

// AllTools are the registered tools in registration order, each with its
// effective exposure (pi getAllTools).
func (s *Session) AllTools() []ToolDefinition {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	out := make([]ToolDefinition, 0, len(s.tools.order))
	for _, name := range s.tools.order {
		d := s.tools.defs[name]
		d.Exposure = d.exposure()
		out = append(out, d)
	}
	return out
}

// SetPromptSection sets a custom system prompt section, or removes it when
// content is empty — what pi's MCP extension does to systemPromptOptions
// before each run for its mcp_servers section. The name must be a valid
// section name (see BuildSystemPromptOptions.Sections). The change reaches the
// model on the next turn.
func (s *Session) SetPromptSection(name, content string) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	// A fresh copy: a snapshot taken by promptOptions may share the old one.
	sections := slices.Clone(s.systemPromptOptions.Sections)
	if content != "" {
		sections.Set(name, &content)
	} else {
		sections.Delete(name)
	}
	s.systemPromptOptions.Sections = sections
}

// promptOptions is a snapshot of the prompt options, taken under toolsMu.
func (s *Session) promptOptions() BuildSystemPromptOptions {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.systemPromptOptions
}
