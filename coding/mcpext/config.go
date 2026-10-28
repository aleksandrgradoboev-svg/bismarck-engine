// Package mcpext is pi's built-in MCP extension (coding-agent
// src/extensions/mcp at v1.2.0, and core/mcp-servers.ts): it reads MCP server
// configuration in the mcpServers shape shared by other MCP clients, connects
// the servers, and offers their tools to a session as mcp__<server>__<tool>.
//
// pi wires extensions through its JavaScript extensions runtime, which the
// engine does not have; a host creates a Manager and hands its tools to the
// session instead.
package mcpext

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// Exposure is how a server's tools reach the model.
//
//   - codemode: callable from codemode scripts, neither declared to the model
//     nor listed in the codemode description ("codemode-deferred" is an alias);
//   - deferred: declared once the tool_search tool loads them;
//   - direct: declared to the model like any other tool;
//   - hidden: registered but unreachable.
//
// It is the session registry's coding.ToolExposure; MCP servers take every
// exposure but model-only.
type Exposure = coding.ToolExposure

// Exposures.
const (
	ExposureCodemode = coding.ToolExposureCodemode
	ExposureDeferred = coding.ToolExposureDeferred
	ExposureDirect   = coding.ToolExposureDirect
	ExposureHidden   = coding.ToolExposureHidden
)

var exposures = []Exposure{ExposureCodemode, ExposureDeferred, ExposureDirect, ExposureHidden}

// exposureAliases are older exposure names, replaced by their current name when
// a config is validated.
var exposureAliases = map[string]Exposure{"codemode-deferred": ExposureCodemode}

// ToolExposureEntry overrides the exposure of the tools a key names: a tool
// name, or a pattern where "*" matches any characters.
type ToolExposureEntry struct {
	Tool     string
	Exposure Exposure
}

// AuthConfig sends the token of a provider instead of using OAuth.
type AuthConfig struct {
	Provider string `json:"provider"`
}

// ServerConfig is one entry of mcpServers: a stdio server (Command) or a
// Streamable HTTP server (URL).
type ServerConfig struct {
	Type string `json:"type,omitempty"`
	// Stdio.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	// HTTP.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// OAuth carries pi's oauth settings as written; OAuth sign-in is not ported
	// yet.
	OAuth json.RawMessage `json:"oauth,omitempty"`
	Auth  *AuthConfig     `json:"auth,omitempty"`
	// Common.
	Exposure     Exposure            `json:"exposure,omitempty"`
	Description  string              `json:"description,omitempty"`
	ToolExposure []ToolExposureEntry `json:"-"`
	Enabled      *bool               `json:"enabled,omitempty"`
	// Timeout is the per-request timeout in seconds; progress notifications
	// reset it. Zero means 60.
	Timeout float64 `json:"timeout,omitempty"`
}

// IsHTTP reports a Streamable HTTP server.
func (c ServerConfig) IsHTTP() bool { return c.URL != "" }

