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
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// ToolName is the tool's name (pi TOOL_SEARCH_TOOL_NAME).
const ToolName = "tool_search"

// DefaultLimit is how many tools a search returns by default.
const DefaultLimit = 8

// Document is a tool as the ranker sees it: its name and the text built by
// NewDocument.
type Document struct {
	Name string
	Text string
}

// Match is a ranked tool.
type Match struct {
	Name  string
	Score float64
}

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
	"by": true, "for": true, "from": true, "in": true, "is": true, "it": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "this": true, "to": true, "with": true,
}

var (
	camelLower  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	camelUpper  = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	nonAlnum    = regexp.MustCompile(`[^a-z0-9]+`)
	sibilantEnd = regexp.MustCompile(`(ches|shes|sses|xes|zes)$`)
)

// stem is a naive singular form, so issues matches issue and searches matches
// search.
func stem(term string) string {
	switch {
	case len(term) > 4 && strings.HasSuffix(term, "ies"):
		return term[:len(term)-3] + "y"
	case len(term) > 4 && sibilantEnd.MatchString(term):
		return term[:len(term)-2]
	case len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss"):
		return term[:len(term)-1]
	}
	return term
}

// Tokenize splits text into lowercase terms at camelCase boundaries and
// non-alphanumerics and drops stop words. Only ASCII letters and digits make
// terms, as in pi: text in other scripts, Cyrillic included, contributes
// nothing.
func Tokenize(text string) []string {
	text = camelLower.ReplaceAllString(text, "$1 $2")
	text = camelUpper.ReplaceAllString(text, "$1 $2")
	var terms []string
	for _, term := range nonAlnum.Split(jstext.ToLower(text), -1) {
		if term != "" && !stopWords[term] {
			terms = append(terms, stem(term))
		}
	}
	return terms
}

// schemaText appends schema descriptions and property names, recursively.
func schemaText(schema *ai.Schema, parts []string) []string {
	if schema == nil {
		return parts
	}
	if schema.Description != "" {
		parts = append(parts, schema.Description)
	}
	for _, name := range schema.OrderedProperties() {
		parts = append(parts, name)
		parts = schemaText(schema.Properties[name], parts)
	}
	parts = schemaText(schema.Items, parts)
	for _, variants := range [][]*ai.Schema{schema.AnyOf, schema.OneOf, schema.AllOf} {
		for _, variant := range variants {
			parts = schemaText(variant, parts)
		}
	}
	return parts
}

// NewDocument is the search text of a tool: the name, the name with "_" as
// spaces, the description, schema descriptions and property names, and the
// namespace with its description and instructions (pi
// createToolSearchDocument).
func NewDocument(tool agent.AgentTool, namespace *coding.ToolNamespace) Document {
	parts := []string{tool.Name, strings.ReplaceAll(tool.Name, "_", " "), tool.Description}
	parts = schemaText(tool.Parameters, parts)
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

// Ranker is Okapi BM25 (pi Bm25Ranker). The zero value has k1 1.2 and b 0.75.
type Ranker struct {
	K1, B float64
}

// Rank returns the documents matching query, best first, at most limit. Ties
// keep document order.
func (r Ranker) Rank(query string, documents []Document, limit int) []Match {
	k1, b := r.K1, r.B
	if k1 == 0 {
		k1 = 1.2
	}
	if b == 0 {
		b = 0.75
	}
	var queryTerms []string
	for _, term := range Tokenize(query) {
		if !slices.Contains(queryTerms, term) {
			queryTerms = append(queryTerms, term)
		}
	}
	if len(queryTerms) == 0 || len(documents) == 0 || limit <= 0 {
		return []Match{}
	}
	termCounts := make([]map[string]int, len(documents))
	lengths := make([]float64, len(documents))
	total := 0.0
	for i, document := range documents {
		counts := map[string]int{}
		for _, term := range Tokenize(document.Text) {
			counts[term]++
		}
		termCounts[i] = counts
		for _, count := range counts {
			lengths[i] += float64(count)
		}
		total += lengths[i]
	}
	averageLength := total / float64(len(documents))
	if averageLength == 0 {
		averageLength = 1
	}
	idf := map[string]float64{}
	for _, term := range queryTerms {
		frequency := 0
		for _, counts := range termCounts {
			if counts[term] > 0 {
				frequency++
			}
		}
		idf[term] = math.Log(1 + (float64(len(documents)-frequency)+0.5)/(float64(frequency)+0.5))
	}
	matches := []Match{}
	for i, document := range documents {
		score := 0.0
		for _, term := range queryTerms {
			count := float64(termCounts[i][term])
			if count == 0 {
				continue
			}
			norm := k1 * (1 - b + (b*lengths[i])/averageLength)
			score += idf[term] * ((count * (k1 + 1)) / (count + norm))
		}
		if score > 0 {
			matches = append(matches, Match{Name: document.Name, Score: score})
		}
	}
	slices.SortStableFunc(matches, func(x, y Match) int { return cmp.Compare(y.Score, x.Score) })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
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
	matches := Ranker{}.Rank(query, documents, limit)
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
