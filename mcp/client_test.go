package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer answers requests on the server end of an in-memory pair and
// records every message it receives (pi client.test.ts createServer).
type testServer struct {
	transport *InMemoryTransport
	mu        sync.Mutex
	messages  []Message
	handlers  map[string]func(Message) (any, error)
}

func (s *testServer) setHandler(method string, h func(Message) (any, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

func (s *testServer) received() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

func newTestServer(t *testing.T) (*InMemoryTransport, *testServer) {
	t.Helper()
	clientT, serverT := NewInMemoryTransportPair()
	s := &testServer{transport: serverT, handlers: map[string]func(Message) (any, error){}}
	serverT.OnMessage(func(msg Message) {
		s.mu.Lock()
		s.messages = append(s.messages, msg)
		handler := s.handlers[msg.Method]
		s.mu.Unlock()
		if !msg.IsRequest() {
			return
		}
		go func() {
			var resp Message
			if handler == nil {
				resp = Message{JSONRPC: "2.0", ID: msg.ID, Error: &ErrorObject{Code: CodeMethodNotFound, Message: "Method not found: " + msg.Method}}
			} else if value, err := handler(msg); err != nil {
				var mcpErr *Error
				if !errors.As(err, &mcpErr) {
					mcpErr = &Error{Code: CodeInternalError, Message: err.Error()}
				}
				resp = Message{JSONRPC: "2.0", ID: msg.ID, Error: &ErrorObject{Code: mcpErr.Code, Message: mcpErr.Message, Data: mcpErr.Data}}
			} else {
				data, _ := json.Marshal(value)
				resp = Message{JSONRPC: "2.0", ID: msg.ID, Result: data}
			}
			_ = serverT.Send(context.Background(), resp)
		}()
	})
	if err := serverT.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.setHandler("initialize", func(Message) (any, error) {
		return map[string]any{
			"protocolVersion": LatestProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "test-server", "version": "1.0.0"},
			"instructions":    "Use test tools.",
		}, nil
	})
	return clientT, s
}

func connectTest(t *testing.T) (*Client, *testServer) {
	t.Helper()
	clientT, s := newTestServer(t)
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "test-client", Version: "2.0.0"}})
	if _, err := c.Connect(context.Background(), clientT); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, s
}

// never blocks a handler until the test ends.
func never(t *testing.T) func(Message) (any, error) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	return func(Message) (any, error) { <-done; return nil, errors.New("test over") }
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientInitializesBeforeExposingServerInfo(t *testing.T) {
	c, s := connectTest(t)
	if c.State() != StateConnected || c.ProtocolVersion() != LatestProtocolVersion {
		t.Fatalf("state %s, version %s", c.State(), c.ProtocolVersion())
	}
	if info := c.ServerInfo(); info == nil || info.Name != "test-server" || info.Version != "1.0.0" {
		t.Fatalf("serverInfo = %+v", info)
	}
	if caps := c.ServerCapabilities(); caps == nil || caps.Tools == nil || !caps.Tools.ListChanged {
		t.Fatalf("capabilities = %+v", caps)
	}
	if c.Instructions() != "Use test tools." {
		t.Fatalf("instructions = %q", c.Instructions())
	}
	waitFor(t, "initialized notification", func() bool { return len(s.received()) == 2 })
	msgs := s.received()
	if msgs[0].Method != "initialize" || string(msgs[0].ID) != "1" {
		t.Fatalf("first message = %+v", msgs[0])
	}
	jsonEqual(t, msgs[0].Params, `{"protocolVersion":"`+LatestProtocolVersion+`","capabilities":{},"clientInfo":{"name":"test-client","version":"2.0.0"}}`)
	if msgs[1].Method != "notifications/initialized" || len(msgs[1].ID) != 0 || len(msgs[1].Params) != 0 {
		t.Fatalf("second message = %+v", msgs[1])
	}
}

func TestClientPaginatesTools(t *testing.T) {
	c, s := connectTest(t)
	s.setHandler("tools/list", func(m Message) (any, error) {
		var p struct{ Cursor *string }
		_ = json.Unmarshal(m.Params, &p)
		if p.Cursor == nil {
			return map[string]any{
				"tools":      []any{map[string]any{"name": "search", "description": "Search", "inputSchema": map[string]any{"type": "object"}}},
				"nextCursor": "page-2",
			}, nil
		}
		// Some servers end pagination with an empty cursor instead of omitting it.
		return map[string]any{
			"tools": []any{map[string]any{
				"name": "read", "inputSchema": map[string]any{"type": "object"},
				"outputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true},
			}},
			"nextCursor": "",
		}, nil
	})
	tools, err := c.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "search" || tools[0].Description != "Search" || tools[1].Name != "read" {
		t.Fatalf("tools = %+v", tools)
	}
	jsonEqual(t, tools[1].OutputSchema, `{"type":"object"}`)
	if tools[1].Annotations == nil || tools[1].Annotations.ReadOnlyHint == nil || !*tools[1].Annotations.ReadOnlyHint {
		t.Fatalf("annotations = %+v", tools[1].Annotations)
	}
}

