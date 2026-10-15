// Package mcp is a Model Context Protocol client: JSON-RPC messages, the
// stdio, Streamable HTTP and in-memory transports, and a client that speaks
// the protocol over any of them. It ports pi's @earendil-works/pi-mcp
// (packages/mcp at v1.2.0) and, like it, depends on nothing else in the
// module: tool results convert to LLMContent, which mirrors the text and image
// content of package ai without importing it.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// JSON-RPC error codes (pi JSON_RPC_ERROR_CODES).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ErrorObject is a JSON-RPC error.
type ErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is one JSON-RPC 2.0 message: a request (ID and Method), a
// notification (Method, no ID), or a response (ID and Result or Error).
//
// ID, Params and Result hold raw JSON. A present field is non-empty — a JSON
// null arrives as the text "null" — so presence is len(field) > 0, which is
// how pi's `"id" in message` and `"result" in message` checks are ported.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// IsRequest reports pi's isJsonRpcRequest: a valid id and a method.
func (m Message) IsRequest() bool { return m.JSONRPC == "2.0" && validID(m.ID) && m.Method != "" }

// IsNotification reports pi's isJsonRpcNotification: a method and no id.
func (m Message) IsNotification() bool { return m.JSONRPC == "2.0" && len(m.ID) == 0 && m.Method != "" }

// IsResponse reports pi's isJsonRpcResponse: a valid id and either a result or
// a well-formed error, not both.
func (m Message) IsResponse() bool {
	if m.JSONRPC != "2.0" || !validID(m.ID) {
		return false
	}
	if len(m.Result) > 0 {
		return m.Error == nil
	}
	return m.Error != nil
}

// IDKey is a comparable key for an id: equal ids give equal keys, so 1 and 1.0
// match as they do as JavaScript Map keys, and the string "1" stays distinct
// from the number 1.
func IDKey(id json.RawMessage) string {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return "s:" + s
		}
	}
	if f, err := strconv.ParseFloat(string(trimmed), 64); err == nil {
		return "n:" + jstext.NumberToString(f)
	}
	return "?:" + string(trimmed)
}

// NumberID is a numeric JSON-RPC id.
func NumberID(n int64) json.RawMessage { return json.RawMessage(strconv.FormatInt(n, 10)) }

// validID is pi's isJsonRpcId: a string or a finite number.
func validID(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '"' {
		var s string
		return json.Unmarshal(trimmed, &s) == nil
	}
	var n json.Number
	if json.Unmarshal(trimmed, &n) != nil {
		return false
	}
	_, err := strconv.ParseFloat(string(n), 64)
	return err == nil
}

// rawMessage is the decoding shape used to check field types before trusting
// them, as pi's guards check typeof before reading.
type rawMessage map[string]json.RawMessage

// ParseMessage decodes and validates one JSON-RPC message (pi
// parseJsonRpcMessage over JSON.parse). Anything that is neither a request, a
// notification nor a response is an invalid-request *Error.
func ParseMessage(data []byte) (Message, error) {
	var fields rawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return Message{}, err
		}
		return Message{}, invalidMessage()
	}
	return messageFromFields(fields)
}

// ParseMessageValue validates an already decoded JSON value, for a body that
// carried a batch (an array of messages).
func ParseMessageValue(raw json.RawMessage) (Message, error) {
	var fields rawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Message{}, invalidMessage()
	}
	return messageFromFields(fields)
}

func invalidMessage() error {
	return &Error{Code: CodeInvalidRequest, Message: "Invalid JSON-RPC message"}
}

func messageFromFields(fields rawMessage) (Message, error) {
	var m Message
	if err := json.Unmarshal(fields["jsonrpc"], &m.JSONRPC); err != nil || m.JSONRPC != "2.0" {
		return Message{}, invalidMessage()
	}
	m.ID = fields["id"]
	if raw, ok := fields["method"]; ok {
		if json.Unmarshal(raw, &m.Method) != nil || m.Method == "" && string(bytes.TrimSpace(raw)) != `""` {
			m.Method = ""
		}
	}
	m.Params = fields["params"]
	m.Result = fields["result"]
	if raw, ok := fields["error"]; ok {
		var errFields rawMessage
		if json.Unmarshal(raw, &errFields) == nil && errFields != nil {
			var obj ErrorObject
			var code float64
			if json.Unmarshal(errFields["code"], &code) == nil && json.Unmarshal(errFields["message"], &obj.Message) == nil {
				obj.Code = int(code)
				obj.Data = errFields["data"]
				m.Error = &obj
			}
		}
		if m.Error == nil && len(m.Result) == 0 {
			return Message{}, invalidMessage()
		}
	}
	if m.IsRequest() || m.IsNotification() || m.IsResponse() {
		return m, nil
	}
	return Message{}, invalidMessage()
}

// Error is an error a JSON-RPC peer returned, or an invalid message (pi
// McpError).
type Error struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *Error) Error() string { return e.Message }

// ConnectionClosedError reports a connection that is closed or was never open
// (pi McpConnectionClosedError).
type ConnectionClosedError struct{ Msg string }

func (e *ConnectionClosedError) Error() string {
	if e.Msg == "" {
		return "MCP connection closed"
	}
	return e.Msg
}

// TimeoutError reports a request that got no answer in time (pi
// McpTimeoutError).
type TimeoutError struct{ TimeoutMs int64 }

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("MCP request timed out after %dms", e.TimeoutMs)
}

// AbortError reports a request its caller gave up on (pi McpAbortError).
type AbortError struct{ Msg string }

func (e *AbortError) Error() string {
	if e.Msg == "" {
		return "MCP request aborted"
	}
	return e.Msg
}

// isObject reports whether raw is a JSON object (pi isObject).
func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// jsonType names a raw JSON value's type the way typeof reads it after
// JSON.parse, with "null" and "array" told apart.
func jsonType(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "undefined"
	}
	switch c := trimmed[0]; {
	case c == '{':
		return "object"
	case c == '[':
		return "array"
	case c == '"':
		return "string"
	case c == 't' || c == 'f':
		return "boolean"
	case c == 'n':
		return "null"
	case c == '-' || c >= '0' && c <= '9':
		return "number"
	}
	return "invalid"
}

// marshalNoEscape writes v as JSON without HTML escaping, as JSON.stringify
// writes it.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// trimCR drops one trailing "\r", as pi's /\r$/ replace does.
func trimCR(s string) string { return strings.TrimSuffix(s, "\r") }
