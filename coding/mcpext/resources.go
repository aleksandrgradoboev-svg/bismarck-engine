package mcpext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// MCP resources through the tools Codex and opencode use (pi extensions/mcp/
// resources.ts): list_mcp_resources, list_mcp_resource_templates and
// read_mcp_resource. They take a server argument and cover every connected
// server with resources, so models trained on those tools use them unchanged.
//
// Listings are JSON, as in Codex: {server?, resources: [{server, ...resource}],
// nextCursor?}. With a server, one page is listed and cursor continues it;
// without, every page of every server. MCP App resources (ui:// URIs and
// profile=mcp-app HTML) are left out, since they are user interfaces for hosts
// that render them. Read resources become text and images for the model;
// binary resources are saved to temp files. Scripts get the JSON payloads.

// ResourceServer is a connected server that offers resources; *Connection is
// one.
type ResourceServer interface {
	Name() string
	Timeout() time.Duration
	ResourcesPage(ctx context.Context, cursor string, opts *mcp.RequestOptions) (*mcp.ListResourcesResult, error)
	ResourceTemplatesPage(ctx context.Context, cursor string, opts *mcp.RequestOptions) (*mcp.ListResourceTemplatesResult, error)
	AllResources(ctx context.Context, opts *mcp.RequestOptions) ([]mcp.Resource, error)
	AllResourceTemplates(ctx context.Context, opts *mcp.RequestOptions) ([]mcp.ResourceTemplate, error)
	ReadResource(ctx context.Context, uri string, opts *mcp.RequestOptions) (*mcp.ReadResourceResult, error)
}

var mcpAppProfile = regexp.MustCompile(`(?i);\s*profile\s*=\s*"?mcp-app"?`)

// isMCPAppResource: MCP App user interfaces, which only hosts that render them
// can use.
func isMCPAppResource(uri, mimeType string) bool {
	return strings.HasPrefix(uri, "ui://") || mcpAppProfile.MatchString(mimeType)
}

