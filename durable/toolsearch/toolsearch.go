// Package toolsearch is the durable tool_search tool (pi
// durable/src/tool-search), a copy of the coding agent's: the model searches
// the deferred tools it is not offered yet, and the matches are loaded with
// ToolControl.AddTools, so the next request offers them. Loading writes the
// conversation's modelTools (its add), so loaded tools stay offered on that
// branch, also after a compaction.
package toolsearch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/search"
)

// ToolName is the tool's name.
const ToolName = "tool_search"

// DefaultLimit is how many tools a search loads by default.
const DefaultLimit = 8

// description is fixed, so it does not change while deferred tools are
// registered, for example as MCP servers connect.
const description = "# Tool discovery\n\nSearches over deferred tool metadata with BM25 and exposes matching tools for the next model call.\n\nSome of the tools, such as tools of MCP servers, may not have been provided to you upfront, and you should use this tool (`" + ToolName + "`) to search for the required tools. For MCP tool discovery, always use `" + ToolName + "`."

// Details are a tool_search result's details: the tools it loaded.
type Details struct {
	Loaded []string `json:"loaded"`
}

// searchText is the search text of a tool: its name, the name with "_" as
// spaces, its description, and its parameters' text.
func searchText(tool *harness.ToolRegistration) string {
	parts := []string{tool.Name, strings.ReplaceAll(tool.Name, "_", " "), tool.Description}
	parts = search.SchemaText(tool.Parameters, parts)
	kept := parts[:0]
	for _, part := range parts {
		if jstext.Trim(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

// firstLine is the first line of a trimmed text.
func firstLine(text string) string {
	line, _, _ := strings.Cut(jstext.Trim(text), "\n")
	return strings.TrimSuffix(line, "\r")
}

// NewTool is the tool_search tool. Only the model calls it: searching changes
// what the model is offered, and scripts have their own searchTools().
func NewTool() *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        ToolName,
			Description: description,
			Parameters: ai.Object(
				ai.Prop("query", ai.String("Search query for deferred tools.")),
				ai.Opt("limit", ai.Number(fmt.Sprintf("Maximum number of tools to return. Defaults to %d.", DefaultLimit))),
			),
		},
		Callers: []string{harness.CallerModel},
		Replay:  harness.ReplaySafe,
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			query, _ := args["query"].(string)
			if jstext.Trim(query) == "" {
				return harness.ToolExecutionResult{}, errors.New("query must not be empty")
			}
			limit := float64(DefaultLimit)
			if value, ok := args["limit"].(float64); ok {
				limit = value
			}
			if limit != math.Trunc(limit) || math.IsInf(limit, 0) || limit <= 0 {
				return harness.ToolExecutionResult{}, errors.New("limit must be a positive integer")
			}
			agent, err := api.Agent(ctx)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			candidates := agent.Deferred
			documents := make([]search.Document, len(candidates))
			for i, tool := range candidates {
				documents[i] = search.Document{Name: tool.Name, Text: searchText(tool)}
			}
			loaded := []string{}
			for _, match := range (search.Ranker{}).Rank(query, documents, int(min(limit, math.MaxInt32))) {
				loaded = append(loaded, match.Name)
			}
			text := "No matching tools found."
			if len(loaded) > 0 {
				plural := "s"
				if len(loaded) == 1 {
					plural = ""
				}
				lines := make([]string, len(loaded))
				for i, name := range loaded {
					var desc string
					for _, tool := range candidates {
						if tool.Name == name {
							desc = tool.Description
							break
						}
					}
					lines[i] = "- " + name + ": " + firstLine(desc)
				}
				text = fmt.Sprintf("Loaded %d tool%s. They are available from your next call:\n%s", len(loaded), plural, strings.Join(lines, "\n"))
			}
			result := harness.ToolExecutionResult{
				Output:  ai.ContentList{ai.TextContent{Text: text}},
				Details: &Details{Loaded: loaded},
			}
			if len(loaded) > 0 {
				result.Control = &harness.ToolControl{AddTools: loaded}
			}
			return result, nil
		},
	}
}

// NewExtension is the tool-search extension: install it and select it where
// the model should find deferred tools. Each call builds a fresh extension.
func NewExtension() *harness.Extension {
	return &harness.Extension{Name: "tool-search", Tools: []*harness.ToolRegistration{NewTool()}}
}
