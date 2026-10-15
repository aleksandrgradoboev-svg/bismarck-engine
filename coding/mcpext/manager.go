package mcpext

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// The server manager (pi extensions/mcp/index.ts without its TUI and its
// extension wiring). It connects the configured servers in the background
// and keeps a definition for every tool they offer, with the tool's exposure.
// Bind (bind.go) is the wiring: it registers the tools with a session, sets
// the mcp_servers section and waits for servers before prompts and calls.

// DefaultStartupWait is how long WaitForStartup waits for servers with direct
// tools.
const DefaultStartupWait = 10 * time.Second

// ServersSectionName names the system prompt section that lists the servers
// whose tools are not declared.
const ServersSectionName = "mcp_servers"

const (
	// maxServerDescriptionChars: characters of a server description in the
	// section, as Codex allows for deferred namespaces.
	maxServerDescriptionChars = 250
	// MaxServersSectionChars: characters of the whole section. Descriptions
	// shrink to fit; when the server lines alone do not fit, the last servers
	// are left out and counted in a closing line.
	MaxServersSectionChars = 4096
)

// ManagerOptions configure a Manager.
type ManagerOptions struct {
	Config LoadedConfig
	// Cwd is the session's working directory, for stdio servers and roots.
	Cwd           string
	NewTransport  TransportFactory
	ClientInfo    mcp.Implementation
	ProviderToken func(ctx context.Context, provider string) (string, error)
	// Log receives server log messages; see ServerLog.
	Log func(server string, params json.RawMessage)
	// StartupWait bounds WaitForStartup; 0 means DefaultStartupWait.
	StartupWait time.Duration
	// OnChange is called when a server's state or tools change. It runs on
	// connection goroutines, without the manager's lock.
	OnChange func()
}

type managedServer struct {
	entry ServerEntry
	conn  *Connection
	// ready is closed when the connection attempt started for the server
	// connected or failed.
	ready chan struct{}
}

// Manager connects MCP servers and adapts their tools. It is safe for
// concurrent use.
type Manager struct {
	opts ManagerOptions

	mu                 sync.Mutex
	servers            []*managedServer
	configErrors       []string
	autoEnableCodemode bool
	// toolOwners maps a tool name to the "<server>\x00<tool>" it was assigned
	// to, so names stay unique and stable.
	toolOwners map[string]string
	// serverTools are the tool names each server currently offers.
	serverTools map[string]map[string]bool
	// definitions hold the last definition of every tool name, in
	// registration order; tools a server dropped stay, as hidden.
	definitions map[string]ToolDefinition
	order       []string
	// resourceExposure is the exposure the resource tools were last
	// registered with; empty until a server has resources.
	resourceExposure Exposure
	waitedStartup    bool
	closed           bool
	// listeners are the Subscribe callbacks, guarded by listenersMu so they
	// can be called without mu.
	listenersMu  sync.Mutex
	listeners    map[int]func()
	nextListener int
}

// Subscribe adds a function called, like ManagerOptions.OnChange, when a
// server's state or tools change: on connection goroutines, without the
// manager's lock. It returns a function that removes it.
func (m *Manager) Subscribe(fn func()) (unsubscribe func()) {
	m.listenersMu.Lock()
	defer m.listenersMu.Unlock()
	if m.listeners == nil {
		m.listeners = map[int]func(){}
	}
	id := m.nextListener
	m.nextListener++
	m.listeners[id] = fn
	return func() {
		m.listenersMu.Lock()
		defer m.listenersMu.Unlock()
		delete(m.listeners, id)
	}
}

// NewManager creates a manager for the configured servers. It connects
// nothing until Start.
func NewManager(opts ManagerOptions) *Manager {
	m := &Manager{
		opts:               opts,
		configErrors:       slices.Clone(opts.Config.Errors),
		autoEnableCodemode: opts.Config.AutoEnableCodemode == nil || *opts.Config.AutoEnableCodemode,
		toolOwners:         map[string]string{},
		serverTools:        map[string]map[string]bool{},
		definitions:        map[string]ToolDefinition{},
	}
	if m.opts.StartupWait == 0 {
		m.opts.StartupWait = DefaultStartupWait
	}
	for _, e := range opts.Config.Servers {
		m.servers = append(m.servers, &managedServer{entry: e})
	}
	return m
}

