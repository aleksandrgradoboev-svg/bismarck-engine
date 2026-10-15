package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	method  string
	headers http.Header
	message map[string]any
}

type httpFixture struct {
	url      string
	mu       sync.Mutex
	requests []recordedRequest
}

func (f *httpFixture) record(r recordedRequest) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
}

func (f *httpFixture) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// startHTTPServer runs handler behind a loopback server (pi helpers.ts listen).
func startHTTPServer(t *testing.T, handler func(f *httpFixture, w http.ResponseWriter, r *http.Request)) *httpFixture {
	t.Helper()
	f := &httpFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(f, w, r) }))
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/mcp"
	return f
}

func readMessage(r *http.Request) map[string]any {
	data, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	data, _ := json.Marshal(v)
	w.Write(data)
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

// protocolHandler is pi's: 405 for GET, 200 for DELETE, 202 for
// notifications, JSON for initialize and tools/list, SSE for everything else.
func protocolHandler(f *httpFixture, w http.ResponseWriter, r *http.Request, msg map[string]any) {
	switch r.Method {
	case http.MethodGet:
		f.record(recordedRequest{method: "GET", headers: r.Header.Clone()})
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		f.record(recordedRequest{method: "DELETE", headers: r.Header.Clone()})
		return
	}
	if msg == nil {
		msg = readMessage(r)
	}
	f.record(recordedRequest{method: r.Method, headers: r.Header.Clone(), message: msg})
	id, hasID := msg["id"]
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg["method"] {
	case "initialize":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-1")
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"protocolVersion": LatestProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "http-fixture", "version": "1.0.0"},
		}})
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}},
		}})
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": toolText("hello")})
		fmt.Fprintf(w, "id: tool-result\ndata: %s\n\n", data)
	}
}

func newHTTPClient(t *testing.T, opts StreamableHTTPTransportOptions) (*Client, *StreamableHTTPTransport, error) {
	t.Helper()
	transport, err := NewStreamableHTTPTransport(opts)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "http-test", Version: "1.0.0"}})
	_, err = c.Connect(context.Background(), transport)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, transport, err
}

func TestConsumeSSEParsesChunkedCRLFEvents(t *testing.T) {
	var events []SSEEvent
	err := ConsumeSSE(strings.NewReader(": keepalive\r\nid: 7\r\ndata: {\"one\":\r\ndata: 1}\r\n\r\n"), SSEOptions{
		OnEvent: func(e SSEEvent) { events = append(events, e) },
	})
	if err != nil || !reflect.DeepEqual(events, []SSEEvent{{ID: "7", Data: "{\"one\":\n1}"}}) {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// endlessData never sends a blank line, so its event is never dispatched.
type endlessData struct{ sent int }

func (e *endlessData) Read(p []byte) (int, error) {
	if e.sent > 1000 {
		return 0, io.EOF
	}
	e.sent++
	return copy(p, "data: xxxxxxxxxxxxxxxx\n"), nil
}

func TestConsumeSSERejectsOversizedEvents(t *testing.T) {
	src := &endlessData{}
	err := ConsumeSSE(src, SSEOptions{MaxEventBytes: 256, OnEvent: func(SSEEvent) {}})
	if err == nil || err.Error() != "MCP SSE event exceeds 256 bytes" {
		t.Fatalf("err = %v", err)
	}
	if src.sent >= 100 {
		t.Fatalf("read %d chunks before failing", src.sent)
	}
}

func TestStreamableHTTPHandlesJSONAndSSEWithHeaders(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) { protocolHandler(f, w, r, nil) })
	c, transport, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url})
	if err != nil {
		t.Fatal(err)
	}
	if transport.SessionID() != "session-1" {
		t.Fatalf("session = %q", transport.SessionID())
	}
	tools, err := c.ListTools(context.Background(), nil)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "hello"}, nil)
	if err != nil || res.Content[0].Text != "hello" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	waitFor(t, "the GET stream", func() bool {
		for _, r := range f.all() {
			if r.method == "GET" {
				return true
			}
		}
		return false
	})
	_ = c.Close()
	var sawList, sawDelete bool
	for _, r := range f.all() {
		if r.message != nil && r.message["method"] == "tools/list" {
			sawList = true
			if r.headers.Get("Mcp-Session-Id") != "session-1" || r.headers.Get("Mcp-Protocol-Version") != LatestProtocolVersion {
				t.Fatalf("tools/list headers = %v", r.headers)
			}
		}
		if r.method == "DELETE" {
			sawDelete = true
		}
	}
	if !sawList || !sawDelete {
		t.Fatalf("tools/list seen %v, DELETE seen %v", sawList, sawDelete)
	}
}

