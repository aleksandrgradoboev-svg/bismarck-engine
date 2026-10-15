package mcpext

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// contentBlockSchema is MCP content blocks as one compact type with the
// fields of text and image blocks, which scripts read most (image(block) takes
// an image block as is). Rendered into every MCP tool's declaration.
const contentBlockSchema = `{"type":"object","properties":{"type":{"enum":["text","image","audio","resource_link","resource"]},"text":{"type":"string"},"data":{"type":"string"},"mimeType":{"type":"string"}},"required":["type"],"additionalProperties":true}`

// schemaDataKeywords are keywords whose values are data, so a $ref key in them
// is not a reference.
var schemaDataKeywords = map[string]bool{"const": true, "enum": true, "default": true, "examples": true}

// schemaMapKeywords are keywords whose values map names to schemas, so their
// keys are names, not keywords.
var schemaMapKeywords = map[string]bool{
	"properties": true, "patternProperties": true, "dependentSchemas": true,
	"dependencies": true, "$defs": true, "definitions": true,
}

// relocateSchema writes schema as a subschema at the JSON pointer pointer of
// another schema: its local references (#, #/$defs/Item) are rewritten to
// point below pointer, so they resolve to the same subschemas as before (pi
// relocateSchema). Keys keep their order.
func relocateSchema(dec *json.Decoder, out *bytes.Buffer, pointer string, mode int) error {
	const (
		asSchema = iota
		asData
		asMap
	)
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out.WriteByte('{')
			first := true
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key := keyTok.(string)
				if !first {
					out.WriteByte(',')
				}
				first = false
				writeJSONString(out, key)
				out.WriteByte(':')
				child := asSchema
				switch {
				case mode == asData:
					child = asData
				case mode == asMap:
					child = asSchema
				case key == "$ref":
					// Rewritten below when it is a local reference string.
					var raw json.RawMessage
					if err := dec.Decode(&raw); err != nil {
						return err
					}
					var ref string
					if json.Unmarshal(raw, &ref) == nil && (ref == "#" || strings.HasPrefix(ref, "#/")) {
						writeJSONString(out, pointer+ref[1:])
					} else {
						out.Write(raw)
					}
					continue
				case schemaDataKeywords[key]:
					child = asData
				case schemaMapKeywords[key]:
					child = asMap
				}
				if err := relocateSchema(dec, out, pointer, child); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				// An array keeps its mode: items of data stay data.
				itemMode := mode
				if mode == asMap {
					itemMode = asSchema
				}
				if err := relocateSchema(dec, out, pointer, itemMode); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			out.WriteByte(']')
		}
	case string:
		writeJSONString(out, t)
	case json.Number:
		out.WriteString(t.String())
	case bool:
		if t {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	}
	return nil
}

func writeJSONString(out *bytes.Buffer, s string) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out.Truncate(out.Len() - 1) // Encode's newline
}

// CreateResultSchema is the output schema of every MCP tool (pi
// createMcpResultSchema): the CallToolResult scripts receive — without _meta,
// which ConvertResult drops — with the tool's own output schema as
// structuredContent, its local references relocated there. Servers may send
// structured content without declaring a schema for it, and it may be any
// JSON value, so without one it is {}.
func CreateResultSchema(structuredContentSchema json.RawMessage) *ai.Schema {
	structured := "{}"
	if len(bytes.TrimSpace(structuredContentSchema)) > 0 && !bytes.Equal(bytes.TrimSpace(structuredContentSchema), []byte("null")) {
		var buf bytes.Buffer
		dec := json.NewDecoder(bytes.NewReader(structuredContentSchema))
		dec.UseNumber()
		if err := relocateSchema(dec, &buf, "#/properties/structuredContent", 0); err == nil {
			if _, err := dec.Token(); err == io.EOF {
				structured = buf.String()
			}
		}
	}
	text := `{"type":"object","properties":{"content":{"type":"array","items":` + contentBlockSchema +
		`},"structuredContent":` + structured + `,"isError":{"type":"boolean"}},"required":["content"],"additionalProperties":true}`
	var schema ai.Schema
	if err := json.Unmarshal([]byte(text), &schema); err != nil {
		return mustSchema(`{"type":"object","properties":{"content":{"type":"array","items":` + contentBlockSchema + `},"structuredContent":{},"isError":{"type":"boolean"}},"required":["content"],"additionalProperties":true}`)
	}
	return &schema
}