func (m *Manager) changed() {
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
	m.listenersMu.Lock()
	ids := slices.Sorted(maps.Keys(m.listeners))
	fns := make([]func(), len(ids))
	for i, id := range ids {
		fns[i] = m.listeners[id]
	}
	m.listenersMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// ConfiguredDiscovery reports which discovery tools the enabled servers'
// configured exposures need, regardless of autoEnableCodemode: codemode for
// codemode tools, tool_search for deferred ones.
func (m *Manager) ConfiguredDiscovery() (codemode, toolSearch bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if !s.entry.Config.IsEnabled() {
			continue
		}
		e := configuredExposures(s.entry)
		codemode = codemode || e[ExposureCodemode]
		toolSearch = toolSearch || e[ExposureDeferred]
	}
	return codemode, toolSearch
}

// Start connects every enabled server in the background.
func (m *Manager) Start() {
	m.mu.Lock()
	var start []*managedServer
	for _, s := range m.servers {
		if s.entry.Config.IsEnabled() && s.conn == nil {
			start = append(start, s)
		}
	}
	m.mu.Unlock()
	for _, s := range start {
		m.startConnection(s)
	}
}

func (m *Manager) startConnection(s *managedServer) {
	ready := make(chan struct{})
	var conn *Connection
	conn = NewConnection(ConnectionOptions{
		Entry:         s.entry,
		Cwd:           m.opts.Cwd,
		NewTransport:  m.opts.NewTransport,
		ClientInfo:    m.opts.ClientInfo,
		ProviderToken: m.opts.ProviderToken,
		Log:           m.opts.Log,
		OnTools: func(c *Connection) {
			if m.isCurrent(s, conn) {
				m.registerTools(c)
			}
		},
		OnChange: func(*Connection) {
			if m.isCurrent(s, conn) {
				m.changed()
			}
		},
	})
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		conn.Close()
		close(ready)
		return
	}
	s.conn, s.ready = conn, ready
	m.mu.Unlock()
	m.changed()
	go func() {
		defer close(ready)
		_, _ = conn.Client(context.Background())
	}()
}

func (m *Manager) isCurrent(s *managedServer, conn *Connection) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && s.conn == conn && s.entry.Config.IsEnabled() && slices.Contains(m.servers, s)
}

func (m *Manager) findServer(name string) *managedServer {
	for _, s := range m.servers {
		if s.entry.Name == name {
			return s
		}
	}
	return nil
}

// configuredExposures are the exposures the server's tools can have, known
// from its config before it connects.
func configuredExposures(entry ServerEntry) map[Exposure]bool {
	set := map[Exposure]bool{ServerExposure(entry.Config): true}
	for _, e := range entry.Config.ToolExposure {
		set[e.Exposure] = true
	}
	return set
}

// hasDirectTools: some of the server's tools are declared to the model, so
// the first prompt waits for them.
func hasDirectTools(entry ServerEntry) bool { return configuredExposures(entry)[ExposureDirect] }

// hasIndirectTools: some of the server's tools are reached through codemode
// or tool_search.
func hasIndirectTools(entry ServerEntry) bool {
	e := configuredExposures(entry)
	return e[ExposureCodemode] || e[ExposureDeferred]
}