func TestClientListsAndReadsResources(t *testing.T) {
	c, s := connectTest(t)
	ctx := context.Background()
	s.setHandler("resources/list", func(m Message) (any, error) {
		var p struct{ Cursor *string }
		_ = json.Unmarshal(m.Params, &p)
		if p.Cursor == nil {
			return map[string]any{"resources": []any{map[string]any{"uri": "file:///a", "name": "a", "mimeType": "text/plain"}}, "nextCursor": "2"}, nil
		}
		return map[string]any{"resources": []any{map[string]any{"uri": "file:///b"}}}, nil
	})
	s.setHandler("resources/templates/list", func(Message) (any, error) {
		return map[string]any{"resourceTemplates": []any{map[string]any{"uriTemplate": "repo://{owner}/{repo}", "name": "repo"}}}, nil
	})
	s.setHandler("resources/read", func(m Message) (any, error) {
		var p struct{ URI string }
		_ = json.Unmarshal(m.Params, &p)
		return map[string]any{"contents": []any{map[string]any{"uri": p.URI, "text": "hello"}}}, nil
	})

	resources, err := c.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A missing name falls back to the URI.
	if len(resources) != 2 || resources[0].Name != "a" || resources[1].Name != "file:///b" {
		t.Fatalf("resources = %+v", resources)
	}
	templates, err := c.ListResourceTemplates(ctx, nil)
	if err != nil || len(templates) != 1 || templates[0].Name != "repo" {
		t.Fatalf("templates = %+v, %v", templates, err)
	}
	page, err := c.ListResourcesPage(ctx, "", nil)
	if err != nil || len(page.Resources) != 1 || page.NextCursor != "2" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = c.ListResourcesPage(ctx, "2", nil)
	if err != nil || len(page.Resources) != 1 || page.Resources[0].Name != "file:///b" || page.NextCursor != "" {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	read, err := c.ReadResource(ctx, "file:///a", nil)
	if err != nil || len(read.Contents) != 1 || read.Contents[0].Text == nil || *read.Contents[0].Text != "hello" {
		t.Fatalf("read = %+v, %v", read, err)
	}

	s.setHandler("resources/read", func(Message) (any, error) {
		return map[string]any{"contents": []any{map[string]any{"uri": "file:///a"}}}, nil
	})
	if _, err := c.ReadResource(ctx, "file:///a", nil); err == nil || !strings.Contains(err.Error(), "Invalid contents in MCP resources/read result") {
		t.Fatalf("read without text or blob: %v", err)
	}
	s.setHandler("resources/list", func(Message) (any, error) {
		return map[string]any{"resources": []any{map[string]any{"name": "no uri"}}}, nil
	})
	if _, err := c.ListResources(ctx, nil); err == nil || !strings.Contains(err.Error(), "Invalid entry in MCP resources/list result") {
		t.Fatalf("resource without uri: %v", err)
	}
}

func TestClientCallToolStructuredAndErrors(t *testing.T) {
	c, s := connectTest(t)
	s.setHandler("tools/call", func(m Message) (any, error) {
		var p struct {
			Name      string
			Arguments map[string]any
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Name == "fail" {
			return nil, &Error{Code: 1234, Message: "tool failed", Data: json.RawMessage(`{"retryable":false}`)}
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "structuredContent": map[string]any{"count": p.Arguments["count"]}}, nil
	})
	res, err := c.CallTool(context.Background(), "count", map[string]any{"count": 3}, nil)
	if err != nil || len(res.Content) != 1 || res.Content[0].Text != "ok" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	jsonEqual(t, res.StructuredContent, `{"count":3}`)

	_, err = c.CallTool(context.Background(), "fail", nil, nil)
	var mcpErr *Error
	if !errors.As(err, &mcpErr) || mcpErr.Code != 1234 || mcpErr.Message != "tool failed" {
		t.Fatalf("fail err = %v", err)
	}
	jsonEqual(t, mcpErr.Data, `{"retryable":false}`)
}

func TestClientRenewsTimeoutOnProgress(t *testing.T) {
	c, s := connectTest(t)
	s.setHandler("tools/call", func(m Message) (any, error) {
		var p struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		time.Sleep(120 * time.Millisecond)
		params, _ := json.Marshal(map[string]any{"progressToken": p.Meta.ProgressToken, "progress": 1, "total": 2})
		_ = s.transport.Send(context.Background(), Message{JSONRPC: "2.0", Method: "notifications/progress", Params: params})
		time.Sleep(120 * time.Millisecond)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}, nil
	})
	var mu sync.Mutex
	var got []ProgressNotification
	res, err := c.CallTool(context.Background(), "slow", map[string]any{}, &RequestOptions{
		Timeout:    200 * time.Millisecond, // shorter than the call, longer than each gap
		OnProgress: func(p ProgressNotification) { mu.Lock(); got = append(got, p); mu.Unlock() },
	})
	if err != nil || res.Content[0].Text != "done" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || string(got[0].ProgressToken) != "2" || got[0].Progress != 1 || got[0].Total == nil || *got[0].Total != 2 {
		t.Fatalf("progress = %+v", got)
	}
}

func TestClientCancelsAbortedAndTimedOutRequests(t *testing.T) {
	c, s := connectTest(t)
	s.setHandler("tools/call", never(t))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.CallTool(ctx, "wait", map[string]any{}, nil); done <- err }()
	waitFor(t, "the call to reach the server", func() bool { return len(s.received()) >= 3 })
	cancel(errors.New("stop"))
	var abortErr *AbortError
	if err := <-done; !errors.As(err, &abortErr) {
		t.Fatalf("aborted err = %v", err)
	}
	waitFor(t, "notifications/cancelled", func() bool {
		for _, m := range s.received() {
			if m.Method == "notifications/cancelled" {
				jsonEqual(t, m.Params, `{"requestId":2,"reason":"stop"}`)
				return true
			}
		}
		return false
	})

	_, err := c.CallTool(context.Background(), "wait", map[string]any{}, &RequestOptions{Timeout: 5 * time.Millisecond})
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("timed-out err = %v", err)
	}
}