func TestStreamableHTTPClassifiesAuthenticationFailures(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		_ = readMessage(r)
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.com/meta"`)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "login required")
	})
	_, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url})
	var authErr *AuthRequiredError
	if !errors.As(err, &authErr) || authErr.Status != 401 || authErr.Body != "login required" ||
		authErr.WWWAuthenticate != `Bearer resource_metadata="https://example.com/meta"` {
		t.Fatalf("err = %#v", err)
	}
}

func TestStreamableHTTPFailsOnlyTheBrokenStream(t *testing.T) {
	release := make(chan struct{})
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		params, _ := msg["params"].(map[string]any)
		switch params["name"] {
		case "broken":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: not json\n\n")
			return
		case "slow":
			<-release
		}
		protocolHandler(f, w, r, msg)
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var errs []error
	c.OnError(func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	slow := make(chan error, 1)
	go func() { _, err := c.CallTool(context.Background(), "slow", nil, nil); slow <- err }()
	if _, err := c.CallTool(context.Background(), "broken", nil, nil); err == nil || !strings.Contains(err.Error(), "MCP response stream failed") {
		t.Fatalf("broken err = %v", err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("slow err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want the one parse error", errs)
	}
}

func TestStreamableHTTPOpensGetStreamAfterInitialized(t *testing.T) {
	var mu sync.Mutex
	var order []string
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			msg := readMessage(r)
			mu.Lock()
			order = append(order, fmt.Sprint(msg["method"]))
			mu.Unlock()
			protocolHandler(f, w, r, msg)
			return
		}
		mu.Lock()
		order = append(order, r.Method)
		mu.Unlock()
		protocolHandler(f, w, r, nil)
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool(context.Background(), "echo", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the GET stream", func() bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(strings.Join(order, ","), "GET") })
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	initIdx, getIdx := -1, -1
	for i, o := range order {
		if o == "notifications/initialized" && initIdx < 0 {
			initIdx = i
		}
		if o == "GET" && getIdx < 0 {
			getIdx = i
		}
	}
	if getIdx < initIdx {
		t.Fatalf("GET opened before notifications/initialized: %v", order)
	}
	for _, r := range f.all() {
		if r.headers.Get("Last-Event-ID") != "" {
			t.Fatalf("Last-Event-ID sent without resuming: %v", r.headers)
		}
	}
}

func TestStreamableHTTPResumesResponseStream(t *testing.T) {
	var mu sync.Mutex
	var resumed []string
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("Last-Event-ID") != "" {
			mu.Lock()
			resumed = append(resumed, r.Header.Get("Last-Event-ID"))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": toolText("resumed")})
			fmt.Fprintf(w, "id: 2\ndata: %s\n\n", data)
			return
		}
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		if msg["method"] != "tools/call" {
			protocolHandler(f, w, r, msg)
			return
		}
		// Priming event (id, no data) and a retry hint, then the server drops the stream.
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 1\nretry: 5\ndata:\n\n")
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true})
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	c.OnError(func(err error) { errs = append(errs, err) })
	res, err := c.CallTool(context.Background(), "echo", nil, nil)
	if err != nil || res.Content[0].Text != "resumed" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(resumed, []string{"1"}) || len(errs) != 0 {
		t.Fatalf("resumed %v, errors %v", resumed, errs)
	}
}

