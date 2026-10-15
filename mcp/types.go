package mcp

import "encoding/json"

// LatestProtocolVersion is the protocol revision the client asks for.
const LatestProtocolVersion = "2025-11-25"

// SupportedProtocolVersions are the versions the client accepts from a server.
// Servers that do not support the requested version answer with their own
// latest one, so older versions stay accepted for servers built on older SDKs.
var SupportedProtocolVersions = []string{LatestProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"}

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// Root is a filesystem root the client offers the server.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// ClientCapabilities are what the client announces at initialize.
type ClientCapabilities struct {
	Experimental map[string]any   `json:"experimental,omitempty"`
	Roots        *RootsCapability `json:"roots,omitempty"`
	Sampling     map[string]any   `json:"sampling,omitempty"`
	Elicitation  map[string]any   `json:"elicitation,omitempty"`
}

// RootsCapability is the client's roots capability.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities are what the server announced at initialize. Each
// capability is kept raw: only presence and a few flags are read.
type ServerCapabilities struct {
	Experimental json.RawMessage `json:"experimental,omitempty"`
	Logging      json.RawMessage `json:"logging,omitempty"`
	Prompts      *ListChanged    `json:"prompts,omitempty"`
	Resources    *ResourcesCaps  `json:"resources,omitempty"`
	Tools        *ListChanged    `json:"tools,omitempty"`
	Completions  json.RawMessage `json:"completions,omitempty"`
}

// ListChanged is a capability whose only flag is listChanged.
type ListChanged struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCaps is the server's resources capability.
type ResourcesCaps struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// InitializeResult is the server's answer to initialize.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ProgressNotification reports progress on a request that asked for it.
type ProgressNotification struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// ToolAnnotations are a tool's behaviour hints.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ToolExecution says whether a tool may run as a task.
type ToolExecution struct {
	TaskSupport string `json:"taskSupport,omitempty"`
}

// Tool is a tool a server lists in tools/list. The schemas stay raw JSON: they
// are handed to the model as they are.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Execution    *ToolExecution   `json:"execution,omitempty"`
	Meta         json.RawMessage  `json:"_meta,omitempty"`
}

// Resource is a resource a server lists in resources/list.
type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ResourceTemplate is a family of resources addressed by an RFC 6570 URI
// template, from resources/templates/list.
type ResourceTemplate struct {
	URITemplate string          `json:"uriTemplate"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ListResourcesResult is one page of resources/list.
type ListResourcesResult struct {
	Resources  []Resource
	NextCursor string
}

// ListResourceTemplatesResult is one page of resources/templates/list.
type ListResourceTemplatesResult struct {
	ResourceTemplates []ResourceTemplate
	NextCursor        string
}

// ReadResourceResult is the answer to resources/read.
type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
	Meta     json.RawMessage    `json:"_meta,omitempty"`
}
