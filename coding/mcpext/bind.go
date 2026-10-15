package mcpext

import (
	"context"
	"slices"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// Bind wires a Manager into a session as pi's MCP extension does through its
// extension API (extensions/mcp/index.ts v1.2.0): the manager's tools are
// registered with the session whenever they change; before each prompt the
// session waits, boundedly, for the servers whose tools are declared and
// lists the servers in the mcp_servers prompt section; before a tool runs it
// waits for the servers the call needs; and the codemode and tool_search
// tools are activated when the servers' exposures need them.
//
// The codemode and tool_search tools are not created here: register them with
// the session first (codemodetool.Definition, toolsearch.Definition), as pi's
// built-in extensions are; Bind activates the ones that declare ReachesTools.
//
// Bind it before Manager.Start, or right after: tools that registered earlier
// are picked up. It returns a function that unbinds.
func Bind(sess *coding.Session, m *Manager, opts BindOptions) (unbind func()) {
	b := &binder{sess: sess, m: m, warn: opts.Warn, exposures: map[string]Exposure{}}
	b.sync()
	b.ensureDiscoveryActive()
	removers := []func(){
		m.Subscribe(b.sync),
		sess.AddBeforeRun(b.beforeRun),
		sess.AddToolGate(b.gate),
	}
	return func() {
		for _, remove := range removers {
			remove()
		}
	}
}

// BindOptions configure Bind.
type BindOptions struct {
	// Warn receives the notices pi shows the user: MCP tools that nothing
	// active can reach, and servers still connecting when a prompt starts.
	Warn func(message string)
}

type binder struct {
	sess *coding.Session
	m    *Manager
	warn func(string)

	mu sync.Mutex
	// exposures are the tools' exposures as last registered.
	exposures         map[string]Exposure
	warnedUnreachable bool
}

func (b *binder) notify(message string) {
	if b.warn != nil {
		b.warn(message)
	}
}

func isDeclarable(e Exposure) bool {
	return e == ExposureDirect || e == coding.ToolExposureModelOnly
}

// sync registers the manager's tools with the session. Tools a server dropped
// stay registered as hidden, as pi re-registers them (tools cannot be
// unregistered there either). A tool whose exposure changed from declared to
// codemode or deferred leaves the active set, as pi does for the resource
// tools and for an exposure changed from the manager.
func (b *binder) sync() {
	b.mu.Lock()
	defer b.mu.Unlock()
	defs := b.m.Tools()
	converted := make([]coding.ToolDefinition, len(defs))
	var leaving []string
	for i, d := range defs {
		converted[i] = coding.ToolDefinition{Tool: d.Tool, Exposure: d.Exposure, Namespace: d.Namespace, Source: "mcp"}
		if previous, ok := b.exposures[d.Tool.Name]; ok && isDeclarable(previous) &&
			(d.Exposure == ExposureCodemode || d.Exposure == ExposureDeferred) {
			leaving = append(leaving, d.Tool.Name)
		}
		b.exposures[d.Tool.Name] = d.Exposure
	}
	b.sess.RegisterTools(converted...)
	if len(leaving) > 0 {
		active := b.sess.ActiveToolNames()
		b.sess.SetActiveTools(slices.DeleteFunc(active, func(name string) bool { return slices.Contains(leaving, name) }))
	}
}

// ensureDiscoveryActive activates the tool the configured exposures ask for:
// codemode for codemode tools unless autoEnableCodemode is false, tool_search
// for deferred ones (pi ensureDiscoveryActive). From the config, so it is
// active before the servers connect. Only a tool of that name that declares
// ReachesTools can reach MCP tools; another tool of the same name is never
// activated. When neither can reach them, it warns once.
func (b *binder) ensureDiscoveryActive() {
	needsCodemode, needsToolSearch := b.m.ConfiguredDiscovery()
	if !needsCodemode && !needsToolSearch {
		return
	}
	all := b.sess.AllTools()
	hasReaching := func(name string) bool {
		return slices.ContainsFunc(all, func(d coding.ToolDefinition) bool { return d.Tool.Name == name && d.ReachesTools != nil })
	}
	hasCodemode, hasToolSearch := hasReaching(codemodeToolName), hasReaching(toolSearchToolName)
	autoEnable := b.m.AutoEnableCodemode()
	active := b.sess.ActiveToolNames()
	var activate []string
	if needsCodemode && hasCodemode && autoEnable && !slices.Contains(active, codemodeToolName) {
		activate = append(activate, codemodeToolName)
	}
	if needsToolSearch && hasToolSearch && !slices.Contains(active, toolSearchToolName) {
		activate = append(activate, toolSearchToolName)
	}
	if len(activate) > 0 {
		b.sess.SetActiveTools(append(slices.Clone(active), activate...))
	}
	reachable := append(slices.Clone(active), activate...)
	if (hasCodemode && slices.Contains(reachable, codemodeToolName)) || (hasToolSearch && slices.Contains(reachable, toolSearchToolName)) {
		return
	}
	b.mu.Lock()
	warned := b.warnedUnreachable
	b.warnedUnreachable = true
	b.mu.Unlock()
	if warned {
		return
	}
	reason := ""
	if needsCodemode && hasCodemode && !autoEnable {
		reason = " (autoEnableCodemode is false)"
	}
	b.notify("MCP tools are only reachable from the codemode or tool_search tool, but neither is active" + reason + "; they cannot be called.")
}

// beforeRun is pi's before_agent_start handler: the first prompt waits for
// servers whose tools are declared, so they are declared in its first
// request, but not indefinitely; every prompt lists the servers as they are
// when it starts.
func (b *binder) beforeRun(ctx context.Context) {
	if !b.m.WaitForStartup(ctx) {
		b.notify("MCP servers are still connecting; their tools become available once connected.")
	}
	b.sess.SetPromptSection(ServersSectionName, b.m.ServersSection())
}

// gate is pi's tool_call handler: a tool that reaches other tools waits for
// the servers it may reach — a codemode script for the servers it names, or
// for every server when it searches or enumerates tools, and tool_search for
// every server; the resource tools wait for every server; a direct MCP call
// waits for its own server, including a reconnect still closing its old
// transport. Like pi, an abort ends the wait and the call goes on.
func (b *binder) gate(ctx context.Context, tool coding.ToolDefinition, args map[string]any) error {
	if tool.ReachesTools != nil {
		all, words := tool.ReachesTools(args)
		_ = b.m.WaitForReach(ctx, all, words)
		return nil
	}
	_ = b.m.WaitForTool(ctx, tool.Tool.Name)
	return nil
}

// The discovery tools' names (pi CODEMODE_TOOL_NAME, TOOL_SEARCH_TOOL_NAME).
// Importing codemodetool for one name would pull the codemode sandbox (wazero)
// into every MCP host, so the names are repeated here; TestDiscoveryToolNames
// ties them to the packages that define them.
const (
	codemodeToolName   = "codemode"
	toolSearchToolName = "tool_search"
)