func TestStreamableHTTPFailsStreamEndingWithoutAnswer(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		if msg["method"] != "tools/call" {
			protocolHandler(f, w, r, msg)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": nothing here\n\n")
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CallTool(context.Background(), "echo", nil, &RequestOptions{Timeout: 5 * time.Second})
	if err == nil || err.Error() != "MCP response stream failed: stream ended without a response" {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamableHTTPReconnectsGetStream(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	var lastIDs []string
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			protocolHandler(f, w, r, nil)
			return
		}
		mu.Lock()
		gets++
		n := gets
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		notification := `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
		if n == 1 {
			fmt.Fprintf(w, "id: g1\ndata: %s\n\n", notification)
			return
		}
		fmt.Fprintf(w, "id: g2\ndata: %s\n\n", notification)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	transport, err := NewStreamableHTTPTransport(StreamableHTTPTransportOptions{URL: f.url, Reconnect: ReconnectOptions{InitialDelay: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "http-test", Version: "1.0.0"}})
	changes := make(chan struct{}, 4)
	c.OnNotification("notifications/tools/list_changed", func(json.RawMessage) { changes <- struct{}{} })
	if _, err := c.Connect(context.Background(), transport); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-changes:
		case <-time.After(3 * time.Second):
			t.Fatalf("got %d list_changed notifications, want 2", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(lastIDs[:2], []string{"", "g1"}) {
		t.Fatalf("Last-Event-ID = %q", lastIDs)
	}
}

func TestStreamableHTTPRejectsRequestAcceptedWithoutResponse(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		if msg["method"] != "tools/call" {
			protocolHandler(f, w, r, msg)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool(context.Background(), "echo", nil, nil); err == nil || !strings.Contains(err.Error(), "without a response") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamableHTTPIncludesResponseBodyInErrors(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		_ = readMessage(r)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "Invalid Accept header")
	})
	_, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url})
	if err == nil || err.Error() != "MCP HTTP request failed with status 400: Invalid Accept header" {
		t.Fatalf("err = %v", err)
	}
}

type funcAuth struct {
	token          func() string
	onUnauthorized func(UnauthorizedContext) error
}

func (a *funcAuth) Token(context.Context) (string, error) { return a.token(), nil }
func (a *funcAuth) OnUnauthorized(_ context.Context, uc UnauthorizedContext) error {
	return a.onUnauthorized(uc)
}

func TestStreamableHTTPHandsUnauthorizedToAuthProvider(t *testing.T) {
	var mu sync.Mutex
	token := "old"
	type seenEntry struct {
		status int
		token  string
	}
	var seen []seenEntry
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer old" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if msg["method"] == "tools/call" && auth == "Bearer new" {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="admin"`)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		protocolHandler(f, w, r, msg)
	})
	auth := &funcAuth{
		token: func() string { mu.Lock(); defer mu.Unlock(); return token },
		onUnauthorized: func(uc UnauthorizedContext) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, seenEntry{uc.Response.StatusCode, uc.Token})
			if uc.Response.StatusCode == 401 {
				token = "new"
			} else {
				token = "admin"
			}
			return nil
		},
	}
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true, AuthProvider: auth})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(context.Background(), "echo", nil, nil)
	if err != nil || res.Content[0].Text != "hello" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []seenEntry{{401, "old"}, {403, "new"}}) {
		t.Fatalf("seen = %+v", seen)
	}
}

