package mcp

import (
	"encoding/json"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// ResourceContents is a text or binary resource body. Text and Blob are
// pointers because a present empty string differs from an absent field (pi's
// `"text" in resource`).
type ResourceContents struct {
	URI      string          `json:"uri"`
	MimeType string          `json:"mimeType,omitempty"`
	Text     *string         `json:"text,omitempty"`
	Blob     *string         `json:"blob,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

// ContentBlock is one block of a tool result: text, image, audio,
// resource_link or resource (an embedded resource). Fields that do not belong
// to Type stay empty.
type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	Resource    *ResourceContents `json:"resource,omitempty"`
	Annotations json.RawMessage   `json:"annotations,omitempty"`
	Meta        json.RawMessage   `json:"_meta,omitempty"`
}

// CallToolResult is the answer to tools/call. StructuredContent may be any JSON
// value, including arrays and null (MCP 2026-07-28); it is present when
// non-empty.
type CallToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              json.RawMessage `json:"_meta,omitempty"`
}

// LLMContent is tool result content in the shape LLM APIs accept: text, or a
// base64 image. Type is "text" or "image"; it mirrors ai.TextContent and
// ai.ImageContent without importing package ai.
type LLMContent struct {
	Type     string
	Text     string
	Data     string
	MimeType string
}

func textContent(text string) LLMContent { return LLMContent{Type: "text", Text: text} }

func blockToLLMContent(block ContentBlock) LLMContent {
	switch block.Type {
	case "text":
		return textContent(block.Text)
	case "image":
		return LLMContent{Type: "image", Data: block.Data, MimeType: block.MimeType}
	case "audio":
		return textContent("[audio " + block.MimeType + " omitted]")
	case "resource_link":
		return textContent(block.Name + ": " + block.URI)
	case "resource":
		resource := block.Resource
		if resource == nil {
			resource = &ResourceContents{}
		}
		if resource.Text != nil {
			return textContent(*resource.Text)
		}
		if strings.HasPrefix(resource.MimeType, "image/") {
			blob := ""
			if resource.Blob != nil {
				blob = *resource.Blob
			}
			return LLMContent{Type: "image", Data: blob, MimeType: resource.MimeType}
		}
		mime := resource.MimeType
		if mime == "" {
			mime = "unknown type"
		}
		return textContent("[binary resource " + resource.URI + " (" + mime + ") omitted]")
	}
	return textContent("[unsupported MCP content " + block.Type + "]")
}

// ToLLMContent converts a tool result to text and image content for a model
// (pi toLlmContent). Text and images pass through, embedded text resources
// become text, embedded image resources become images, and other blocks
// (audio, resource links, binary resources) become a short text placeholder.
// A result without content blocks but with structured content becomes its
// JSON, since servers should, but do not always, mirror structured results as
// text.
func ToLLMContent(result CallToolResult) []LLMContent {
	content := make([]LLMContent, 0, len(result.Content))
	for _, block := range result.Content {
		content = append(content, blockToLLMContent(block))
	}
	if len(content) == 0 && len(result.StructuredContent) > 0 {
		content = append(content, textContent(indentJSON(result.StructuredContent)))
	}
	return content
}

// indentJSON is JSON.stringify(JSON.parse(raw), null, 2): numbers spelled the
// JavaScript way, two-space indentation. Object keys keep their source order,
// as JSON.parse keeps insertion order.
func indentJSON(raw json.RawMessage) string {
	var b strings.Builder
	if err := writeIndented(&b, raw, ""); err != nil {
		return string(raw)
	}
	return b.String()
}

func writeIndented(b *strings.Builder, raw json.RawMessage, indent string) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return writeValue(b, dec, indent)
}

func writeValue(b *strings.Builder, dec *json.Decoder, indent string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		closing := "}"
		if v == '[' {
			closing = "]"
		}
		inner := indent + "  "
		first := true
		b.WriteString(string(v))
		for dec.More() {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString("\n" + inner)
			if v == '{' {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := marshalNoEscape(keyTok)
				b.Write(key)
				b.WriteString(": ")
			}
			if err := writeValue(b, dec, inner); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		if !first {
			b.WriteString("\n" + indent)
		}
		b.WriteString(closing)
	case json.Number:
		b.WriteString(jstext.NumberToString(jstext.Number(v)))
	default:
		text, err := marshalNoEscape(v)
		if err != nil {
			return err
		}
		b.Write(text)
	}
	return nil
}
