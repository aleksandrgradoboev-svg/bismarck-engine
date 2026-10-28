// Package toolsearch is pi's tool discovery (coding-agent
// src/extensions/tool-search at v1.2.0): a BM25 ranker over tool metadata,
// shared by searchTools() in codemode scripts and the optional tool_search
// tool.
//
// tool_search searches the tools that are not declared to the model (codemode
// and deferred exposure) and loads the matches, so they are declared for the
// next model call. Loading goes through the session's active tool set, so it is
// recorded in the transcript like any other tool change.
//
// pi registers it through its extension runtime; here a host registers
// Definition with the session.
package toolsearch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/search"
)

// ToolName is the tool's name (pi TOOL_SEARCH_TOOL_NAME).
const ToolName = "tool_search"

// DefaultLimit is how many tools a search returns by default.
const DefaultLimit = 8

// The ranking lives in internal/search, shared with the durable tool_search.
type (
	// Document is a tool as the ranker sees it: its name and the text built
	// by NewDocument.
	Document = search.Document
	// Match is a ranked tool.
	Match = search.Match
	// Ranker is Okapi BM25 (pi Bm25Ranker). The zero value has k1 1.2 and b
	// 0.75.
	Ranker = search.Ranker
)

// Tokenize splits text into lowercase terms at camelCase boundaries and
// non-alphanumerics and drops stop words.
func Tokenize(text string) []string { return search.Tokenize(text) }

// NewDocument is the search text of a tool: the name, the name with "_" as
// spaces, the description, schema descriptions and property names, and the
// namespace with its description and instructions (pi
// createToolSearchDocument).
func NewDocument(tool agent.AgentTool, namespace *coding.ToolNamespace) Document {
	parts := []string{tool.Name, strings.ReplaceAll(tool.Name, "_", " "), tool.Description}
	parts = search.SchemaText(tool.Parameters, parts)
	if namespace != nil {
		parts = append(parts, namespace.Name, namespace.Description, namespace.Instructions)
	}
	kept := parts[:0]
	for _, part := range parts {
		if jstext.Trim(part) != "" {
			kept = append(kept, part)
		}
	}
	return Document{Name: tool.Name, Text: strings.Join(kept, " ")}
}

// Tools is the session as tool_search uses it; *coding.Session fits.
type Tools interface {
	AllTools() []coding.ToolDefinition
	ActiveToolNames() []string
	SetActiveTools(names []string)
}

// Details are the tool result's details (pi ToolSearchToolDetails).
type Details struct {
	// Loaded are the tools this call loaded.
	Loaded []string `json:"loaded"`
}

// isSearchable reports whether tool_search can load a tool with this exposure.
func isSearchable(exposure coding.ToolExposure) bool {
	return exposure == coding.ToolExposureCodemode || exposure == coding.ToolExposureDeferred
}

type loadedTool struct{ name, description string }

// searchAndLoad ranks the searchable tools that are not active yet and
// activates the matches, so the next model call declares them.
func searchAndLoad(tools Tools, query string, limit int) []loadedTool {
	active := tools.ActiveToolNames()
	var candidates []coding.ToolDefinition
	var documents []Document
	for _, d := range tools.AllTools() {
		if isSearchable(d.Exposure) && !slices.Contains(active, d.Tool.Name) {
			candidates = append(candidates, d)
			documents = append(documents, NewDocument(d.Tool, d.Namespace))
		}
	}
	matches := Ranker{NamesFirst: true}.Rank(query, documents, limit)
	if len(matches) > 0 {
		next := slices.Clone(active)
		for _, m := range matches {
			next = append(next, m.Name)
		}
		tools.SetActiveTools(next)
	}
	out := make([]loadedTool, 0, len(matches))
	for _, m := range matches {
		description := ""
		for _, d := range candidates {
			if d.Tool.Name == m.Name {
				description = d.Tool.Description
				break
			}
		}
		out = append(out, loadedTool{name: m.Name, description: description})
	}
	return out
}

// Description is the tool's description. It does not list the searchable
// tools or their namespaces, so it stays the same while tools are registered,
// for example when MCP servers connect.
const Description = "# Tool discovery\n\nSearches over deferred tool metadata with BM25 and exposes matching tools for the next model call.\n\nSome of the tools, such as tools of MCP servers, may not have been provided to you upfront, and you should use this tool (`" + ToolName + "`) to search for the required tools. For MCP tool discovery, always use `" + ToolName + "`."

// firstLine is pi's `text.trim().split(/\r?\n/)[0]`.
func firstLine(text string) string {
	text = jstext.Trim(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSuffix(text[:i], "\r")
	}
	return text
}

// Definition is the tool_search tool for the session tools (pi
// createToolSearchToolDefinition plus createToolSearchExtension). It is
// model-only — scripts have no use for searching, which changes what the model
// sees — and registered inactive: activate it by name. With nil tools it finds
// nothing.
func Definition(tools Tools) coding.ToolDefinition {
	inactive := false
	tool := agent.AgentTool{
		Name:        ToolName,
		Label:       ToolName,
		Description: Description,
		Parameters: ai.Object(
			ai.Prop("query", ai.String("Search query for deferred tools.")),
			ai.Opt("limit", ai.Number(fmt.Sprintf("Maximum number of tools to return. Defaults to %d.", DefaultLimit))),
		),
		Execute: func(_ context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			query, _ := params["query"].(string)
			if jstext.Trim(query) == "" {
				return agent.AgentToolResult{}, errors.New("query must not be empty")
			}
			limit := float64(DefaultLimit)
			if v, ok := params["limit"]; ok && v != nil {
				n, isNumber := v.(float64)
				if !isNumber {
					return agent.AgentToolResult{}, errors.New("limit must be a positive integer")
				}
				limit = n
			}
			if math.IsInf(limit, 0) || math.IsNaN(limit) || limit != math.Trunc(limit) || limit <= 0 {
				return agent.AgentToolResult{}, errors.New("limit must be a positive integer")
			}
			var loaded []loadedTool
			if tools != nil {
				loaded = searchAndLoad(tools, query, int(math.Min(limit, math.MaxInt32)))
			}
			text := "No matching tools found."
			if len(loaded) > 0 {
				plural := "s"
				if len(loaded) == 1 {
					plural = ""
				}
				lines := make([]string, len(loaded))
				for i, t := range loaded {
					lines[i] = "- " + t.name + ": " + firstLine(t.description)
				}
				text = fmt.Sprintf("Loaded %d tool%s. They are available from your next call:\n%s", len(loaded), plural, strings.Join(lines, "\n"))
			}
			details := Details{Loaded: make([]string, 0, len(loaded))}
			for _, t := range loaded {
				details.Loaded = append(details.Loaded, t.name)
			}
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}, Details: details}, nil
		},
	}
	return coding.ToolDefinition{
		Tool:          tool,
		Exposure:      coding.ToolExposureModelOnly,
		DefaultActive: &inactive,
		PromptSnippet: "Search for tools that are not loaded yet and load the matches",
		Source:        "builtin",
		// A query can match any tool that is not declared yet.
		ReachesTools: func(map[string]any) (bool, []string) { return true, nil },
	}
}