func (m *Manager) registerTools(conn *Connection) {
	server := conn.Name()
	m.mu.Lock()
	entry := conn.Entry()
	if s := m.findServer(server); s != nil {
		entry = s.entry
	}
	tools := conn.Tools()
	previous := m.serverTools[server]
	current := map[string]bool{}
	// Like Codex, all tools whose names sanitize to the same name get the
	// hash suffix, so which one would keep the plain name does not depend on
	// the order of the list.
	var unique []string
	for _, t := range tools {
		if !slices.Contains(unique, t.Name) {
			unique = append(unique, t.Name)
		}
	}
	plain := make([]string, len(unique))
	for i, t := range unique {
		plain[i] = ToolName(server, t, nil)
	}
	plainCount := map[string]int{}
	for _, p := range plain {
		plainCount[p]++
	}
	timeout := conn.Timeout()
	namespace := ServerNamespace(server, entry.Config.Description, conn.Instructions())
	for _, tool := range tools {
		owner := server + "\x00" + tool.Name
		name := ToolName(server, tool.Name, func(candidate string) bool {
			existing, ok := m.toolOwners[candidate]
			return (ok && existing != owner) || current[candidate] || plainCount[candidate] > 1
		})
		m.toolOwners[name] = owner
		current[name] = true
		toolName := tool.Name
		def, err := NewToolDefinition(server, tool, name, ToolExposureOf(entry.Config, tool.Name), namespace, timeout,
			func(context.Context) (ToolCaller, error) { return m.callerFor(server, toolName) },
			func() bool { return m.reachesResources(server) })
		if err != nil {
			m.configErrors = append(m.configErrors, err.Error())
			continue
		}
		m.setDefinition(def)
	}
	m.serverTools[server] = current
	// Tools the server dropped stay registered as hidden; offered again, they
	// get their configured exposure above.
	for name := range previous {
		if !current[name] {
			if def, ok := m.definitions[name]; ok {
				def.Exposure = ExposureHidden
				m.definitions[name] = def
			}
		}
	}
	m.syncResourceToolsLocked()
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) setDefinition(def ToolDefinition) {
	name := def.Tool.Name
	if _, ok := m.definitions[name]; !ok {
		m.order = append(m.order, name)
	}
	m.definitions[name] = def
}

// callerFor resolves a tool's connection when it runs: a prepared call can
// outlive the connection it was defined on.
func (m *Manager) callerFor(server, tool string) (ToolCaller, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.findServer(server)
	if s == nil || !s.entry.Config.IsEnabled() {
		return nil, fmt.Errorf("MCP server %q is disabled.", server)
	}
	if s.conn == nil {
		return nil, fmt.Errorf("MCP server %q is still starting.", server)
	}
	offered := slices.ContainsFunc(s.conn.Tools(), func(t mcp.Tool) bool { return t.Name == tool })
	if ToolExposureOf(s.entry.Config, tool) == ExposureHidden || (s.conn.State() == StateConnected && !offered) {
		return nil, fmt.Errorf("MCP tool %q is no longer available.", server+"/"+tool)
	}
	return s.conn, nil
}

// serversWithResources are the enabled servers with resources whose exposure
// is not hidden, which the resource tools reach. m.mu must be held.
func (m *Manager) serversWithResources() []*managedServer {
	var out []*managedServer
	for _, s := range m.servers {
		if s.conn != nil && s.conn.HasResources() && s.entry.Config.IsEnabled() && ServerExposure(s.entry.Config) != ExposureHidden {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) reachesResources(server string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.ContainsFunc(m.serversWithResources(), func(s *managedServer) bool { return s.entry.Name == server })
}

func (m *Manager) resourceServers() []ResourceServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ResourceServer
	for _, s := range m.serversWithResources() {
		out = append(out, s.conn)
	}
	return out
}

// syncResourceToolsLocked registers the resource tools with the widest
// exposure of the servers they reach: direct when one of them is direct, and
// so on; hidden when no server has resources. m.mu must be held.
func (m *Manager) syncResourceToolsLocked() {
	exposures := map[Exposure]bool{}
	for _, s := range m.serversWithResources() {
		exposures[ServerExposure(s.entry.Config)] = true
	}
	next := ExposureHidden
	for _, candidate := range []Exposure{ExposureDirect, ExposureCodemode, ExposureDeferred} {
		if exposures[candidate] {
			next = candidate
			break
		}
	}
	if next == m.resourceExposure || (m.resourceExposure == "" && next == ExposureHidden) {
		return
	}
	m.resourceExposure = next
	for _, def := range ResourceToolDefinitions(next, m.resourceServers) {
		m.setDefinition(def)
	}
}

// hideTools makes a disabled server's tools unreachable. m.mu must be held.
func (m *Manager) hideToolsLocked(server string) {
	for name := range m.serverTools[server] {
		if def, ok := m.definitions[name]; ok {
			def.Exposure = ExposureHidden
			m.definitions[name] = def
		}
	}
	m.serverTools[server] = map[string]bool{}
	m.syncResourceToolsLocked()
}

// Tools returns every tool definition, in registration order, with its
// current exposure. Tools a server dropped and the tools of disabled servers
// are hidden.
func (m *Manager) Tools() []ToolDefinition {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolDefinition, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.definitions[name])
	}
	return out
}