// listedResource is a listed resource without _meta, tagged with its server.
type listedResource struct {
	Server      string          `json:"server"`
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type listedTemplate struct {
	Server      string          `json:"server"`
	URITemplate string          `json:"uriTemplate"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type listingError struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

type resourceListing struct {
	Server     string           `json:"server,omitempty"`
	Resources  []listedResource `json:"resources"`
	NextCursor string           `json:"nextCursor,omitempty"`
	Errors     []listingError   `json:"errors,omitempty"`
}

type templateListing struct {
	Server            string           `json:"server,omitempty"`
	ResourceTemplates []listedTemplate `json:"resourceTemplates"`
	NextCursor        string           `json:"nextCursor,omitempty"`
	Errors            []listingError   `json:"errors,omitempty"`
}

func mustSchema(text string) *ai.Schema {
	var s ai.Schema
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		panic(err)
	}
	return &s
}

const (
	serverFilterSchema  = `{"type":"string","description":"MCP server name. Omit to list every server with resources."}`
	cursorSchema        = `{"type":"string","description":"Opaque cursor from a previous call with the same server; omit for the first page."}`
	listingErrorsSchema = `{"type":"array","description":"Servers that could not be listed","items":{"type":"object","properties":{"server":{"type":"string"},"error":{"type":"string"}},"required":["server","error"]}}`
)

var (
	listParameters = mustSchema(`{"type":"object","properties":{"server":` + serverFilterSchema + `,"cursor":` + cursorSchema + `},"additionalProperties":false}`)
	readParameters = mustSchema(`{"type":"object","properties":{
		"server":{"type":"string","description":"MCP server name exactly as configured. Must match the 'server' field returned by list_mcp_resources."},
		"uri":{"type":"string","description":"Resource URI to read. Must be one of the URIs returned by list_mcp_resources."}},
		"required":["server","uri"],"additionalProperties":false}`)
	listOutputSchema = mustSchema(`{"type":"object","properties":{"server":{"type":"string"},"resources":{"type":"array","items":{"type":"object","properties":{
		"server":{"type":"string"},"uri":{"type":"string"},"name":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},
		"mimeType":{"type":"string"},"size":{"type":"number"}},"required":["server","uri","name"]}},
		"nextCursor":{"type":"string"},"errors":` + listingErrorsSchema + `},"required":["resources"]}`)
	listTemplatesOutputSchema = mustSchema(`{"type":"object","properties":{"server":{"type":"string"},"resourceTemplates":{"type":"array","items":{"type":"object","properties":{
		"server":{"type":"string"},"uriTemplate":{"type":"string","description":"RFC 6570 URI template"},"name":{"type":"string"},"title":{"type":"string"},
		"description":{"type":"string"},"mimeType":{"type":"string"}},"required":["server","uriTemplate","name"]}},
		"nextCursor":{"type":"string"},"errors":` + listingErrorsSchema + `},"required":["resourceTemplates"]}`)
	readOutputSchema = mustSchema(`{"type":"object","properties":{"server":{"type":"string"},"uri":{"type":"string"},"contents":{"type":"array","items":{"anyOf":[
		{"type":"object","properties":{"uri":{"type":"string"},"mimeType":{"type":"string"},"text":{"type":"string"}},"required":["uri","text"]},
		{"type":"object","properties":{"uri":{"type":"string"},"mimeType":{"type":"string"},"blob":{"type":"string","description":"base64"}},"required":["uri","blob"]}]}}},
		"required":["server","uri","contents"]}`)
)

// stringArgument reads an optional string argument: absent or null is "", a
// non-string fails, and the value is trimmed.
func stringArgument(params map[string]any, key string) (string, error) {
	v, ok := params[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return jstext.Trim(s), nil
}

// compareServerNames orders server names for listings. pi uses
// localeCompare; names are [A-Za-z0-9_-], for which ignoring case first and
// then comparing bytes gives the same order for letters and digits.
func compareServerNames(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func jsonResult(tool, server string, payload any) (agent.AgentToolResult, error) {
	text, err := jstext.Stringify(payload)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	content, fullPath := limitContent(ai.ContentList{ai.TextContent{Text: text}}, nil)
	return agent.AgentToolResult{
		Content:           content,
		Details:           ToolDetails{Server: server, Tool: tool, FullOutputPath: fullPath},
		StructuredContent: payload,
	}, nil
}

// ResourceToolDefinitions are the three resource tools with the given
// exposure. servers returns the servers they reach, at call time.
func ResourceToolDefinitions(exposure Exposure, servers func() []ResourceServer) []ToolDefinition {
	findServer := func(name string) (ResourceServer, error) {
		all := servers()
		for _, s := range all {
			if s.Name() == name {
				return s, nil
			}
		}
		names := make([]string, len(all))
		for i, s := range all {
			names[i] = s.Name()
		}
		msg := fmt.Sprintf("MCP server %q has no resources", name)
		if len(names) > 0 {
			msg += ". Servers with resources: " + strings.Join(names, ", ")
		}
		return nil, errors.New(msg)
	}
	requestOpts := func(s ResourceServer) *mcp.RequestOptions { return &mcp.RequestOptions{Timeout: s.Timeout()} }
	// sortedServers lists every server, by name, for the all-servers listings.
	sortedServers := func() []ResourceServer {
		all := slices.Clone(servers())
		slices.SortFunc(all, func(a, b ResourceServer) int { return compareServerNames(a.Name(), b.Name()) })
		return all
	}

	listResources := agent.AgentTool{
		Name:         ListResourcesTool,
		Label:        ListResourcesTool,
		Description:  "Lists resources provided by MCP servers. Resources allow servers to share data that provides context to language models, such as files, database schemas, or application-specific information. Prefer resources over web search when possible.",
		Parameters:   listParameters,
		OutputSchema: listOutputSchema,
		Execute: func(ctx context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			name, err := stringArgument(params, "server")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			cursor, err := stringArgument(params, "cursor")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			listing := resourceListing{Resources: []listedResource{}}
			add := func(server string, items []mcp.Resource) {
				for _, r := range items {
					if !isMCPAppResource(r.URI, r.MimeType) {
						listing.Resources = append(listing.Resources, listedResource{server, r.URI, r.Name, r.Title, r.Description, r.MimeType, r.Size, r.Annotations})
					}
				}
			}
			if name != "" {
				s, err := findServer(name)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				page, err := s.ResourcesPage(ctx, cursor, requestOpts(s))
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				listing.Server = s.Name()
				add(s.Name(), page.Resources)
				listing.NextCursor = page.NextCursor
			} else {
				if cursor != "" {
					return agent.AgentToolResult{}, errors.New("cursor can only be used when a server is specified")
				}
				all := sortedServers()
				results := make([][]mcp.Resource, len(all))
				errs := make([]error, len(all))
				var wg sync.WaitGroup
				for i, s := range all {
					wg.Add(1)
					go func() {
						defer wg.Done()
						results[i], errs[i] = s.AllResources(ctx, requestOpts(s))
					}()
				}
				wg.Wait()
				for i, s := range all {
					if errs[i] != nil {
						listing.Errors = append(listing.Errors, listingError{s.Name(), errs[i].Error()})
					} else {
						add(s.Name(), results[i])
					}
				}
			}
			return jsonResult(ListResourcesTool, name, listing)
		},
	}

	listTemplates := agent.AgentTool{
		Name:         ListResourceTemplatesTool,
		Label:        ListResourceTemplatesTool,
		Description:  "Lists resource templates provided by MCP servers. Parameterized resource templates allow servers to share data that takes parameters and provides context to language models, such as files, database schemas, or application-specific information. Prefer resource templates over web search when possible.",
		Parameters:   listParameters,
		OutputSchema: listTemplatesOutputSchema,
		Execute: func(ctx context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			name, err := stringArgument(params, "server")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			cursor, err := stringArgument(params, "cursor")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			listing := templateListing{ResourceTemplates: []listedTemplate{}}
			add := func(server string, items []mcp.ResourceTemplate) {
				for _, r := range items {
					if !isMCPAppResource(r.URITemplate, r.MimeType) {
						listing.ResourceTemplates = append(listing.ResourceTemplates, listedTemplate{server, r.URITemplate, r.Name, r.Title, r.Description, r.MimeType, r.Annotations})
					}
				}
			}
			if name != "" {
				s, err := findServer(name)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				page, err := s.ResourceTemplatesPage(ctx, cursor, requestOpts(s))
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				listing.Server = s.Name()
				add(s.Name(), page.ResourceTemplates)
				listing.NextCursor = page.NextCursor
			} else {
				if cursor != "" {
					return agent.AgentToolResult{}, errors.New("cursor can only be used when a server is specified")
				}
				all := sortedServers()
				results := make([][]mcp.ResourceTemplate, len(all))
				errs := make([]error, len(all))
				var wg sync.WaitGroup
				for i, s := range all {
					wg.Add(1)
					go func() {
						defer wg.Done()
						results[i], errs[i] = s.AllResourceTemplates(ctx, requestOpts(s))
					}()
				}
				wg.Wait()
				for i, s := range all {
					if errs[i] != nil {
						listing.Errors = append(listing.Errors, listingError{s.Name(), errs[i].Error()})
					} else {
						add(s.Name(), results[i])
					}
				}
			}
			return jsonResult(ListResourceTemplatesTool, name, listing)
		},
	}

	readResource := agent.AgentTool{
		Name:         ReadResourceTool,
		Label:        ReadResourceTool,
		Description:  "Read a specific resource from an MCP server given the server name and resource URI.",
		Parameters:   readParameters,
		OutputSchema: readOutputSchema,
		Execute: func(ctx context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			name, err := stringArgument(params, "server")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			uri, err := stringArgument(params, "uri")
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if name == "" {
				return agent.AgentToolResult{}, errors.New("server must be provided")
			}
			if uri == "" {
				return agent.AgentToolResult{}, errors.New("uri must be provided")
			}
			s, err := findServer(name)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			result, err := s.ReadResource(ctx, uri, requestOpts(s))
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			// Several contents (for example a directory) are labeled with their URIs.
			var converted ai.ContentList
			contents := make([]mcp.ResourceContents, len(result.Contents))
			for i, c := range result.Contents {
				if len(result.Contents) > 1 {
					converted = append(converted, ai.TextContent{Text: c.URI + ":"})
				}
				resource := c
				converted = append(converted, blockToContent(s.Name(), mcp.ContentBlock{Type: "resource", Resource: &resource}, ConvertOptions{})...)
				c.Meta = nil
				contents[i] = c
			}
			if len(converted) == 0 {
				converted = ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Resource %s is empty.", uri)}}
			}
			content, fullPath := limitContent(converted, nil)
			return agent.AgentToolResult{
				Content: content,
				Details: ToolDetails{Server: s.Name(), Tool: ReadResourceTool, FullOutputPath: fullPath},
				StructuredContent: struct {
					Server   string                 `json:"server"`
					URI      string                 `json:"uri"`
					Contents []mcp.ResourceContents `json:"contents"`
				}{s.Name(), uri, contents},
			}, nil
		},
	}

	defs := []agent.AgentTool{listResources, listTemplates, readResource}
	out := make([]ToolDefinition, len(defs))
	for i, t := range defs {
		out[i] = ToolDefinition{Tool: t, Exposure: exposure}
	}
	return out
}
