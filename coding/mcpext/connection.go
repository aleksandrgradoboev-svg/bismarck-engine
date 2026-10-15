package mcpext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// The part of the extension that talks to servers (pi extensions/mcp/
// runtime.ts): one connection per configured server, connected lazily and
// reconnected when a call finds it gone.
//
// OAuth sign-in is not ported. A server with auth.provider gets the host's
// token for that provider (ConnectionOptions.ProviderToken); an HTTP server
// that answers 401 and has neither auth nor an Authorization header would need
// OAuth, and ends in the needs-auth state with a message saying so.

// DefaultTimeout is the per-request timeout when the config sets none.
const DefaultTimeout = 60 * time.Second

const stderrTailChars = 2_000

// connectRetryDelays are the delays between attempts to connect to an HTTP
// server that failed with a transient error.
var connectRetryDelays = []time.Duration{250 * time.Millisecond, 1_000 * time.Millisecond}

// ServerState is a connection's state.
type ServerState string

// Server states. Disconnected: the connection dropped (for example the stdio
// server exited); the next call reconnects.
const (
	StateConnecting   ServerState = "connecting"
	StateConnected    ServerState = "connected"
	StateDisconnected ServerState = "disconnected"
	StateNeedsAuth    ServerState = "needs-auth"
	StateFailed       ServerState = "failed"
	StateClosed       ServerState = "closed"
)

// TransportFactory creates the transport for a server. auth is nil unless the
// server uses auth.provider.
type TransportFactory func(entry ServerEntry, cwd string, auth mcp.AuthProvider) (mcp.Transport, error)