// DirectTools are the tools declared to the model: those with direct
// exposure.
func (m *Manager) DirectTools() []agent.AgentTool {
	var out []agent.AgentTool
	for _, def := range m.Tools() {
		if def.Exposure == ExposureDirect {
			out = append(out, def.Tool)
		}
	}
	return out
}

// Discovery reports which discovery tools the enabled servers' configured
// exposures ask for: codemode for codemode tools (unless autoEnableCodemode
// is false), tool_search for deferred ones. From the config, so the tools can
// be activated before the servers connect.
func (m *Manager) Discovery() (codemode, toolSearch bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if !s.entry.Config.IsEnabled() {
			continue
		}
		e := configuredExposures(s.entry)
		codemode = codemode || e[ExposureCodemode]
		toolSearch = toolSearch || e[ExposureDeferred]
	}
	return codemode && m.autoEnableCodemode, toolSearch
}

// AutoEnableCodemode reports the config's autoEnableCodemode (default true).
func (m *Manager) AutoEnableCodemode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.autoEnableCodemode
}

// ServerListing is what the mcp_servers section needs of a server.
type ServerListing struct {
	Entry ServerEntry
	// Instructions are the server's instructions once connected.
	Instructions string
}

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

// truncateUnits cuts text to max UTF-16 code units, ending with "…", as pi's
// truncate does.
func truncateUnits(text string, max int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= max {
		return text
	}
	if max <= 1 {
		return ""
	}
	return jstext.TrimEnd(string(utf16.Decode(units[:max-1]))) + "…"
}

// serverSummary is the first line of the configured description, or of the
// server instructions once connected.
func serverSummary(s ServerListing) string {
	text := jstext.Trim(s.Entry.Config.Description)
	if text == "" {
		text = s.Instructions
	}
	return jstext.Trim(strings.SplitN(text, "\n", 2)[0])
}

// RenderServersSection renders the mcp_servers section: every enabled server
// with codemode or deferred tools, with how its tools are reached and a
// one-line summary. The model learns of the servers from it, since neither
// codemode nor tool_search lists them. Empty when there are no such servers.
func RenderServersSection(servers []ServerListing) string {
	var listed []ServerListing
	for _, s := range servers {
		if s.Entry.Config.IsEnabled() && hasIndirectTools(s.Entry) {
			listed = append(listed, s)
		}
	}
	if len(listed) == 0 {
		return ""
	}
	slices.SortStableFunc(listed, func(a, b ServerListing) int { return compareServerNames(a.Entry.Name, b.Entry.Name) })
	reaches := make([]string, len(listed))
	usesCodemode, usesToolSearch := false, false
	for i, s := range listed {
		if configuredExposures(s.Entry)[ExposureCodemode] {
			reaches[i], usesCodemode = "codemode", true
		} else {
			reaches[i], usesToolSearch = "tool_search", true
		}
	}
	intro := "MCP servers whose tools are not declared to you."
	if usesCodemode {
		intro += " Call the tools of `codemode` servers from codemode scripts."
	}
	if usesToolSearch {
		intro += " Load the tools of `tool_search` servers with `tool_search`."
	}
	heads := make([]string, len(listed))
	for i, s := range listed {
		heads[i] = fmt.Sprintf("- %s (%s)", Namespace(s.Entry.Name), reaches[i])
	}
	omitted := func(count int) []string {
		if count <= 0 {
			return nil
		}
		plural := "s"
		if count == 1 {
			plural = ""
		}
		return []string{fmt.Sprintf("- … %d more server%s; find their tools with searchTools()", count, plural)}
	}
	// Characters of the intro, the first kept server lines without
	// descriptions, and the omission line.
	size := func(kept int) int {
		lines := append(append([]string{intro}, heads[:kept]...), omitted(len(listed)-kept)...)
		return utf16Length(strings.Join(lines, "\n"))
	}
	kept := len(listed)
	for kept > 0 && size(kept) > MaxServersSectionChars {
		kept--
	}
	// Each description also takes a ": " separator.
	perServer := 0
	if kept > 0 {
		perServer = min(maxServerDescriptionChars, (MaxServersSectionChars-size(kept))/kept-2)
	}
	lines := []string{intro}
	for i, s := range listed[:kept] {
		summary := ""
		if perServer > 0 {
			summary = truncateUnits(serverSummary(s), perServer)
		}
		if summary != "" {
			lines = append(lines, heads[i]+": "+summary)
		} else {
			lines = append(lines, heads[i])
		}
	}
	lines = append(lines, omitted(len(listed)-kept)...)
	return strings.Join(lines, "\n")
}