func TestClientReportsTransportErrorsWithoutFailingRequests(t *testing.T) {
	clientT, s := newTestServer(t)
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "test-client", Version: "1.0.0"}})
	if _, err := c.Connect(context.Background(), clientT); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mu sync.Mutex
	var errs []string
	c.OnError(func(err error) { mu.Lock(); errs = append(errs, err.Error()); mu.Unlock() })
	release := make(chan struct{})
	s.setHandler("tools/call", func(Message) (any, error) { <-release; return map[string]any{"content": []any{}}, nil })
	done := make(chan error, 1)
	go func() { _, err := c.CallTool(context.Background(), "wait", nil, nil); done <- err }()
	waitFor(t, "the call to reach the server", func() bool { return len(s.received()) >= 3 })
	clientT.EmitError(errors.New("stray log line"))
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("call failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(errs, []string{"stray log line"}) {
		t.Fatalf("errors = %v", errs)
	}
}

func TestClientAcceptsOlderProtocolVersion(t *testing.T) {
	clientT, s := newTestServer(t)
	s.setHandler("initialize", func(Message) (any, error) {
		return map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "old-server", "version": "0.1.0"}}, nil
	})
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "test-client", Version: "1.0.0"}})
	if _, err := c.Connect(context.Background(), clientT); err != nil {
		t.Fatal(err)
	}
	if c.ProtocolVersion() != "2024-11-05" {
		t.Fatalf("version = %s", c.ProtocolVersion())
	}
	_ = c.Close()

	clientT2, s2 := newTestServer(t)
	s2.setHandler("initialize", func(Message) (any, error) {
		return map[string]any{"protocolVersion": "1999-01-01", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "ancient-server", "version": "0.1.0"}}, nil
	})
	rejected := NewClient(ClientOptions{Implementation: Implementation{Name: "test-client", Version: "1.0.0"}})
	if _, err := rejected.Connect(context.Background(), clientT2); err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("connect err = %v", err)
	}
	if rejected.State() != StateClosed {
		t.Fatalf("state = %s, want closed", rejected.State())
	}
}

func TestClientDefaultsMissingToolContent(t *testing.T) {
	c, s := connectTest(t)
	s.setHandler("tools/call", func(Message) (any, error) {
		return map[string]any{"structuredContent": map[string]any{"ok": true}}, nil
	})
	res, err := c.CallTool(context.Background(), "structured", nil, nil)
	if err != nil || res.Content == nil || len(res.Content) != 0 {
		t.Fatalf("call = %+v, %v", res, err)
	}
	jsonEqual(t, res.StructuredContent, `{"ok":true}`)
	s.setHandler("tools/call", func(Message) (any, error) { return map[string]any{"content": "not a list"}, nil })
	if _, err := c.CallTool(context.Background(), "broken", nil, nil); err == nil || !strings.Contains(err.Error(), "Invalid MCP tools/call result") {
		t.Fatalf("broken err = %v", err)
	}
}