// DefaultClientInfo names this engine to servers: the module and its version
// from the build information.
func DefaultClientInfo() mcp.Implementation {
	version := "0.0.0"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range append([]*debug.Module{&info.Main}, info.Deps...) {
			if dep != nil && dep.Path == "github.com/aleksandrgradoboev-svg/bismarck-engine" && dep.Version != "" && dep.Version != "(devel)" {
				version = strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return mcp.Implementation{Name: "bismarck-engine", Version: version}
}

// expandHome expands "~" and "~/…" (also "~\…" on Windows), like a shell.
func expandHome(value string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(value, `~\`)) {
		return filepath.Join(home, value[2:])
	}
	return value
}

// DefaultTransport is the stdio or Streamable HTTP transport the config
// describes, with ${ENV} and !command values resolved.
func DefaultTransport(entry ServerEntry, cwd string, auth mcp.AuthProvider) (mcp.Transport, error) {
	cfg, name := entry.Config, entry.Name
	if cfg.IsHTTP() {
		headers, err := coding.ResolveHeadersOrError(cfg.Headers, fmt.Sprintf("MCP server %q", name), nil)
		if err != nil {
			return nil, err
		}
		return mcp.NewStreamableHTTPTransport(mcp.StreamableHTTPTransportOptions{URL: cfg.URL, Headers: headers, AuthProvider: auth})
	}
	env := map[string]string{}
	for key, value := range cfg.Env {
		v, err := coding.ResolveConfigValueOrError(value, fmt.Sprintf("MCP server %q env %q", name, key), nil)
		if err != nil {
			return nil, err
		}
		env[key] = v
	}
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = expandHome(a)
	}
	dir := cfg.Cwd
	if dir == "" {
		dir = "."
	}
	dir = expandHome(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return mcp.NewStdioTransport(mcp.StdioTransportOptions{Command: expandHome(cfg.Command), Args: args, Dir: dir, Env: env}), nil
}

// httpStatus is the HTTP status of an MCP HTTP error, or 0.
func httpStatus(err error) int {
	var h *mcp.HTTPError
	var a *mcp.AuthRequiredError
	var s *mcp.SessionExpiredError
	switch {
	case errors.As(err, &a):
		return a.Status
	case errors.As(err, &s):
		return s.Status
	case errors.As(err, &h):
		return h.Status
	}
	return 0
}

// isTransient: network failures and overloaded or restarting servers, which
// are worth another attempt.
func isTransient(err error) bool {
	if status := httpStatus(err); status != 0 {
		return status == 408 || status == 429 || (status >= 500 && status != 501)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr)
}

// usesOAuth: an HTTP server authenticates with OAuth unless the config supplies
// an Authorization header or auth.
func usesOAuth(entry ServerEntry) bool {
	cfg := entry.Config
	if !cfg.IsHTTP() || cfg.Auth != nil {
		return false
	}
	for header := range cfg.Headers {
		if strings.EqualFold(header, "authorization") {
			return false
		}
	}
	return true
}

func signInRequiredMessage(entry ServerEntry) string {
	if entry.Config.Auth != nil {
		return fmt.Sprintf("MCP server %q rejected the token of provider %q; sign in to %q again.", entry.Name, entry.Config.Auth.Provider, entry.Config.Auth.Provider)
	}
	return fmt.Sprintf("MCP server %q requires OAuth sign-in, which this engine does not support yet. Configure an Authorization header or auth.provider for it.", entry.Name)
}

// fileURL is pathToFileURL(path).href.
func fileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

func withoutTemplates[T any](list func() (T, error), empty T) (T, error) {
	v, err := list()
	var rpcErr *mcp.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == mcp.CodeMethodNotFound {
		return empty, nil
	}
	return v, err
}

// ConnectionOptions configure a Connection.
type ConnectionOptions struct {
	Entry ServerEntry
	Cwd   string
	// NewTransport creates transports; nil means DefaultTransport.
	NewTransport TransportFactory
	// ClientInfo names the client to servers; empty means DefaultClientInfo.
	ClientInfo mcp.Implementation
	// ProviderToken is the host's current token for a provider, for servers
	// with auth.provider. It is read on every request, so refreshes apply.
	ProviderToken func(ctx context.Context, provider string) (string, error)
	// OnTools is called when the server's tools are (re)listed.
	OnTools func(*Connection)
	// OnChange is called when the state, error or tools change.
	OnChange func(*Connection)
	// Log receives the server's log messages (notifications/message).
	Log func(server string, params json.RawMessage)
}

type opening struct {
	done   chan struct{}
	client *mcp.Client
	err    error
}

// Connection is one configured server (pi McpServerConnection). It reconnects
// lazily when a call finds the connection gone. It is safe for concurrent use.
type Connection struct {
	opts        ConnectionOptions
	auth        mcp.AuthProvider
	shutdownCtx context.Context
	shutdown    context.CancelFunc

	mu           sync.Mutex
	state        ServerState
	err          string
	tools        []mcp.Tool
	hasResources bool
	instructions string
	client       *mcp.Client
	opening      *opening
	stderrTail   string
}

type providerAuth struct {
	provider string
	token    func(ctx context.Context, provider string) (string, error)
}

func (p providerAuth) Token(ctx context.Context) (string, error) { return p.token(ctx, p.provider) }

// NewConnection creates a connection; it connects on the first call.
func NewConnection(opts ConnectionOptions) *Connection {
	if opts.NewTransport == nil {
		opts.NewTransport = DefaultTransport
	}
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = DefaultClientInfo()
	}
	c := &Connection{opts: opts, state: StateConnecting}
	c.shutdownCtx, c.shutdown = context.WithCancel(context.Background())
	if a := opts.Entry.Config.Auth; a != nil && opts.Entry.Config.IsHTTP() {
		token := opts.ProviderToken
		if token == nil {
			token = func(context.Context, string) (string, error) {
				return "", fmt.Errorf("MCP server %q uses auth.provider %q, but the host supplies no provider tokens", opts.Entry.Name, a.Provider)
			}
		}
		c.auth = providerAuth{provider: a.Provider, token: token}
	}
	return c
}

// Name is the server's name.
func (c *Connection) Name() string { return c.opts.Entry.Name }

// Entry is the server's configuration.
func (c *Connection) Entry() ServerEntry { return c.opts.Entry }

// Timeout is the per-request timeout.
func (c *Connection) Timeout() time.Duration {
	if t := c.opts.Entry.Config.Timeout; t > 0 {
		return time.Duration(t * float64(time.Second))
	}
	return DefaultTimeout
}

// State, Error, Tools, HasResources and Instructions read the connection's
// current status. HasResources: the server offers resources, which the
// resource tools list and read on demand. Instructions are the server's
// instructions from initialize.
func (c *Connection) State() ServerState { c.mu.Lock(); defer c.mu.Unlock(); return c.state }
func (c *Connection) Error() string      { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *Connection) Tools() []mcp.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mcp.Tool(nil), c.tools...)
}
func (c *Connection) HasResources() bool   { c.mu.Lock(); defer c.mu.Unlock(); return c.hasResources }
func (c *Connection) Instructions() string { c.mu.Lock(); defer c.mu.Unlock(); return c.instructions }

func (c *Connection) closed() bool { return c.shutdownCtx.Err() != nil }

func (c *Connection) changed() {
	if c.opts.OnChange != nil {
		c.opts.OnChange(c)
	}
}

// Client returns a connected client, connecting first if needed. Concurrent
// callers share one connect; ctx only bounds this caller's wait.
func (c *Connection) Client(ctx context.Context) (*mcp.Client, error) {
	c.mu.Lock()
	if c.closed() {
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP server %q is shut down", c.Name())
	}
	if c.client != nil && c.client.State() == mcp.StateConnected {
		client := c.client
		c.mu.Unlock()
		return client, nil
	}
	op := c.opening
	if op == nil {
		op = &opening{done: make(chan struct{})}
		c.opening = op
		go func() {
			op.client, op.err = c.open()
			c.mu.Lock()
			c.opening = nil
			c.mu.Unlock()
			close(op.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-op.done:
		return op.client, op.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CallTool calls a tool. Tool calls are not retried after a transient error,
// since they may have run.
func (c *Connection) CallTool(ctx context.Context, name string, args map[string]any, opts *mcp.RequestOptions) (*mcp.CallToolResult, error) {
	return withClient(c, ctx, false, func(client *mcp.Client) (*mcp.CallToolResult, error) {
		return client.CallTool(ctx, name, args, opts)
	})
}

// ReadResource reads a resource.
func (c *Connection) ReadResource(ctx context.Context, uri string, opts *mcp.RequestOptions) (*mcp.ReadResourceResult, error) {
	return withClient(c, ctx, true, func(client *mcp.Client) (*mcp.ReadResourceResult, error) {
		return client.ReadResource(ctx, uri, opts)
	})
}

// ResourcesPage lists one page of resources.
func (c *Connection) ResourcesPage(ctx context.Context, cursor string, opts *mcp.RequestOptions) (*mcp.ListResourcesResult, error) {
	return withClient(c, ctx, true, func(client *mcp.Client) (*mcp.ListResourcesResult, error) {
		return client.ListResourcesPage(ctx, cursor, opts)
	})
}

// ResourceTemplatesPage lists one page of resource templates; servers that
// do not implement resources/templates/list have none.
func (c *Connection) ResourceTemplatesPage(ctx context.Context, cursor string, opts *mcp.RequestOptions) (*mcp.ListResourceTemplatesResult, error) {
	return withClient(c, ctx, true, func(client *mcp.Client) (*mcp.ListResourceTemplatesResult, error) {
		return withoutTemplates(func() (*mcp.ListResourceTemplatesResult, error) {
			return client.ListResourceTemplatesPage(ctx, cursor, opts)
		}, &mcp.ListResourceTemplatesResult{})
	})
}

// AllResources lists every resource.
func (c *Connection) AllResources(ctx context.Context, opts *mcp.RequestOptions) ([]mcp.Resource, error) {
	return withClient(c, ctx, true, func(client *mcp.Client) ([]mcp.Resource, error) {
		return client.ListResources(ctx, opts)
	})
}

// AllResourceTemplates lists every resource template.
func (c *Connection) AllResourceTemplates(ctx context.Context, opts *mcp.RequestOptions) ([]mcp.ResourceTemplate, error) {
	return withClient(c, ctx, true, func(client *mcp.Client) ([]mcp.ResourceTemplate, error) {
		return withoutTemplates(func() ([]mcp.ResourceTemplate, error) { return client.ListResourceTemplates(ctx, opts) }, nil)
	})
}

// withClient runs a request, reconnecting when needed. readOnly requests are
// retried once after a transient HTTP error.
func withClient[T any](c *Connection, ctx context.Context, readOnly bool, run func(*mcp.Client) (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		client, err := c.Client(ctx)
		if err != nil {
			return zero, err
		}
		v, err := run(client)
		if err == nil {
			return v, nil
		}
		if readOnly && attempt == 1 && httpStatus(err) != 0 && isTransient(err) {
			select {
			case <-time.After(connectRetryDelays[0]):
			case <-ctx.Done():
				return zero, ctx.Err()
			}
			continue
		}
		var expired *mcp.SessionExpiredError
		if errors.As(err, &expired) && attempt == 1 {
			// The server no longer knows the session (restart, deploy), so it
			// did not run the request. Retry once on a new session. The old
			// client is detached but not closed: closing would fail its other
			// in-flight calls, which instead get the same 404 and retry.
			c.mu.Lock()
			if c.client == client {
				c.client = nil
			}
			c.mu.Unlock()
			continue
		}
		if !c.needsSignIn(err) {
			return zero, err
		}
		c.dropClient(client)
		c.markNeedsAuth()
		return zero, errors.New(signInRequiredMessage(c.opts.Entry))
	}
}

// needsSignIn: a 401 from a server that authenticates with a provider token
// or would need OAuth.
func (c *Connection) needsSignIn(err error) bool {
	var authErr *mcp.AuthRequiredError
	return errors.As(err, &authErr) && (c.auth != nil || usesOAuth(c.opts.Entry))
}

func (c *Connection) markNeedsAuth() {
	c.mu.Lock()
	c.state, c.err = StateNeedsAuth, ""
	c.mu.Unlock()
	c.changed()
}

func (c *Connection) dropClient(client *mcp.Client) {
	c.mu.Lock()
	if c.client == client {
		c.client = nil
	}
	c.mu.Unlock()
	_ = client.Close()
}

// Reconnect connects again, for example after the credentials changed.
func (c *Connection) Reconnect(ctx context.Context) error {
	c.waitOpening()
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client != nil {
		c.dropClient(client)
	}
	_, err := c.Client(ctx)
	return err
}

func (c *Connection) waitOpening() {
	c.mu.Lock()
	op := c.opening
	c.mu.Unlock()
	if op != nil {
		<-op.done
	}
}

func (c *Connection) open() (*mcp.Client, error) {
	c.mu.Lock()
	c.state = StateConnecting
	c.mu.Unlock()
	c.changed()
	var retries []time.Duration
	if c.opts.Entry.Config.IsHTTP() {
		retries = connectRetryDelays
	}
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		c.stderrTail = ""
		c.mu.Unlock()
		client, err := c.connectOnce()
		if err == nil {
			return client, nil
		}
		if c.closed() || attempt >= len(retries) || !isTransient(err) {
			return nil, c.connectFailed(err)
		}
		select {
		case <-time.After(retries[attempt]):
		case <-c.shutdownCtx.Done():
			return nil, c.connectFailed(err)
		}
	}
}

func (c *Connection) connectOnce() (*mcp.Client, error) {
	cwd := c.opts.Cwd
	client := mcp.NewClient(mcp.ClientOptions{
		Implementation: c.opts.ClientInfo,
		RequestTimeout: c.Timeout(),
		Roots: func(context.Context) ([]mcp.Root, error) {
			return []mcp.Root{{URI: fileURL(cwd), Name: filepath.Base(cwd)}}, nil
		},
	})
	if log := c.opts.Log; log != nil {
		client.OnNotification("notifications/message", func(params json.RawMessage) { log(c.Name(), params) })
	}
	// Closing the client fails its pending initialize or discovery requests
	// and closes the transport.
	stopOnShutdown := context.AfterFunc(c.shutdownCtx, func() { _ = client.Close() })
	defer stopOnShutdown()

	transport, err := c.opts.NewTransport(c.opts.Entry, cwd, c.auth)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	stdio, _ := transport.(*mcp.StdioTransport)
	fail := func(err error) (*mcp.Client, error) {
		_ = client.Close()
		if stdio != nil {
			tail := strings.TrimSpace(stdio.Stderr())
			if len(tail) > stderrTailChars {
				tail = tail[len(tail)-stderrTailChars:]
			}
			c.mu.Lock()
			c.stderrTail = tail
			c.mu.Unlock()
		}
		return nil, err
	}
	if _, err := client.Connect(c.shutdownCtx, transport); err != nil {
		return fail(err)
	}
	client.OnNotification("notifications/tools/list_changed", func(json.RawMessage) { go c.refreshTools(client) })
	client.OnClose(func() { c.handleClientClose(client, stdio) })
	var tools []mcp.Tool
	// Servers without the tools capability (prompts or resources only) do
	// not answer tools/list.
	caps := client.ServerCapabilities()
	if caps != nil && caps.Tools != nil {
		if tools, err = client.ListTools(c.shutdownCtx, nil); err != nil {
			return fail(err)
		}
	}
	if c.closed() {
		return fail(errors.New("shut down while connecting"))
	}
	if client.State() != mcp.StateConnected {
		return fail(errors.New("connection closed during setup"))
	}
	c.mu.Lock()
	c.client = client
	c.tools = tools
	c.hasResources = caps != nil && caps.Resources != nil
	c.instructions = strings.TrimSpace(client.Instructions())
	c.state, c.err = StateConnected, ""
	c.mu.Unlock()
	if c.opts.OnTools != nil {
		c.opts.OnTools(c)
	}
	c.changed()
	return client, nil
}

func (c *Connection) connectFailed(err error) error {
	if c.needsSignIn(err) && !c.closed() {
		c.markNeedsAuth()
		return errors.New(signInRequiredMessage(c.opts.Entry))
	}
	c.mu.Lock()
	c.state = StateFailed
	if c.closed() {
		c.state = StateClosed
	}
	c.err = err.Error()
	if c.stderrTail != "" {
		c.err += "\n" + c.stderrTail
	}
	message := c.err
	c.mu.Unlock()
	c.changed()
	return fmt.Errorf("MCP server %q failed to connect: %s", c.Name(), message)
}

// handleClientClose: the transport dropped. The next call reconnects; until
// then the status shows why.
func (c *Connection) handleClientClose(client *mcp.Client, stdio *mcp.StdioTransport) {
	c.mu.Lock()
	if c.client != client || c.closed() {
		c.mu.Unlock()
		return
	}
	c.client = nil
	c.state = StateDisconnected
	c.err = "Connection closed"
	if stdio != nil {
		if tail := strings.TrimSpace(stdio.Stderr()); tail != "" {
			if len(tail) > stderrTailChars {
				tail = tail[len(tail)-stderrTailChars:]
			}
			c.err += "\n" + tail
		}
	}
	c.mu.Unlock()
	c.changed()
}

func (c *Connection) refreshTools(client *mcp.Client) {
	tools, err := client.ListTools(c.shutdownCtx, nil)
	c.mu.Lock()
	if err != nil {
		c.err = "Failed to refresh tools: " + err.Error()
		c.mu.Unlock()
		c.changed()
		return
	}
	if c.client != client || c.closed() {
		c.mu.Unlock()
		return
	}
	c.tools = tools
	c.mu.Unlock()
	if c.opts.OnTools != nil {
		c.opts.OnTools(c)
	}
	c.changed()
}

// Close shuts the connection down. It returns once no transport of this
// server is open, including one that was still connecting.
func (c *Connection) Close() {
	c.shutdown()
	c.mu.Lock()
	c.state = StateClosed
	client := c.client
	c.client = nil
	c.mu.Unlock()
	c.changed()
	if client != nil {
		_ = client.Close()
	}
	c.waitOpening()
}