// ServersSection is the mcp_servers section for the servers as they are now;
// a host sets it as a system prompt section before every prompt, under
// ServersSectionName. Empty means no section.
func (m *Manager) ServersSection() string {
	m.mu.Lock()
	listings := make([]ServerListing, len(m.servers))
	for i, s := range m.servers {
		listings[i] = ServerListing{Entry: s.entry}
		if s.conn != nil {
			listings[i].Instructions = s.conn.Instructions()
		}
	}
	m.mu.Unlock()
	return RenderServersSection(listings)
}

// waitFor waits for the latest connection attempts of the servers chosen by
// pick, including attempts started while waiting, or for ctx.
func (m *Manager) waitFor(ctx context.Context, pick func(*managedServer) bool) error {
	for {
		m.mu.Lock()
		type attempt struct {
			s     *managedServer
			ready chan struct{}
		}
		var waiting []attempt
		for _, s := range m.servers {
			if s.entry.Config.IsEnabled() && s.ready != nil && pick(s) {
				waiting = append(waiting, attempt{s, s.ready})
			}
		}
		m.mu.Unlock()
		if len(waiting) == 0 {
			return nil
		}
		for _, a := range waiting {
			select {
			case <-a.ready:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		m.mu.Lock()
		settled := true
		for _, a := range waiting {
			if a.s.entry.Config.IsEnabled() && slices.Contains(m.servers, a.s) && a.s.ready != a.ready {
				settled = false
			}
		}
		m.mu.Unlock()
		if settled {
			return nil
		}
	}
}

// WaitForStartup waits, once per manager, for servers with direct tools, so
// their tools are declared in the first request, but at most StartupWait: a
// slow server must not hold up the prompt. It reports whether they all
// connected or failed in time; false means some are still connecting and
// their tools come later.
func (m *Manager) WaitForStartup(ctx context.Context) bool {
	m.mu.Lock()
	if m.waitedStartup {
		m.mu.Unlock()
		return true
	}
	m.waitedStartup = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.opts.StartupWait)
	defer cancel()
	return m.waitFor(ctx, func(s *managedServer) bool { return hasDirectTools(s.entry) }) == nil
}

// WaitForServers waits for the named servers (all when names is empty).
func (m *Manager) WaitForServers(ctx context.Context, names ...string) error {
	return m.waitFor(ctx, func(s *managedServer) bool { return len(names) == 0 || slices.Contains(names, s.entry.Name) })
}

// WaitForReach waits for the servers a tool that reaches other tools needs:
// every server when all is set, else those whose namespace one of the words
// contains (a codemode script naming mcp__docs, for example).
func (m *Manager) WaitForReach(ctx context.Context, all bool, words []string) error {
	return m.waitFor(ctx, func(s *managedServer) bool {
		if all {
			return true
		}
		ns := Namespace(s.entry.Name)
		return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, ns) })
	})
}

// WaitForTool waits for the server a call to tool needs: its owner for an MCP
// tool, every server for the resource tools, none for other tools.
func (m *Manager) WaitForTool(ctx context.Context, tool string) error {
	if tool == ListResourcesTool || tool == ListResourceTemplatesTool || tool == ReadResourceTool {
		return m.WaitForServers(ctx)
	}
	m.mu.Lock()
	owner, ok := m.toolOwners[tool]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return m.WaitForServers(ctx, strings.SplitN(owner, "\x00", 2)[0])
}

func firstLine(text string) string { return strings.SplitN(text, "\n", 2)[0] }

