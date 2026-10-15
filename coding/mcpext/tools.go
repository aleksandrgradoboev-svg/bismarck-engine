package mcpext

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// maxToolNameLength: provider tool names are limited to 64 characters of
// [A-Za-z0-9_-].
const maxToolNameLength = 64

// OutputMaxBytes caps model-facing text of an MCP result; longer text is cut
// in the middle.
const OutputMaxBytes = 20 * 1024

// Resource tool names (Codex's), which reach every server with resources.
// The core owns them, as pi's core/mcp-servers.ts does, because the session's
// allowlist treats MCP tools apart.
const (
	ListResourcesTool         = coding.ListMCPResourcesTool
	ListResourceTemplatesTool = coding.ListMCPResourceTemplatesTool
	ReadResourceTool          = coding.ReadMCPResourceTool
)

// IsMCPToolName reports whether a tool comes from MCP: a server tool
// (mcp__<server>__<tool>) or a resource tool.
var IsMCPToolName = coding.IsMCPToolName

var nonIdentifier = regexp.MustCompile(`[^A-Za-z0-9_]`)

// ToolName is mcp__<server>__<tool>, sanitized and shortened with a hash
// suffix when too long (pi createMcpToolName). Like Codex, everything but
// [A-Za-z0-9_] becomes "_", so the name is also the identifier codemode
// scripts call it by. isTaken reports names used by a different MCP tool:
// sanitizing can map two tools to one name, which then get the hash suffix.
func ToolName(server, tool string, isTaken func(string) bool) string {
	// JavaScript's replace works on UTF-16 code units, so a character outside
	// the BMP becomes two underscores.
	name := nonIdentifier.ReplaceAllStringFunc("mcp__"+server+"__"+tool, func(c string) string {
		if r, _ := utf8.DecodeRuneInString(c); r > 0xFFFF {
			return "__"
		}
		return "_"
	})
	if len(name) <= maxToolNameLength && (isTaken == nil || !isTaken(name)) {
		return name
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	hash := hex.EncodeToString(sum[:])[:8]
	// slice() stops at the end of a shorter name, which a taken name can be.
	return name[:min(len(name), maxToolNameLength-len(hash)-1)] + "_" + hash
}

// ToolDetails are an MCP tool result's details.
type ToolDetails struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// FullOutputPath is the temp file with the full text, when the model-facing
	// text was truncated.
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

// OutputSaver saves truncated text or a binary resource and returns the file
// path; extension includes the dot.
type OutputSaver func(data []byte, extension string) (string, error)

// SaveToTempFile writes an MCP output file (coding.WriteOutputFile with the
// pi-mcp prefix).
func SaveToTempFile(data []byte, extension string) (string, error) {
	return coding.WriteOutputFile("pi-mcp", extension, data)
}

// truncateMiddle keeps the start and the end of content, half of maxBytes
// each, and replaces the middle with a "…N chars truncated…" marker, like
// Codex does for tool output (pi truncateMiddle). It cuts at character
// boundaries.
func truncateMiddle(content string, maxBytes int) (out string, truncated bool, totalBytes, totalLines int) {
	totalBytes = len(content)
	totalLines = strings.Count(content, "\n") + 1
	if totalBytes <= maxBytes {
		return content, false, totalBytes, totalLines
	}
	headEnd := maxBytes / 2
	for headEnd > 0 && !utf8.RuneStart(content[headEnd]) {
		headEnd--
	}
	tailStart := totalBytes - (maxBytes - maxBytes/2)
	for tailStart < totalBytes && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	removed := utf8.RuneCountInString(content[headEnd:tailStart])
	return fmt.Sprintf("%s…%d chars truncated…%s", content[:headEnd], removed, content[tailStart:]), true, totalBytes, totalLines
}

func textOf(content ai.ContentList) string {
	var parts []string
	for _, c := range content {
		if t, ok := c.(ai.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// limitContent keeps model-facing text within OutputMaxBytes. Longer text
// becomes one text block in Codex's truncation format, followed by the path of
// the file with the full text; images follow it.
func limitContent(content ai.ContentList, save OutputSaver) (ai.ContentList, string) {
	combined := textOf(content)
	cut, truncated, totalBytes, totalLines := truncateMiddle(combined, OutputMaxBytes)
	if !truncated {
		return content, ""
	}
	if save == nil {
		save = SaveToTempFile
	}
	var where, fullPath string
	if path, err := save([]byte(combined), ".txt"); err == nil {
		fullPath = path
		where = "[Full output: " + path + " (read it with offset/limit)]"
	} else {
		where = "[Could not save the full output: " + err.Error() + "]"
	}
	tokens := int(math.Ceil(float64(totalBytes) / 4))
	text := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s\n\n%s", tokens, totalLines, cut, where)
	out := ai.ContentList{ai.TextContent{Text: text}}
	for _, c := range content {
		if img, ok := c.(ai.ImageContent); ok {
			out = append(out, img)
		}
	}
	return out, fullPath
}

func llmToContent(items []mcp.LLMContent) ai.ContentList {
	out := make(ai.ContentList, 0, len(items))
	for _, c := range items {
		if c.Type == "image" {
			out = append(out, ai.ImageContent{Data: c.Data, MimeType: c.MimeType})
		} else {
			out = append(out, ai.TextContent{Text: c.Text})
		}
	}
	return out
}

func extensionOf(uri string) string {
	path := uri
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		path = u.Path
	}
	if m := regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`).FindString(path); m != "" {
		return m
	}
	return ".bin"
}

func isTextMimeType(mimeType string) bool {
	if mimeType == "" {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	return strings.HasPrefix(t, "text/") || t == "application/json" || strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml")
}

func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}

// ConvertOptions configure ConvertResult.
type ConvertOptions struct {
	// Save stores truncated text and binary resources; nil means a temp file.
	Save OutputSaver
	// ReadableResources: the server's resources can be read with
	// read_mcp_resource, which resource links then name.
	ReadableResources bool
}

func blockToContent(server string, block mcp.ContentBlock, opts ConvertOptions) ai.ContentList {
	if block.Type == "resource_link" {
		var details []string
		if block.MimeType != "" {
			details = append(details, block.MimeType)
		}
		if block.Size != nil {
			details = append(details, formatSize(*block.Size))
		}
		title := block.Title
		if title == "" {
			title = block.Name
		}
		text := fmt.Sprintf("[Resource %s %q", block.URI, title)
		if len(details) > 0 {
			text += " (" + strings.Join(details, ", ") + ")"
		}
		if block.Description != "" {
			text += ": " + block.Description
		}
		if opts.ReadableResources {
			text += fmt.Sprintf(". Read it with %s (server %q)", ReadResourceTool, server)
		}
		return ai.ContentList{ai.TextContent{Text: text + "]"}}
	}
	if block.Type == "resource" && block.Resource != nil && block.Resource.Blob != nil && !strings.HasPrefix(block.Resource.MimeType, "image/") {
		r := block.Resource
		data, _ := base64.StdEncoding.DecodeString(*r.Blob)
		if isTextMimeType(r.MimeType) {
			return ai.ContentList{ai.TextContent{Text: string(data)}}
		}
		mime := r.MimeType
		if mime == "" {
			mime = "unknown type"
		}
		kind := mime + ", " + formatSize(int64(len(data)))
		save := opts.Save
		if save == nil {
			save = SaveToTempFile
		}
		path, err := save(data, extensionOf(r.URI))
		if err != nil {
			return ai.ContentList{ai.TextContent{Text: fmt.Sprintf("[Binary resource %s (%s) could not be saved: %v]", r.URI, kind, err)}}
		}
		return ai.ContentList{ai.TextContent{Text: fmt.Sprintf("[Binary resource %s (%s) saved to %s]", r.URI, kind, path)}}
	}
	return llmToContent(mcp.ToLLMContent(mcp.CallToolResult{Content: []mcp.ContentBlock{block}}))
}

// ConvertResult converts an MCP result for the model (pi convertMcpResult).
// An isError result becomes an error result. StructuredContent is the whole
// result without _meta, never truncated, as codemode scripts receive it.
func ConvertResult(server, tool string, result *mcp.CallToolResult, opts ConvertOptions) agent.AgentToolResult {
	var converted ai.ContentList
	if len(result.Content) > 0 {
		for _, block := range result.Content {
			converted = append(converted, blockToContent(server, block, opts)...)
		}
	} else {
		converted = llmToContent(mcp.ToLLMContent(*result))
	}
	if result.IsError && textOf(converted) == "" {
		converted = append(converted, ai.TextContent{Text: fmt.Sprintf("MCP tool %s/%s returned an error", server, tool)})
	}
	content, fullPath := limitContent(converted, opts.Save)
	structured := map[string]any{"content": result.Content}
	if len(result.StructuredContent) > 0 {
		structured["structuredContent"] = json.RawMessage(result.StructuredContent)
	}
	if result.IsError {
		structured["isError"] = true
	}
	return agent.AgentToolResult{
		Content:           content,
		Details:           ToolDetails{Server: server, Tool: tool, FullOutputPath: fullPath},
		StructuredContent: structured,
		IsError:           result.IsError,
	}
}

// ToolCaller calls one server's tools.
type ToolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any, opts *mcp.RequestOptions) (*mcp.CallToolResult, error)
}

// toParameters: tool input schemas must be objects. MCP servers may omit
// "type", and some providers reject object schemas without "properties".
func toParameters(schema json.RawMessage) (*ai.Schema, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil || fields == nil {
		fields = map[string]json.RawMessage{}
	}
	if _, ok := fields["type"]; !ok {
		fields["type"] = json.RawMessage(`"object"`)
	}
	if _, ok := fields["properties"]; !ok {
		fields["properties"] = json.RawMessage(`{}`)
	}
	data, _ := json.Marshal(fields)
	var s ai.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ToolDefinition is an MCP tool as an agent tool, with its exposure and the
// namespace of its server.
type ToolDefinition struct {
	Tool      agent.AgentTool
	Server    string
	MCPName   string
	Exposure  Exposure
	Namespace *coding.ToolNamespace
}

// ServerNamespace is the namespace of a server's tools (pi registerTools):
// mcp__<server>, found by describeNamespace() under the server's own name too,
// with the configured description and the server's instructions.
func ServerNamespace(server, description, instructions string) *coding.ToolNamespace {
	return &coding.ToolNamespace{
		Name:         Namespace(server),
		Aliases:      []string{server},
		Description:  strings.TrimSpace(description),
		Instructions: instructions,
	}
}

// NewToolDefinition adapts one MCP tool (pi createMcpToolDefinition). Its
// output schema is the CallToolResult scripts receive (CreateResultSchema).
// getCaller resolves the server's connection when the tool runs.
func NewToolDefinition(server string, tool mcp.Tool, name string, exposure Exposure, namespace *coding.ToolNamespace, timeout time.Duration,
	getCaller func(ctx context.Context) (ToolCaller, error), readableResources func() bool) (ToolDefinition, error) {
	params, err := toParameters(tool.InputSchema)
	if err != nil {
		return ToolDefinition{}, fmt.Errorf("MCP tool %s/%s: input schema: %w", server, tool.Name, err)
	}
	title := tool.Title
	if title == "" && tool.Annotations != nil {
		title = tool.Annotations.Title
	}
	description := strings.TrimSpace(tool.Description)
	if description == "" {
		description = title
	}
	if description == "" {
		description = fmt.Sprintf("MCP tool %s from server %s", tool.Name, server)
	}
	mcpName := tool.Name
	at := agent.AgentTool{
		Name:         name,
		Label:        server + "/" + tool.Name,
		Description:  description,
		Parameters:   params,
		OutputSchema: CreateResultSchema(tool.OutputSchema),
		Execute: func(ctx context.Context, _ string, args map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			caller, err := getCaller(ctx)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if args == nil {
				args = map[string]any{}
			}
			result, err := caller.CallTool(ctx, mcpName, args, &mcp.RequestOptions{
				Timeout: timeout,
				OnProgress: func(p mcp.ProgressNotification) {
					if onUpdate == nil {
						return
					}
					text := p.Message
					if text == "" {
						text = fmt.Sprintf("Progress %v", p.Progress)
						if p.Total != nil {
							text += fmt.Sprintf("/%v", *p.Total)
						}
					}
					onUpdate(agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}, Details: ToolDetails{Server: server, Tool: mcpName}})
				},
			})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			readable := readableResources != nil && readableResources()
			return ConvertResult(server, mcpName, result, ConvertOptions{ReadableResources: readable}), nil
		},
	}
	return ToolDefinition{Tool: at, Server: server, MCPName: mcpName, Exposure: exposure, Namespace: namespace}, nil
}