// IsEnabled reports whether the server should connect (default true).
func (c ServerConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// ServerEntry is a configured server.
type ServerEntry struct {
	Name   string
	Config ServerConfig
	// Source is the config file that defined the entry.
	Source string
	// Scope is "global" or "project".
	Scope string
	// Override is the project mcp.json that overrides this global server's
	// enabled, exposure or toolExposure.
	Override string
}

// LoadedConfig is the merged global and project configuration.
type LoadedConfig struct {
	Servers []ServerEntry
	// AutoEnableCodemode activates the codemode tool when codemode servers
	// connect; nil means the default, true.
	AutoEnableCodemode *bool
	Errors             []string
	// ProjectConfig is the project mcp.json when the project is trusted.
	ProjectConfig string
}

var overrideKeys = []string{"enabled", "exposure", "toolExposure"}

var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var loopbackHosts = []string{"localhost", "127.0.0.1", "[::1]", "::1"}

// Namespace is the namespace of a server's tools: mcp__<server> with "-"
// replaced by "_", like the tool names.
func Namespace(server string) string { return "mcp__" + strings.ReplaceAll(server, "-", "_") }

// toolPattern is the regular expression of a tool name pattern where "*"
// matches any characters.
var toolPattern = coding.ToolNamePattern

// ToolNameMatcher reports whether a tool name matches any of the entries, each
// an exact name or a pattern (pi createToolNameMatcher, which lives in pi's
// core, as coding.ToolNameMatcher does here).
var ToolNameMatcher = coding.ToolNameMatcher

// ToolExposureOf is the exposure of one tool of a server: its toolExposure
// entry (an exact name wins over patterns; among patterns the first match
// wins), else the server's exposure, else codemode.
func ToolExposureOf(c ServerConfig, tool string) Exposure {
	for _, e := range c.ToolExposure {
		if e.Tool == tool {
			return e.Exposure
		}
	}
	for _, e := range c.ToolExposure {
		if strings.Contains(e.Tool, "*") && toolPattern(e.Tool).MatchString(tool) {
			return e.Exposure
		}
	}
	return ServerExposure(c)
}

// ServerExposure is the server's exposure, codemode by default.
func ServerExposure(c ServerConfig) Exposure {
	if c.Exposure == "" {
		return ExposureCodemode
	}
	return c.Exposure
}

// orderedKeys returns an object's keys in source order.
func orderedKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not an object")
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, keyTok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func jsonKind(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "undefined"
	}
	switch t[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

func resolveAlias(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if alias, ok := exposureAliases[s]; ok {
			data, _ := json.Marshal(string(alias))
			return data
		}
	}
	return raw
}

func isExposure(raw json.RawMessage) (Exposure, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return Exposure(s), slices.Contains(exposures, Exposure(s))
}

func quotedExposures() string {
	quoted := make([]string, len(exposures))
	for i, e := range exposures {
		quoted[i] = `"` + string(e) + `"`
	}
	return strings.Join(quoted, ", ")
}

func isStringMap(raw json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	for _, v := range m {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
}

// ValidateServerConfig validates one mcpServers entry (pi
// validateMcpServerConfig) and returns it with exposure aliases resolved, or
// an error naming the problem.
func ValidateServerConfig(name string, raw json.RawMessage) (ServerConfig, error) {
	fail := func(format string, args ...any) (ServerConfig, error) {
		return ServerConfig{}, fmt.Errorf(format, args...)
	}
	if !serverName.MatchString(name) {
		return fail(`invalid server name "%s" (use letters, digits, "_" and "-")`, name)
	}
	var fields map[string]json.RawMessage
	if jsonKind(raw) != "object" || json.Unmarshal(raw, &fields) != nil {
		return fail(`server "%s" must be an object`, name)
	}
	if e, ok := fields["exposure"]; ok {
		fields["exposure"] = resolveAlias(e)
		if _, valid := isExposure(fields["exposure"]); !valid {
			return fail(`server "%s": exposure must be one of %s`, name, quotedExposures())
		}
	}
	var toolExposure []ToolExposureEntry
	if te, ok := fields["toolExposure"]; ok {
		if jsonKind(te) != "object" {
			return fail(`server "%s": toolExposure must map tool names to exposures`, name)
		}
		keys, _ := orderedKeys(te)
		var values map[string]json.RawMessage
		_ = json.Unmarshal(te, &values)
		for _, tool := range keys {
			exposure, valid := isExposure(resolveAlias(values[tool]))
			if !valid {
				return fail(`server "%s": toolExposure "%s" must be one of %s`, name, tool, quotedExposures())
			}
			toolExposure = append(toolExposure, ToolExposureEntry{Tool: tool, Exposure: exposure})
		}
	}
	if e, ok := fields["enabled"]; ok && jsonKind(e) != "boolean" {
		return fail(`server "%s": enabled must be a boolean`, name)
	}
	if d, ok := fields["description"]; ok && jsonKind(d) != "string" {
		return fail(`server "%s": description must be a string`, name)
	}
	if t, ok := fields["timeout"]; ok {
		var v float64
		if jsonKind(t) != "number" || json.Unmarshal(t, &v) != nil || !(v > 0) {
			return fail(`server "%s": timeout must be a positive number of seconds`, name)
		}
	}
	var typ string
	if t, ok := fields["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	if typ == "sse" {
		return fail(`server "%s": legacy SSE transport is not supported; use the streamable HTTP URL`, name)
	}

	var cfg ServerConfig
	normalized, _ := json.Marshal(fields)
	if err := json.Unmarshal(normalized, &cfg); err != nil {
		return fail(`server "%s": %v`, name, err)
	}
	cfg.ToolExposure = toolExposure

	if jsonKind(fields["url"]) == "string" && (typ == "" || typ == "http" || typ == "streamable-http") {
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fail(`server "%s": url must be an http or https URL`, name)
		}
		if h, ok := fields["headers"]; ok && !isStringMap(h) {
			return fail(`server "%s": headers must map names to strings`, name)
		}
		if o, ok := fields["oauth"]; ok && jsonKind(o) != "object" {
			return fail(`server "%s": oauth must be an object`, name)
		}
		if a, ok := fields["auth"]; ok {
			if cfg.Auth == nil || cfg.Auth.Provider == "" || jsonKind(a) != "object" {
				return fail(`server "%s": auth.provider must be a provider name`, name)
			}
			if u.Scheme != "https" && !slices.Contains(loopbackHosts, u.Hostname()) {
				return fail(`server "%s": auth requires an https URL, or http on localhost, 127.0.0.1, or [::1]`, name)
			}
		}
		cfg.Command, cfg.Args, cfg.Env, cfg.Cwd = "", nil, nil, ""
		return cfg, nil
	}
	if jsonKind(fields["command"]) == "string" && (typ == "" || typ == "stdio") {
		if a, ok := fields["args"]; ok {
			var args []string
			if jsonKind(a) != "array" || json.Unmarshal(a, &args) != nil {
				return fail(`server "%s": args must be an array of strings`, name)
			}
		}
		if e, ok := fields["env"]; ok && !isStringMap(e) {
			return fail(`server "%s": env must map names to strings`, name)
		}
		if c, ok := fields["cwd"]; ok && jsonKind(c) != "string" {
			return fail(`server "%s": cwd must be a string`, name)
		}
		cfg.URL, cfg.Headers, cfg.OAuth, cfg.Auth = "", nil, nil, nil
		return cfg, nil
	}
	return fail(`server "%s" needs either "command" (stdio) or "url" (streamable HTTP)`, name)
}

// isOverride: an entry without command, url or type overrides a server
// defined elsewhere instead of defining one.
func isOverride(fields map[string]json.RawMessage) bool {
	_, c := fields["command"]
	_, u := fields["url"]
	_, t := fields["type"]
	return !c && !u && !t
}

type configState struct {
	servers            []ServerEntry
	raw                map[string]map[string]json.RawMessage
	autoEnableCodemode *bool
	errors             []string
}

func (s *configState) find(name string) int {
	for i, e := range s.servers {
		if e.Name == name {
			return i
		}
	}
	return -1
}

func (s *configState) set(entry ServerEntry, raw map[string]json.RawMessage) {
	if i := s.find(entry.Name); i >= 0 {
		s.servers[i] = entry
	} else {
		s.servers = append(s.servers, entry)
	}
	s.raw[entry.Name] = raw
}

func readConfigFile(path, scope string, s *configState) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.errors = append(s.errors, fmt.Sprintf("%s: %v", path, err))
		return
	}
	// A leading UTF-8 BOM, which some editors write, is not JSON (pi
	// bdd9f039, #11191: stripBom before JSON.parse).
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			s.errors = append(s.errors, fmt.Sprintf("%s: %v", path, err))
			return
		}
		s.errors = append(s.errors, path+`: expected an object with an "mcpServers" object`)
		return
	}
	servers, hasServers := top["mcpServers"]
	if top == nil || hasServers && jsonKind(servers) != "object" {
		s.errors = append(s.errors, path+`: expected an object with an "mcpServers" object`)
		return
	}
	if a, ok := top["autoEnableCodemode"]; ok {
		var v bool
		if jsonKind(a) == "boolean" && json.Unmarshal(a, &v) == nil {
			s.autoEnableCodemode = &v
		} else {
			s.errors = append(s.errors, path+": autoEnableCodemode must be a boolean")
		}
	}
	if !hasServers {
		return
	}
	names, _ := orderedKeys(servers)
	var entries map[string]json.RawMessage
	_ = json.Unmarshal(servers, &entries)
	for _, name := range names {
		value := entries[name]
		var fields map[string]json.RawMessage
		isObj := jsonKind(value) == "object" && json.Unmarshal(value, &fields) == nil
		if scope == "project" && isObj && isOverride(fields) {
			i := s.find(name)
			var extra []string
			for k := range fields {
				if !slices.Contains(overrideKeys, k) {
					extra = append(extra, k)
				}
			}
			switch {
			case i < 0:
				s.errors = append(s.errors, fmt.Sprintf(`%s: server "%s" needs "command" or "url", or a global server to override`, path, name))
			case len(extra) > 0:
				s.errors = append(s.errors, fmt.Sprintf(`%s: server "%s": an override can only set %s`, path, name, strings.Join(overrideKeys, ", ")))
			default:
				merged := map[string]json.RawMessage{}
				for k, v := range s.raw[name] {
					merged[k] = v
				}
				for k, v := range fields {
					merged[k] = v
				}
				data, _ := json.Marshal(merged)
				cfg, err := ValidateServerConfig(name, data)
				if err != nil {
					s.errors = append(s.errors, fmt.Sprintf("%s: %v", path, err))
					continue
				}
				entry := s.servers[i]
				entry.Config = cfg
				entry.Override = path
				s.set(entry, merged)
			}
			continue
		}
		cfg, err := ValidateServerConfig(name, value)
		if err != nil {
			s.errors = append(s.errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		// Names that differ only in "-" and "_" would share a namespace.
		clash := ""
		for _, other := range s.servers {
			if other.Name != name && Namespace(other.Name) == Namespace(name) {
				clash = other.Name
				break
			}
		}
		if clash != "" {
			s.errors = append(s.errors, fmt.Sprintf(`%s: server "%s" conflicts with "%s"`, path, name, clash))
			continue
		}
		if scope == "project" && cfg.IsHTTP() && cfg.Auth != nil {
			s.errors = append(s.errors, fmt.Sprintf(`%s: server "%s": auth is only allowed in the global mcp.json`, path, name))
			continue
		}
		s.set(ServerEntry{Name: name, Config: cfg, Source: path, Scope: scope}, fields)
	}
}

// LoadConfig reads the global mcp.json and, when projectTrusted, the
// project's <cwd>/.pi/mcp.json (pi loadMcpConfig). A project entry replaces a
// global entry of the same name; a project entry without command, url or type
// overrides only enabled, exposure and toolExposure of that global server.
// Disabled servers are included, so they can be enabled again.
func LoadConfig(globalPath, cwd string, projectTrusted bool) LoadedConfig {
	s := &configState{raw: map[string]map[string]json.RawMessage{}}
	if globalPath != "" {
		readConfigFile(globalPath, "global", s)
	}
	out := LoadedConfig{}
	if projectTrusted && cwd != "" {
		project := filepath.Join(cwd, ".pi", "mcp.json")
		readConfigFile(project, "project", s)
		out.ProjectConfig = project
	}
	out.Servers = s.servers
	out.AutoEnableCodemode = s.autoEnableCodemode
	out.Errors = s.errors
	return out
}
