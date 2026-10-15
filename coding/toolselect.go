package coding

import (
	"regexp"
	"slices"
	"strings"
)

// Tool selection, ported from pi core: createToolNameMatcher and
// isMcpToolName (core/mcp-servers.ts), isToolModifier, getToolListError and
// applyToolModifiers (core/settings-manager.ts), upstream ddaa0a03 and
// 04b97ef0.

// MCP resource tool names (Codex's), which reach every server with resources.
const (
	ListMCPResourcesTool         = "list_mcp_resources"
	ListMCPResourceTemplatesTool = "list_mcp_resource_templates"
	ReadMCPResourceTool          = "read_mcp_resource"
)

// IsMCPToolName reports whether a tool comes from MCP: a server tool
// (mcp__<server>__<tool>) or a resource tool.
func IsMCPToolName(name string) bool {
	return strings.HasPrefix(name, "mcp__") || name == ListMCPResourcesTool ||
		name == ListMCPResourceTemplatesTool || name == ReadMCPResourceTool
}

// ToolNamePattern is the regular expression of a tool name pattern where "*"
// matches any characters, as toolExposure, ToolNames and ExcludeTools take
// them.
func ToolNamePattern(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// ToolNameMatcher reports whether a tool name matches any of the entries, each
// an exact name or a pattern (pi createToolNameMatcher).
func ToolNameMatcher(entries []string) func(name string) bool {
	names := map[string]bool{}
	var patterns []*regexp.Regexp
	for _, e := range entries {
		if strings.Contains(e, "*") {
			patterns = append(patterns, ToolNamePattern(e))
		} else {
			names[e] = true
		}
	}
	return func(name string) bool {
		if names[name] {
			return true
		}
		for _, p := range patterns {
			if p.MatchString(name) {
				return true
			}
		}
		return false
	}
}

// IsToolModifier reports whether a tool selection entry is a +name or -name
// modifier.
func IsToolModifier(entry string) bool {
	return strings.HasPrefix(entry, "+") || strings.HasPrefix(entry, "-")
}

// ToolListError validates a tool list such as SessionOptions.ToolNames (pi
// getToolListError): either an allowlist of plain names and patterns or a list
// of only +name/-name entries with exact names. It returns the problem, or ""
// when the list is valid.
func ToolListError(entries []string) string {
	var modifiers []string
	for _, e := range entries {
		if IsToolModifier(e) {
			modifiers = append(modifiers, e)
		}
	}
	if len(modifiers) == 0 {
		return ""
	}
	if len(modifiers) < len(entries) {
		return "tool names cannot be mixed with +name or -name entries"
	}
	for _, e := range modifiers {
		if strings.Contains(e, "*") {
			return "+name and -name entries take exact tool names, not patterns: " + e
		}
	}
	return ""
}

// ApplyToolModifiers applies the +name and -name entries of entries to base in
// order: +name adds a tool and -name removes one. Other entries are ignored
// (pi applyToolModifiers).
func ApplyToolModifiers(base, entries []string) []string {
	tools := slices.Clone(base)
	for _, entry := range entries {
		if !IsToolModifier(entry) {
			continue
		}
		name := entry[1:]
		index := slices.Index(tools, name)
		if strings.HasPrefix(entry, "+") && index == -1 && name != "" {
			tools = append(tools, name)
		} else if strings.HasPrefix(entry, "-") && index != -1 {
			tools = slices.Delete(tools, index, index+1)
		}
	}
	return tools
}