// #10416: MCP 2026-07-28 allows any JSON value as structured content.
func TestClientAcceptsNonObjectStructuredContent(t *testing.T) {
	c, s := connectTest(t)
	for _, sc := range []string{`[{"id":1},{"id":2}]`, `null`, `"text"`, `3`, `false`} {
		s.setHandler("tools/call", func(Message) (any, error) {
			return map[string]json.RawMessage{"content": json.RawMessage(`[]`), "structuredContent": json.RawMessage(sc)}, nil
		})
		res, err := c.CallTool(context.Background(), "structured", nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", sc, err)
		}
		jsonEqual(t, res.StructuredContent, sc)
	}
}

func TestClientDoesNotCancelTimedOutInitialize(t *testing.T) {
	clientT, s := newTestServer(t)
	s.setHandler("initialize", never(t))
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "test-client", Version: "1.0.0"}, RequestTimeout: 5 * time.Millisecond})
	_, err := c.Connect(context.Background(), clientT)
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("connect err = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	for _, m := range s.received() {
		if m.Method == "notifications/cancelled" {
			t.Fatal("initialize must not be cancelled")
		}
	}
}

func TestClientNotifiesCloseListenersOnce(t *testing.T) {
	c, s := connectTest(t)
	var closed int
	var mu sync.Mutex
	c.OnClose(func() { mu.Lock(); closed++; mu.Unlock() })
	s.setHandler("tools/call", never(t))
	done := make(chan error, 1)
	go func() { _, err := c.CallTool(context.Background(), "wait", nil, nil); done <- err }()
	waitFor(t, "the call to reach the server", func() bool { return len(s.received()) >= 3 })
	_ = s.transport.Close()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "MCP connection closed") {
		t.Fatalf("pending err = %v", err)
	}
	if c.State() != StateClosed {
		t.Fatalf("state = %s", c.State())
	}
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	if closed != 1 {
		t.Fatalf("close listeners called %d times, want 1", closed)
	}
}

func TestClientAnswersRootsListAndDispatchesNotifications(t *testing.T) {
	clientT, s := newTestServer(t)
	c := NewClient(ClientOptions{
		Implementation: Implementation{Name: "test-client", Version: "1.0.0"},
		Roots: func(context.Context) ([]Root, error) {
			return []Root{{URI: "file:///workspace", Name: "workspace"}}, nil
		},
	})
	if _, err := c.Connect(context.Background(), clientT); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	changed := make(chan json.RawMessage, 1)
	c.OnNotification("notifications/tools/list_changed", func(p json.RawMessage) { changed <- p })
	ctx := context.Background()
	_ = s.transport.Send(ctx, Message{JSONRPC: "2.0", ID: json.RawMessage(`"roots"`), Method: "roots/list"})
	_ = s.transport.Send(ctx, Message{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
	waitFor(t, "roots/list answer", func() bool {
		for _, m := range s.received() {
			if string(m.ID) == `"roots"` && len(m.Result) > 0 {
				jsonEqual(t, m.Result, `{"roots":[{"uri":"file:///workspace","name":"workspace"}]}`)
				return true
			}
		}
		return false
	})
	select {
	case p := <-changed:
		if len(p) != 0 {
			t.Fatalf("notification params = %s, want none", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not dispatched")
	}
}

func TestToLLMContent(t *testing.T) {
	text := "embedded"
	blob := "iVBOR"
	got := ToLLMContent(CallToolResult{Content: []ContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "image", Data: "AAA", MimeType: "image/png"},
		{Type: "audio", Data: "x", MimeType: "audio/wav"},
		{Type: "resource_link", Name: "doc", URI: "file:///doc"},
		{Type: "resource", Resource: &ResourceContents{URI: "file:///t", Text: &text}},
		{Type: "resource", Resource: &ResourceContents{URI: "file:///i", MimeType: "image/png", Blob: &blob}},
		{Type: "resource", Resource: &ResourceContents{URI: "file:///b", MimeType: "application/zip", Blob: &blob}},
		{Type: "video"},
	}})
	want := []LLMContent{
		{Type: "text", Text: "hi"},
		{Type: "image", Data: "AAA", MimeType: "image/png"},
		{Type: "text", Text: "[audio audio/wav omitted]"},
		{Type: "text", Text: "doc: file:///doc"},
		{Type: "text", Text: "embedded"},
		{Type: "image", Data: "iVBOR", MimeType: "image/png"},
		{Type: "text", Text: "[binary resource file:///b (application/zip) omitted]"},
		{Type: "text", Text: "[unsupported MCP content video]"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToLLMContent =\n%+v\nwant\n%+v", got, want)
	}
	structured := ToLLMContent(CallToolResult{StructuredContent: json.RawMessage(`{"b":1.50,"a":[1,"x"]}`)})
	if len(structured) != 1 || structured[0].Text != "{\n  \"b\": 1.5,\n  \"a\": [\n    1,\n    \"x\"\n  ]\n}" {
		t.Fatalf("structured = %q", structured)
	}
}
