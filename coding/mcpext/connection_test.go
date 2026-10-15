package mcpext

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// Ported from pi v1.2.0 packages/coding-agent/test/mcp-extension.test.ts,
// "MCP connections".

type fakeServerOptions struct {
	expireFirstCall bool
	noTools         bool
	resources       bool
	tools           []mcp.Tool
	instructions    string
}

type fakeServer struct {
	server  *mcp.InMemoryTransport
	mu      sync.Mutex
	methods []string
	tools   []mcp.Tool
}

func (f *fakeServer) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// setTools changes the listed tools and announces it.
func (f *fakeServer) setTools(tools []mcp.Tool) {
	f.mu.Lock()
	f.tools = tools
	f.mu.Unlock()
	_ = f.server.Send(context.Background(), mcp.Message{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
}

// newFakeServer is an in-memory server that answers initialize, tools/list,
// resources/read and tools/call (with "ok").
func newFakeServer(opts fakeServerOptions) (*fakeServer, mcp.Transport) {
	client, server := mcp.NewInMemoryTransportPair()
	f := &fakeServer{server: server, tools: opts.tools}
	server.OnMessage(func(m mcp.Message) {
		if len(m.ID) == 0 || m.Method == "" {
			return
		}
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		tools := f.tools
		f.mu.Unlock()
		reply := mcp.Message{JSONRPC: "2.0", ID: m.ID}
		switch m.Method {
		case "initialize":
			caps := map[string]any{}
			if opts.noTools {
				caps["prompts"] = map[string]any{}
			} else {
				caps["tools"] = map[string]any{"listChanged": true}
			}
			if opts.resources {
				caps["resources"] = map[string]any{}
			}
			result := map[string]any{"protocolVersion": mcp.LatestProtocolVersion, "capabilities": caps, "serverInfo": map[string]any{"name": "fake", "version": "1.0.0"}}
			if opts.instructions != "" {
				result["instructions"] = opts.instructions
			}
			reply.Result, _ = json.Marshal(result)
		case "tools/list":
			if opts.noTools {
				reply.Error = &mcp.ErrorObject{Code: -32601, Message: "Method not found"}
			} else {
				if tools == nil {
					tools = []mcp.Tool{}
				}
				reply.Result, _ = json.Marshal(map[string]any{"tools": tools})
			}
		case "resources/read":
			reply.Result = json.RawMessage(`{"contents":[{"uri":"docs://a","text":"ok"}]}`)
		case "resources/list":
			reply.Result = json.RawMessage(`{"resources":[{"uri":"docs://a","name":"a","_meta":{"x":1}},{"uri":"ui://app","name":"app"}]}`)
		case "resources/templates/list":
			reply.Error = &mcp.ErrorObject{Code: -32601, Message: "Method not found"}
		default:
			reply.Result = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)
		}
		go func() { _ = server.Send(context.Background(), reply) }()
	})
	_ = server.Start(context.Background())
	var transport mcp.Transport = client
	if opts.expireFirstCall {
		transport = &failingTransport{Transport: client, fail: func(m mcp.Message) error {
			if m.Method == "tools/call" {
				return &mcp.SessionExpiredError{HTTPError: mcp.HTTPError{Status: 404, Msg: "gone"}}
			}
			return nil
		}}
	}
	return f, transport
}

// failingTransport fails the messages fail picks.
type failingTransport struct {
	mcp.Transport
	fail func(mcp.Message) error
}

func (t *failingTransport) Send(ctx context.Context, m mcp.Message) error {
	if err := t.fail(m); err != nil {
		return err
	}
	return t.Transport.Send(ctx, m)
}

type testConn struct {
	*Connection
	opened atomic.Int32
}

func connectWith(t *testing.T, entry ServerEntry, transports ...func() mcp.Transport) *testConn {
	t.Helper()
	tc := &testConn{}
	tc.Connection = NewConnection(ConnectionOptions{
		Entry: entry,
		Cwd:   t.TempDir(),
		NewTransport: func(ServerEntry, string, mcp.AuthProvider) (mcp.Transport, error) {
			n := int(tc.opened.Add(1)) - 1
			if n >= len(transports) {
				return nil, errors.New("no more transports")
			}
			return transports[n](), nil
		},
	})
	t.Cleanup(tc.Close)
	return tc
}

func stdioEntry(name string) ServerEntry {
	return ServerEntry{Name: name, Config: ServerConfig{Command: "unused"}, Source: "test"}
}

func httpEntry(name string, headers map[string]string) ServerEntry {
	return ServerEntry{Name: name, Config: ServerConfig{URL: "http://unused.invalid", Headers: headers}, Source: "test"}
}

func fake(opts fakeServerOptions) func() mcp.Transport {
	return func() mcp.Transport { _, t := newFakeServer(opts); return t }
}

func okText(t *testing.T, r *mcp.CallToolResult) {
	t.Helper()
	if len(r.Content) != 1 || r.Content[0].Text != "ok" {
		t.Fatalf("result %+v", r)
	}
}

func TestSessionExpiredRetriesOnANewSession(t *testing.T) {
	c := connectWith(t, stdioEntry("fake"), fake(fakeServerOptions{expireFirstCall: true}), fake(fakeServerOptions{}))
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.CallTool(context.Background(), "echo", map[string]any{}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			okText(t, r)
		}()
	}
	wg.Wait()
	if c.opened.Load() != 2 {
		t.Fatalf("opened %d transports", c.opened.Load())
	}
}