// describeState is a server's short state; withError appends the first line
// of a failure.
func describeState(s *managedServer, withError bool) string {
	if !s.entry.Config.IsEnabled() {
		return "disabled"
	}
	if s.conn == nil {
		return "starting"
	}
	switch s.conn.State() {
	case StateNeedsAuth:
		return "needs sign-in"
	case StateFailed:
		if withError {
			err := s.conn.Error()
			if err == "" {
				err = "unknown error"
			}
			return "failed: " + firstLine(err)
		}
		return "failed"
	case StateConnected:
		n := len(s.conn.Tools())
		text := fmt.Sprintf("connected · %d tool", n)
		if n != 1 {
			text += "s"
		}
		if s.conn.HasResources() {
			text += " · resources"
		}
		return text
	case StateConnecting:
		return "connecting…"
	}
	return string(s.conn.State())
}

// Problems lists what needs the user: config errors, and servers that failed
// or need a sign-in.
func (m *Manager) Problems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lines []string
	for _, e := range m.configErrors {
		lines = append(lines, "config: "+e)
	}
	for _, s := range m.servers {
		if s.conn != nil && (s.conn.State() == StateNeedsAuth || s.conn.State() == StateFailed) {
			lines = append(lines, s.entry.Name+": "+describeState(s, true))
		}
	}
	return lines
}

// Status describes every server, one line each, with errors indented below.
func (m *Manager) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.servers) == 0 && len(m.configErrors) == 0 {
		return "No MCP servers configured."
	}
	var lines []string
	for _, s := range m.servers {
		exposure := ServerExposure(s.entry.Config)
		state, tools, errText := "starting", "", ""
		switch {
		case !s.entry.Config.IsEnabled():
			state = "disabled"
		case s.conn != nil:
			switch s.conn.State() {
			case StateDisconnected:
				state = "disconnected, reconnects on next call"
			case StateNeedsAuth:
				state = "needs sign-in"
			default:
				state = string(s.conn.State())
			}
			if s.conn.State() == StateConnected {
				tools = fmt.Sprintf(", %d tools", len(s.conn.Tools()))
			} else if e := s.conn.Error(); e != "" {
				errText = "\n    " + strings.ReplaceAll(e, "\n", "\n    ")
			}
		}
		lines = append(lines, fmt.Sprintf("%s: %s%s (%s)%s", s.entry.Name, state, tools, exposure, errText))
	}
	for _, e := range m.configErrors {
		lines = append(lines, "config error: "+e)
	}
	return strings.Join(lines, "\n")
}

// Reconnect connects a server again, for example after its credentials
// changed.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	m.mu.Lock()
	s := m.findServer(name)
	if s == nil {
		m.mu.Unlock()
		return fmt.Errorf("No MCP server named %q.", name)
	}
	conn := s.conn
	previous := s.ready
	if conn == nil {
		m.mu.Unlock()
		return fmt.Errorf("MCP server %q is disabled.", name)
	}
	ready := make(chan struct{})
	s.ready = ready
	m.mu.Unlock()
	var err error
	func() {
		defer close(ready)
		if previous != nil {
			<-previous
		}
		err = conn.Reconnect(ctx)
	}()
	return err
}

// SetEnabled enables or disables a server for this manager's lifetime; it
// does not change the config file. Disabling closes the connection and hides
// the server's tools.
func (m *Manager) SetEnabled(name string, enabled bool) error {
	m.mu.Lock()
	s := m.findServer(name)
	if s == nil {
		m.mu.Unlock()
		return fmt.Errorf("No MCP server named %q.", name)
	}
	if s.entry.Config.IsEnabled() == enabled {
		m.mu.Unlock()
		return nil
	}
	s.entry.Config.Enabled = &enabled
	if enabled {
		m.mu.Unlock()
		m.startConnection(s)
		return nil
	}
	conn := s.conn
	s.conn, s.ready = nil, nil
	m.hideToolsLocked(name)
	m.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	m.changed()
	return nil
}

// Close shuts every connection down and waits until their transports are
// closed.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var conns []*Connection
	for _, s := range m.servers {
		if s.conn != nil {
			conns = append(conns, s.conn)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() { defer wg.Done(); c.Close() }()
	}
	wg.Wait()
}