// #10598: a request cancelled or timed out while waiting for a shared refresh
// must not be retried.
func TestStreamableHTTPDoesNotRetryCancelledRequestsAfterAuthRecovery(t *testing.T) {
	var mu sync.Mutex
	token := "old"
	var executed []string
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			protocolHandler(f, w, r, nil)
			return
		}
		msg := readMessage(r)
		if msg["method"] == "tools/call" && r.Header.Get("Authorization") == "Bearer old" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if msg["method"] == "tools/call" {
			params, _ := msg["params"].(map[string]any)
			mu.Lock()
			executed = append(executed, fmt.Sprint(params["name"]))
			mu.Unlock()
		}
		protocolHandler(f, w, r, msg)
	})
	releaseRefresh := make(chan struct{})
	allWaiting := make(chan struct{})
	var once sync.Once
	var refreshOnce sync.Once
	refreshes, waiting := 0, 0
	auth := &funcAuth{
		token: func() string { mu.Lock(); defer mu.Unlock(); return token },
		onUnauthorized: func(UnauthorizedContext) error {
			mu.Lock()
			waiting++
			if waiting == 3 {
				once.Do(func() { close(allWaiting) })
			}
			mu.Unlock()
			<-releaseRefresh
			refreshOnce.Do(func() { mu.Lock(); refreshes++; token = "new"; mu.Unlock() })
			return nil
		},
	}
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true, AuthProvider: auth})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	aborted := make(chan error, 1)
	timedOut := make(chan error, 1)
	kept := make(chan error, 1)
	go func() { _, err := c.CallTool(ctx, "aborted", map[string]any{}, nil); aborted <- err }()
	go func() {
		_, err := c.CallTool(context.Background(), "timed-out", map[string]any{}, &RequestOptions{Timeout: 200 * time.Millisecond})
		timedOut <- err
	}()
	go func() { _, err := c.CallTool(context.Background(), "kept", map[string]any{}, nil); kept <- err }()
	select {
	case <-allWaiting:
	case <-time.After(3 * time.Second):
		t.Fatal("requests did not all reach the auth recovery")
	}
	cancel()
	var abortErr *AbortError
	if err := <-aborted; !errors.As(err, &abortErr) {
		t.Fatalf("aborted err = %v", err)
	}
	var timeoutErr *TimeoutError
	if err := <-timedOut; !errors.As(err, &timeoutErr) {
		t.Fatalf("timed-out err = %v", err)
	}
	close(releaseRefresh)
	if err := <-kept; err != nil {
		t.Fatalf("kept err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(executed, []string{"kept"}) || refreshes != 1 {
		t.Fatalf("executed %v, refreshes %d", executed, refreshes)
	}
}

// #10565: closing reuses the last request's token and never asks the auth
// provider, which may refresh over the network.
func TestStreamableHTTPClosesWithLastToken(t *testing.T) {
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) { protocolHandler(f, w, r, nil) })
	var mu sync.Mutex
	calls := 0
	auth := &funcAuth{
		token:          func() string { mu.Lock(); defer mu.Unlock(); calls++; return fmt.Sprintf("token-%d", calls) },
		onUnauthorized: func(UnauthorizedContext) error { return nil },
	}
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true, AuthProvider: auth})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	_ = c.Close()
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("close asked the auth provider (%d -> %d calls)", before, after)
	}
	var deletes []recordedRequest
	for _, r := range f.all() {
		if r.method == "DELETE" {
			deletes = append(deletes, r)
		}
	}
	if len(deletes) != 1 || deletes[0].headers.Get("Authorization") != fmt.Sprintf("Bearer token-%d", before) ||
		deletes[0].headers.Get("Mcp-Session-Id") != "session-1" {
		t.Fatalf("deletes = %+v", deletes)
	}
}

func TestStreamableHTTPClassifiesExpiredSession(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	f := startHTTPServer(t, func(f *httpFixture, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			n := posts
			posts++
			mu.Unlock()
			if n >= 2 {
				_ = readMessage(r)
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "gone")
				return
			}
		}
		protocolHandler(f, w, r, nil)
	})
	c, _, err := newHTTPClient(t, StreamableHTTPTransportOptions{URL: f.url, NoGetStream: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListTools(context.Background(), nil)
	var expired *SessionExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err = %#v", err)
	}
}