func TestServerWithoutToolsIsNotListed(t *testing.T) {
	var server *fakeServer
	c := connectWith(t, stdioEntry("fake"), func() mcp.Transport {
		f, tr := newFakeServer(fakeServerOptions{noTools: true})
		server = f
		return tr
	})
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != StateConnected || len(c.Tools()) != 0 || strings.Join(server.Methods(), ",") != "initialize" {
		t.Fatalf("state %s tools %v methods %v", c.State(), c.Tools(), server.Methods())
	}
}

func TestDroppedConnectionReconnects(t *testing.T) {
	var servers []*fakeServer
	open := func() mcp.Transport {
		f, tr := newFakeServer(fakeServerOptions{})
		servers = append(servers, f)
		return tr
	}
	c := connectWith(t, stdioEntry("fake"), open, open)
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = servers[len(servers)-1].server.Close()
	waitUntil(t, func() bool { return c.State() == StateDisconnected })
	if c.Error() != "Connection closed" {
		t.Fatalf("error %q", c.Error())
	}
	r, err := c.CallTool(context.Background(), "echo", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	okText(t, r)
	if c.State() != StateConnected || c.opened.Load() != 2 {
		t.Fatalf("state %s opened %d", c.State(), c.opened.Load())
	}
}

func tool(name string) mcp.Tool {
	return mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func failAll(status int, msg string) func() mcp.Transport {
	return func() mcp.Transport {
		_, tr := newFakeServer(fakeServerOptions{})
		return &failingTransport{Transport: tr, fail: func(mcp.Message) error { return &mcp.HTTPError{Status: status, Msg: msg} }}
	}
}

func TestTransientHTTPConnectFailuresRetry(t *testing.T) {
	auth := map[string]string{"Authorization": "x"}
	c := connectWith(t, httpEntry("fake", auth), failAll(503, "MCP HTTP request failed with status 503"), fake(fakeServerOptions{}))
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != StateConnected || c.opened.Load() != 2 {
		t.Fatalf("state %s opened %d", c.State(), c.opened.Load())
	}

	failing := connectWith(t, httpEntry("fake", auth), failAll(400, "MCP HTTP request failed with status 400: bad"))
	_, err := failing.Client(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 400: bad") || failing.State() != StateFailed || failing.opened.Load() != 1 {
		t.Fatalf("err %v state %s opened %d", err, failing.State(), failing.opened.Load())
	}
}

func TestCloseStopsAnInitializingConnection(t *testing.T) {
	client, server := mcp.NewInMemoryTransportPair()
	initializing := make(chan struct{})
	var once sync.Once
	server.OnMessage(func(mcp.Message) { once.Do(func() { close(initializing) }) })
	_ = server.Start(context.Background())
	closed := make(chan struct{})
	client.OnClose(func() { close(closed) })
	c := connectWith(t, stdioEntry("fake"), func() mcp.Transport { return client })
	failure := make(chan error, 1)
	go func() { _, err := c.Client(context.Background()); failure <- err }()
	<-initializing
	c.Close()
	select {
	case <-closed:
	default:
		t.Fatal("transport not closed when Close returned")
	}
	if err := <-failure; err == nil || !strings.Contains(err.Error(), "failed to connect") {
		t.Fatalf("failure %v", err)
	}
}

func TestCloseStopsWaitingToRetry(t *testing.T) {
	c := connectWith(t, httpEntry("fake", map[string]string{"Authorization": "x"}), failAll(503, "MCP HTTP request failed with status 503"))
	failure := make(chan error, 1)
	go func() { _, err := c.Client(context.Background()); failure <- err }()
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	c.Close()
	if err := <-failure; err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("failure %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("close waited %v for the retry", time.Since(start))
	}
}

func TestReadsRetryButToolCallsDoNot(t *testing.T) {
	var mu sync.Mutex
	failed := map[string]bool{}
	c := connectWith(t, stdioEntry("fake"), func() mcp.Transport {
		_, tr := newFakeServer(fakeServerOptions{})
		return &failingTransport{Transport: tr, fail: func(m mcp.Message) error {
			mu.Lock()
			defer mu.Unlock()
			if (m.Method == "resources/read" || m.Method == "tools/call") && !failed[m.Method] {
				failed[m.Method] = true
				return &mcp.HTTPError{Status: 502, Msg: "MCP HTTP request failed with status 502"}
			}
			return nil
		}}
	})
	r, err := c.ReadResource(context.Background(), "docs://a", nil)
	if err != nil || len(r.Contents) != 1 || *r.Contents[0].Text != "ok" {
		t.Fatalf("read %+v %v", r, err)
	}
	if _, err := c.CallTool(context.Background(), "echo", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("call %v", err)
	}
}

func TestOAuthServerNeedsSignIn(t *testing.T) {
	c := connectWith(t, httpEntry("fake", nil), func() mcp.Transport {
		_, tr := newFakeServer(fakeServerOptions{})
		return &failingTransport{Transport: tr, fail: func(mcp.Message) error {
			return &mcp.AuthRequiredError{HTTPError: mcp.HTTPError{Status: 401, Msg: "MCP server requires authentication"}}
		}}
	})
	_, err := c.Client(context.Background())
	if err == nil || err.Error() != `MCP server "fake" requires OAuth sign-in, which this engine does not support yet. Configure an Authorization header or auth.provider for it.` {
		t.Fatalf("err %v", err)
	}
	if c.State() != StateNeedsAuth {
		t.Fatalf("state %s", c.State())
	}
}

func TestProviderTokenIsSentAndRejectionNeedsSignIn(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(401)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	c := NewConnection(ConnectionOptions{
		Entry: ServerEntry{Name: "radius", Config: ServerConfig{URL: "http://" + ln.Addr().String() + "/mcp", Auth: &AuthConfig{Provider: "radius"}}, Source: "test"},
		Cwd:   t.TempDir(),
		ProviderToken: func(_ context.Context, provider string) (string, error) {
			if provider == "radius" {
				return "tok", nil
			}
			return "", nil
		},
	})
	defer c.Close()
	_, err = c.Client(context.Background())
	if err == nil || err.Error() != `MCP server "radius" rejected the token of provider "radius"; sign in to "radius" again.` {
		t.Fatalf("err %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if c.State() != StateNeedsAuth || strings.Join(authorizations, ",") != "Bearer tok" {
		t.Fatalf("state %s authorizations %v", c.State(), authorizations)
	}
}

func TestServerLogMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.log")
	log := NewServerLog(path)
	var server *fakeServer
	c := NewConnection(ConnectionOptions{
		Entry: stdioEntry("fake"),
		Cwd:   t.TempDir(),
		NewTransport: func(ServerEntry, string, mcp.AuthProvider) (mcp.Transport, error) {
			f, tr := newFakeServer(fakeServerOptions{})
			server = f
			return tr, nil
		},
		Log: log.Write,
	})
	defer c.Close()
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	send := func(params string) {
		_ = server.server.Send(context.Background(), mcp.Message{JSONRPC: "2.0", Method: "notifications/message", Params: json.RawMessage(params)})
	}
	send(`{"level":"warning","logger":"db","data":"slow\nquery"}`)
	send(`{"level":"error","data":{"code":7}}`)
	want := "[fake] warning db: slow\n    query\n[fake] error {\"code\":7}\n"
	var got string
	waitUntil(t, func() bool {
		data, _ := os.ReadFile(path)
		got = regexp.MustCompile(`(?m)^\S+ `).ReplaceAllString(string(data), "")
		return got == want
	})
}

func TestServerWithResourcesIsNotListedAtConnect(t *testing.T) {
	var server *fakeServer
	c := connectWith(t, stdioEntry("fake"), func() mcp.Transport {
		f, tr := newFakeServer(fakeServerOptions{resources: true})
		server = f
		return tr
	})
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != StateConnected || !c.HasResources() || strings.Join(server.Methods(), ",") != "initialize,tools/list" {
		t.Fatalf("state %s resources %v methods %v", c.State(), c.HasResources(), server.Methods())
	}
}

func TestToolsListChangedRefreshesTools(t *testing.T) {
	var server *fakeServer
	var notified atomic.Int32
	c := NewConnection(ConnectionOptions{
		Entry: stdioEntry("fake"),
		Cwd:   t.TempDir(),
		NewTransport: func(ServerEntry, string, mcp.AuthProvider) (mcp.Transport, error) {
			f, tr := newFakeServer(fakeServerOptions{tools: []mcp.Tool{tool("a")}})
			server = f
			return tr, nil
		},
		OnTools: func(*Connection) { notified.Add(1) },
	})
	defer c.Close()
	if _, err := c.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.setTools([]mcp.Tool{tool("a"), tool("b")})
	waitUntil(t, func() bool { return len(c.Tools()) == 2 })
	if notified.Load() != 2 {
		t.Fatalf("OnTools called %d times", notified.Load())
	}
}

func TestStdioServerExpandsHome(t *testing.T) {
	if os.Getenv("GO_MCP_CWD_SERVER") == "1" {
		return
	}
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewConnection(ConnectionOptions{
		Entry: ServerEntry{Name: "home", Source: "test", Config: ServerConfig{
			Command: exe, Args: []string{"-test.run=^TestCwdServerHelper$"}, Cwd: "~/work",
			Env: map[string]string{"GO_MCP_CWD_SERVER": "1"},
		}},
		Cwd: t.TempDir(),
	})
	defer c.Close()
	r, err := c.CallTool(context.Background(), "cwd", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.EvalSymlinks(r.Content[0].Text)
	want, _ := filepath.EvalSymlinks(filepath.Join(home, "work"))
	if !strings.EqualFold(got, want) {
		t.Fatalf("server ran in %q, want %q", got, want)
	}
}

// TestCwdServerHelper is a stdio MCP server that answers every tool call with
// its working directory; TestStdioServerExpandsHome runs this test binary as it.
func TestCwdServerHelper(t *testing.T) {
	if os.Getenv("GO_MCP_CWD_SERVER") != "1" {
		t.Skip("helper process")
	}
	dec := json.NewDecoder(os.Stdin)
	for {
		var m mcp.Message
		if dec.Decode(&m) != nil {
			os.Exit(0)
		}
		if len(m.ID) == 0 {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "cwd", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "cwd", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			wd, _ := os.Getwd()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": wd}}}
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		os.Stdout.Write(append(data, '\n'))
	}
}
