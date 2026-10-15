package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// DefaultRequestTimeout bounds a request that sets no timeout of its own.
const DefaultRequestTimeout = 30 * time.Second

const maxListPages = 1_000

// ClientState is the connection's lifecycle state.
type ClientState string

// Client states.
const (
	StateIdle       ClientState = "idle"
	StateConnecting ClientState = "connecting"
	StateConnected  ClientState = "connected"
	StateClosed     ClientState = "closed"
)

// RequestHandler answers a request the server sends to the client. ctx is
// cancelled when the server cancels the request or the connection closes. A
// returned *Error goes back as is; any other error as an internal error.
type RequestHandler func(ctx context.Context, params json.RawMessage) (any, error)

// ClientOptions configure a Client (pi McpClientOptions).
type ClientOptions struct {
	Implementation
	Capabilities ClientCapabilities
	// ProtocolVersion is the version to ask for; empty means
	// LatestProtocolVersion.
	ProtocolVersion string
	// RequestTimeout bounds each request. Zero means DefaultRequestTimeout; a
	// negative value means no timeout.
	RequestTimeout time.Duration
	// Roots answers roots/list. Nil offers no roots.
	Roots func(ctx context.Context) ([]Root, error)
}

// RequestOptions are per-request settings (pi McpRequestOptions; the signal is
// the context passed with the request).
type RequestOptions struct {
	// Timeout overrides ClientOptions.RequestTimeout (zero keeps it, negative
	// means none). Progress notifications restart it.
	Timeout time.Duration
	// OnProgress asks the server for progress notifications and receives them.
	OnProgress func(ProgressNotification)
}

type result struct {
	value json.RawMessage
	err   error
}

type handlerEntry struct {
	id      int
	handler RequestHandler
}

type pendingRequest struct {
	ch          chan result
	timeout     time.Duration
	timer       *time.Timer
	cancelSend  context.CancelFunc
	cancellable bool
	onProgress  func(ProgressNotification)
	progressKey string
	id          json.RawMessage
}

// Client is an MCP client over one transport (pi McpClient).
type Client struct {
	opts ClientOptions

	mu              sync.Mutex
	state           ClientState
	transport       Transport
	nextRequestID   int64
	serverInfo      *Implementation
	serverCaps      *ServerCapabilities
	instructions    string
	protocolVersion string
	pending         map[string]*pendingRequest
	progress        map[string]string // progress token key -> request key
	incoming        map[string]context.CancelFunc
	handlers        map[string]handlerEntry
	notification    map[string]map[int]func(json.RawMessage)
	onError         map[int]func(error)
	onClose         map[int]func()
	nextListenerID  int
	disposers       []func()
}

// NewClient returns an idle client.
func NewClient(opts ClientOptions) *Client {
	c := &Client{
		opts:          opts,
		state:         StateIdle,
		nextRequestID: 1,
		pending:       map[string]*pendingRequest{},
		progress:      map[string]string{},
		incoming:      map[string]context.CancelFunc{},
		handlers:      map[string]handlerEntry{},
		notification:  map[string]map[int]func(json.RawMessage){},
		onError:       map[int]func(error){},
		onClose:       map[int]func(){},
	}
	c.handlers["ping"] = handlerEntry{id: -1, handler: func(context.Context, json.RawMessage) (any, error) { return map[string]any{}, nil }}
	if opts.Roots != nil {
		roots := opts.Roots
		c.handlers["roots/list"] = handlerEntry{id: -2, handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			list, err := roots(ctx)
			if err != nil {
				return nil, err
			}
			if list == nil {
				list = []Root{}
			}
			return map[string]any{"roots": list}, nil
		}}
	}
	return c
}

// State is the connection state.
func (c *Client) State() ClientState { c.mu.Lock(); defer c.mu.Unlock(); return c.state }

// ServerInfo is the server's implementation, once connected.
func (c *Client) ServerInfo() *Implementation { c.mu.Lock(); defer c.mu.Unlock(); return c.serverInfo }

// ServerCapabilities are the server's capabilities, once connected.
func (c *Client) ServerCapabilities() *ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCaps
}

// Instructions are the server's instructions, once connected.
func (c *Client) Instructions() string { c.mu.Lock(); defer c.mu.Unlock(); return c.instructions }

// ProtocolVersion is the negotiated protocol version, once connected.
func (c *Client) ProtocolVersion() string { c.mu.Lock(); defer c.mu.Unlock(); return c.protocolVersion }

// Connect starts the transport and runs the initialize handshake. On failure
// the client is closed.
func (c *Client) Connect(ctx context.Context, transport Transport) (*InitializeResult, error) {
	c.mu.Lock()
	if c.state != StateIdle {
		state := c.state
		c.mu.Unlock()
		return nil, fmt.Errorf("Cannot connect MCP client in %s state", state)
	}
	c.state = StateConnecting
	c.transport = transport
	c.disposers = []func(){
		transport.OnMessage(c.handleMessage),
		// Transport errors are reported only. Pending requests fail when the
		// transport closes.
		transport.OnError(c.emitError),
		transport.OnClose(func() { c.markClosed(&ConnectionClosedError{}) }),
	}
	c.mu.Unlock()

	res, err := c.initialize(ctx, transport)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return res, nil
}

func (c *Client) initialize(ctx context.Context, transport Transport) (*InitializeResult, error) {
	if err := transport.Start(ctx); err != nil {
		return nil, err
	}
	caps := c.opts.Capabilities
	if c.opts.Roots != nil && caps.Roots == nil {
		caps.Roots = &RootsCapability{}
	}
	version := c.opts.ProtocolVersion
	if version == "" {
		version = LatestProtocolVersion
	}
	params := map[string]any{
		"protocolVersion": version,
		"capabilities":    caps,
		"clientInfo":      c.opts.Implementation,
	}
	raw, err := c.requestInternal(ctx, "initialize", params, nil, true)
	if err != nil {
		return nil, err
	}
	res, err := validateInitializeResult(raw)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(SupportedProtocolVersions, res.ProtocolVersion) {
		return nil, fmt.Errorf("MCP server selected unsupported protocol version %s", res.ProtocolVersion)
	}
	c.mu.Lock()
	c.protocolVersion = res.ProtocolVersion
	c.serverInfo = &res.ServerInfo
	c.serverCaps = &res.Capabilities
	c.instructions = res.Instructions
	c.mu.Unlock()
	if setter, ok := transport.(ProtocolVersionSetter); ok {
		setter.SetProtocolVersion(res.ProtocolVersion)
	}
	if err := c.notifyInternal(ctx, "notifications/initialized", nil, true); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.state = StateConnected
	c.mu.Unlock()
	return res, nil
}

// Request sends a request and waits for its result. ctx cancels it: the
// server is told with notifications/cancelled, as is a request that times out
// (initialize excepted, which the spec forbids cancelling).
func (c *Client) Request(ctx context.Context, method string, params any, opts *RequestOptions) (json.RawMessage, error) {
	return c.requestInternal(ctx, method, params, opts, false)
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	return c.notifyInternal(ctx, method, params, false)
}

// SetRequestHandler answers requests of method from the server; the returned
// function removes the handler if it is still the current one.
func (c *Client) SetRequestHandler(method string, handler RequestHandler) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextListenerID
	c.nextListenerID++
	c.handlers[method] = handlerEntry{id: id, handler: handler}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if current, ok := c.handlers[method]; ok && current.id == id {
			delete(c.handlers, method)
		}
	}
}

// OnNotification registers a listener for notifications of method.
func (c *Client) OnNotification(method string, listener func(params json.RawMessage)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notification[method] == nil {
		c.notification[method] = map[int]func(json.RawMessage){}
	}
	id := c.nextListenerID
	c.nextListenerID++
	c.notification[method][id] = listener
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.notification[method], id)
		if len(c.notification[method]) == 0 {
			delete(c.notification, method)
		}
	}
}

// OnError registers a listener for transport and protocol errors.
func (c *Client) OnError(listener func(error)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextListenerID
	c.nextListenerID++
	c.onError[id] = listener
	return func() { c.mu.Lock(); delete(c.onError, id); c.mu.Unlock() }
}

// OnClose registers a listener called once when the connection closes,
// whether the transport dropped or Close was called.
func (c *Client) OnClose(listener func()) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextListenerID
	c.nextListenerID++
	c.onClose[id] = listener
	return func() { c.mu.Lock(); delete(c.onClose, id); c.mu.Unlock() }
}

// Ping checks the connection.
func (c *Client) Ping(ctx context.Context, opts *RequestOptions) error {
	_, err := c.Request(ctx, "ping", nil, opts)
	return err
}

// ListTools returns every tool, following nextCursor through all pages.
func (c *Client) ListTools(ctx context.Context, opts *RequestOptions) ([]Tool, error) {
	items, err := c.listAll(ctx, "tools/list", "tools", isTool, opts)
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(items))
	for _, item := range items {
		var tool Tool
		if err := json.Unmarshal(item, &tool); err != nil {
			return nil, invalid("Invalid entry in MCP tools/list result")
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// ListResources returns every resource, following nextCursor through all
// pages.
func (c *Client) ListResources(ctx context.Context, opts *RequestOptions) ([]Resource, error) {
	items, err := c.listAll(ctx, "resources/list", "resources", isResource, opts)
	if err != nil {
		return nil, err
	}
	return toResources(items)
}

// ListResourcesPage returns one page of resources, starting at cursor.
func (c *Client) ListResourcesPage(ctx context.Context, cursor string, opts *RequestOptions) (*ListResourcesResult, error) {
	items, next, err := c.listPage(ctx, "resources/list", "resources", isResource, cursor, opts)
	if err != nil {
		return nil, err
	}
	resources, err := toResources(items)
	if err != nil {
		return nil, err
	}
	return &ListResourcesResult{Resources: resources, NextCursor: next}, nil
}

// ListResourceTemplates returns every resource template, following nextCursor
// through all pages.
func (c *Client) ListResourceTemplates(ctx context.Context, opts *RequestOptions) ([]ResourceTemplate, error) {
	items, err := c.listAll(ctx, "resources/templates/list", "resourceTemplates", isResourceTemplate, opts)
	if err != nil {
		return nil, err
	}
	return toResourceTemplates(items)
}

// ListResourceTemplatesPage returns one page of resource templates.
func (c *Client) ListResourceTemplatesPage(ctx context.Context, cursor string, opts *RequestOptions) (*ListResourceTemplatesResult, error) {
	items, next, err := c.listPage(ctx, "resources/templates/list", "resourceTemplates", isResourceTemplate, cursor, opts)
	if err != nil {
		return nil, err
	}
	templates, err := toResourceTemplates(items)
	if err != nil {
		return nil, err
	}
	return &ListResourceTemplatesResult{ResourceTemplates: templates, NextCursor: next}, nil
}

// ReadResource reads one resource.
func (c *Client) ReadResource(ctx context.Context, uri string, opts *RequestOptions) (*ReadResourceResult, error) {
	raw, err := c.Request(ctx, "resources/read", map[string]any{"uri": uri}, opts)
	if err != nil {
		return nil, err
	}
	return validateReadResourceResult(raw)
}

// CallTool calls a tool. args nil sends no arguments.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, opts *RequestOptions) (*CallToolResult, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	raw, err := c.Request(ctx, "tools/call", params, opts)
	if err != nil {
		return nil, err
	}
	return validateCallToolResult(raw)
}

// Close closes the connection; pending requests fail with
// *ConnectionClosedError.
func (c *Client) Close() error {
	c.mu.Lock()
	transport := c.transport
	c.transport = nil
	disposers := c.disposers
	c.disposers = nil
	c.mu.Unlock()
	for _, dispose := range disposers {
		dispose()
	}
	c.markClosed(&ConnectionClosedError{})
	if transport != nil {
		return transport.Close()
	}
	return nil
}

func (c *Client) listPage(ctx context.Context, method, key string, isItem func(rawMessage) bool, cursor string, opts *RequestOptions) ([]json.RawMessage, string, error) {
	var params any
	if cursor != "" {
		params = map[string]any{"cursor": cursor}
	}
	raw, err := c.Request(ctx, method, params, opts)
	if err != nil {
		return nil, "", err
	}
	return validateListPage(method, key, raw, isItem)
}

func (c *Client) listAll(ctx context.Context, method, key string, isItem func(rawMessage) bool, opts *RequestOptions) ([]json.RawMessage, error) {
	var items []json.RawMessage
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		pageItems, next, err := c.listPage(ctx, method, key, isItem, cursor, opts)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)
		if next == "" {
			return items, nil
		}
		if cursors[next] {
			return nil, fmt.Errorf("MCP %s returned duplicate cursor: %s", method, next)
		}
		cursors[next] = true
		cursor = next
	}
	return nil, fmt.Errorf("MCP %s exceeded %d pages", method, maxListPages)
}

func (c *Client) requestInternal(ctx context.Context, method string, params any, opts *RequestOptions, allowConnecting bool) (json.RawMessage, error) {
	if opts == nil {
		opts = &RequestOptions{}
	}
	c.mu.Lock()
	transport, err := c.requireTransportLocked(allowConnecting)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if ctx.Err() != nil {
		c.mu.Unlock()
		return nil, &AbortError{}
	}
	id := NumberID(c.nextRequestID)
	c.nextRequestID++
	key := IDKey(id)
	paramsRaw, err := buildParams(params, opts.OnProgress != nil, id)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	timeout := c.opts.RequestTimeout
	if opts.Timeout != 0 {
		timeout = opts.Timeout
	}
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	sendCtx, cancelSend := context.WithCancel(context.Background())
	entry := &pendingRequest{
		ch:          make(chan result, 1),
		timeout:     timeout,
		cancelSend:  cancelSend,
		cancellable: method != "initialize", // the spec forbids cancelling initialize
		onProgress:  opts.OnProgress,
		id:          id,
	}
	if opts.OnProgress != nil {
		entry.progressKey = key
		c.progress[key] = key
	}
	c.pending[key] = entry
	c.armTimeoutLocked(key, entry)
	c.mu.Unlock()

	msg := Message{JSONRPC: "2.0", ID: id, Method: method, Params: paramsRaw}
	go func() {
		if err := transport.Send(sendCtx, msg); err != nil {
			c.cancelPending(key, err, false, "")
		}
	}()

	select {
	case r := <-entry.ch:
		return r.value, r.err
	case <-ctx.Done():
		reason := "Aborted"
		if cause := context.Cause(ctx); cause != nil {
			reason = cause.Error()
		}
		c.cancelPending(key, &AbortError{}, entry.cancellable, reason)
		r := <-entry.ch
		return r.value, r.err
	}
}

// buildParams marshals params and, when progress is wanted, adds
// _meta.progressToken (the request id), keeping any _meta already there.
func buildParams(params any, withProgress bool, id json.RawMessage) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := marshalNoEscape(params)
		if err != nil {
			return nil, err
		}
		raw = data
	}
	if !withProgress {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
	}
	meta := map[string]json.RawMessage{}
	if isObject(fields["_meta"]) {
		_ = json.Unmarshal(fields["_meta"], &meta)
	}
	meta["progressToken"] = id
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	fields["_meta"] = metaRaw
	return json.Marshal(fields)
}

func (c *Client) notifyInternal(ctx context.Context, method string, params any, allowConnecting bool) error {
	c.mu.Lock()
	transport, err := c.requireTransportLocked(allowConnecting)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if params != nil {
		data, err := marshalNoEscape(params)
		if err != nil {
			return err
		}
		raw = data
	}
	return transport.Send(ctx, Message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Client) requireTransportLocked(allowConnecting bool) (Transport, error) {
	if c.transport != nil && (c.state == StateConnected || allowConnecting && c.state == StateConnecting) {
		return c.transport, nil
	}
	return nil, &ConnectionClosedError{Msg: "MCP client is " + string(c.state)}
}

func (c *Client) handleMessage(msg Message) {
	switch {
	case msg.IsResponse():
		c.handleResponse(msg)
	case msg.IsRequest():
		go c.handleRequest(msg)
	case msg.IsNotification():
		c.handleNotification(msg.Method, msg.Params)
	default:
		c.emitError(&Error{Code: CodeInvalidRequest, Message: "Received invalid JSON-RPC message"})
	}
}

func (c *Client) handleResponse(msg Message) {
	key := IDKey(msg.ID)
	c.mu.Lock()
	entry, ok := c.pending[key]
	if !ok {
		c.mu.Unlock()
		c.emitError(fmt.Errorf("Received response for unknown MCP request %s", string(msg.ID)))
		return
	}
	c.removePendingLocked(key, entry)
	c.mu.Unlock()
	if msg.Error != nil {
		entry.ch <- result{err: &Error{Code: msg.Error.Code, Message: msg.Error.Message, Data: msg.Error.Data}}
		return
	}
	entry.ch <- result{value: msg.Result}
}

func (c *Client) handleRequest(msg Message) {
	c.mu.Lock()
	transport := c.transport
	entry, ok := c.handlers[msg.Method]
	handler := entry.handler
	c.mu.Unlock()
	if transport == nil {
		return
	}
	if !ok {
		err := transport.Send(context.Background(), Message{
			JSONRPC: "2.0", ID: msg.ID,
			Error: &ErrorObject{Code: CodeMethodNotFound, Message: "Method not found: " + msg.Method},
		})
		if err != nil {
			c.emitError(err)
		}
		return
	}
	key := IDKey(msg.ID)
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.incoming[key] = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.incoming, key)
		c.mu.Unlock()
		cancel()
	}()

	value, err := handler(ctx, msg.Params)
	var response Message
	if err != nil {
		var mcpErr *Error
		if errors.As(err, &mcpErr) {
			response = Message{JSONRPC: "2.0", ID: msg.ID, Error: &ErrorObject{Code: mcpErr.Code, Message: mcpErr.Message, Data: mcpErr.Data}}
		} else {
			response = Message{JSONRPC: "2.0", ID: msg.ID, Error: &ErrorObject{Code: CodeInternalError, Message: err.Error()}}
		}
	} else {
		if value == nil {
			value = map[string]any{}
		}
		data, merr := marshalNoEscape(value)
		if merr != nil {
			response = Message{JSONRPC: "2.0", ID: msg.ID, Error: &ErrorObject{Code: CodeInternalError, Message: merr.Error()}}
		} else {
			response = Message{JSONRPC: "2.0", ID: msg.ID, Result: data}
		}
	}
	if err := transport.Send(context.Background(), response); err != nil {
		c.emitError(err)
	}
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "notifications/progress":
		c.handleProgress(params)
	case "notifications/cancelled":
		c.handleCancelled(params)
	}
	c.mu.Lock()
	listeners := make([]func(json.RawMessage), 0, len(c.notification[method]))
	for _, l := range c.notification[method] {
		listeners = append(listeners, l)
	}
	c.mu.Unlock()
	for _, l := range listeners {
		l(params)
	}
}

func (c *Client) handleProgress(params json.RawMessage) {
	var fields rawMessage
	if json.Unmarshal(params, &fields) != nil || !validID(fields["progressToken"]) || jsonType(fields["progress"]) != "number" {
		return
	}
	var progress ProgressNotification
	if json.Unmarshal(params, &progress) != nil {
		return
	}
	c.mu.Lock()
	requestKey, ok := c.progress[IDKey(fields["progressToken"])]
	entry := c.pending[requestKey]
	if !ok || entry == nil {
		c.mu.Unlock()
		return
	}
	c.armTimeoutLocked(requestKey, entry)
	onProgress := entry.onProgress
	c.mu.Unlock()
	if onProgress != nil {
		onProgress(progress)
	}
}

func (c *Client) handleCancelled(params json.RawMessage) {
	var fields rawMessage
	if json.Unmarshal(params, &fields) != nil || !validID(fields["requestId"]) {
		return
	}
	c.mu.Lock()
	cancel := c.incoming[IDKey(fields["requestId"])]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) armTimeoutLocked(key string, entry *pendingRequest) {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.timeout <= 0 {
		return
	}
	timeout := entry.timeout
	entry.timer = time.AfterFunc(timeout, func() {
		c.cancelPending(key, &TimeoutError{TimeoutMs: timeout.Milliseconds()}, entry.cancellable, "Request timed out")
	})
}

func (c *Client) cancelPending(key string, err error, notifyServer bool, reason string) {
	c.mu.Lock()
	entry, ok := c.pending[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	c.removePendingLocked(key, entry)
	transport := c.transport
	c.mu.Unlock()
	entry.ch <- result{err: err}
	if notifyServer && transport != nil {
		params := map[string]any{"requestId": entry.id}
		if reason != "" {
			params["reason"] = reason
		}
		data, _ := marshalNoEscape(params)
		go func() {
			if sendErr := transport.Send(context.Background(), Message{JSONRPC: "2.0", Method: "notifications/cancelled", Params: data}); sendErr != nil {
				c.emitError(sendErr)
			}
		}()
	}
}

func (c *Client) removePendingLocked(key string, entry *pendingRequest) {
	delete(c.pending, key)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.progressKey != "" {
		delete(c.progress, entry.progressKey)
	}
	entry.cancelSend()
}

// markClosed is idempotent: it fails in-flight requests, cancels server
// requests being served, and flips the state; close listeners run once.
func (c *Client) markClosed(err error) {
	c.mu.Lock()
	wasClosed := c.state == StateClosed
	c.state = StateClosed
	pending := make([]*pendingRequest, 0, len(c.pending))
	for key, entry := range c.pending {
		c.removePendingLocked(key, entry)
		pending = append(pending, entry)
	}
	incoming := make([]context.CancelFunc, 0, len(c.incoming))
	for _, cancel := range c.incoming {
		incoming = append(incoming, cancel)
	}
	c.incoming = map[string]context.CancelFunc{}
	var listeners []func()
	if !wasClosed {
		for _, l := range c.onClose {
			listeners = append(listeners, l)
		}
	}
	c.mu.Unlock()
	for _, entry := range pending {
		entry.ch <- result{err: err}
	}
	for _, cancel := range incoming {
		cancel()
	}
	for _, l := range listeners {
		l()
	}
}

func (c *Client) emitError(err error) {
	c.mu.Lock()
	listeners := make([]func(error), 0, len(c.onError))
	for _, l := range c.onError {
		listeners = append(listeners, l)
	}
	c.mu.Unlock()
	for _, l := range listeners {
		l(err)
	}
}

func invalid(message string) error { return &Error{Code: CodeInvalidRequest, Message: message} }

func validateInitializeResult(raw json.RawMessage) (*InitializeResult, error) {
	var fields rawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil ||
		jsonType(fields["protocolVersion"]) != "string" ||
		jsonType(fields["capabilities"]) != "object" ||
		jsonType(fields["serverInfo"]) != "object" {
		return nil, invalid("Invalid MCP initialize result")
	}
	var info rawMessage
	if json.Unmarshal(fields["serverInfo"], &info) != nil ||
		jsonType(info["name"]) != "string" || jsonType(info["version"]) != "string" {
		return nil, invalid("Invalid MCP initialize result")
	}
	if t := jsonType(fields["instructions"]); t != "undefined" && t != "string" {
		return nil, invalid("Invalid MCP initialize result")
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, invalid("Invalid MCP initialize result")
	}
	return &res, nil
}

// validateListPage is one page of a paginated list: the items under key, each
// checked by isItem. Some servers end pagination with null or "" instead of
// omitting the cursor.
func validateListPage(method, key string, raw json.RawMessage, isItem func(rawMessage) bool) ([]json.RawMessage, string, error) {
	var fields rawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || jsonType(fields[key]) != "array" {
		return nil, "", invalid("Invalid MCP " + method + " result")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(fields[key], &items); err != nil {
		return nil, "", invalid("Invalid MCP " + method + " result")
	}
	for _, item := range items {
		var itemFields rawMessage
		if json.Unmarshal(item, &itemFields) != nil || itemFields == nil || !isItem(itemFields) {
			return nil, "", invalid("Invalid entry in MCP " + method + " result")
		}
	}
	next := ""
	switch jsonType(fields["nextCursor"]) {
	case "undefined", "null":
	case "string":
		_ = json.Unmarshal(fields["nextCursor"], &next)
	default:
		return nil, "", invalid("Invalid MCP " + method + " cursor")
	}
	return items, next, nil
}

func isTool(f rawMessage) bool {
	return jsonType(f["name"]) == "string" && jsonType(f["inputSchema"]) == "object"
}

// isResource: name is required by the spec, but some servers omit it; the URI
// stands in.
func isResource(f rawMessage) bool {
	t := jsonType(f["name"])
	return jsonType(f["uri"]) == "string" && (t == "undefined" || t == "string")
}

func isResourceTemplate(f rawMessage) bool {
	t := jsonType(f["name"])
	return jsonType(f["uriTemplate"]) == "string" && (t == "undefined" || t == "string")
}

func toResources(items []json.RawMessage) ([]Resource, error) {
	out := make([]Resource, 0, len(items))
	for _, item := range items {
		var r Resource
		if err := json.Unmarshal(item, &r); err != nil {
			return nil, invalid("Invalid entry in MCP resources/list result")
		}
		if r.Name == "" {
			var f rawMessage
			_ = json.Unmarshal(item, &f)
			if _, has := f["name"]; !has {
				r.Name = r.URI
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func toResourceTemplates(items []json.RawMessage) ([]ResourceTemplate, error) {
	out := make([]ResourceTemplate, 0, len(items))
	for _, item := range items {
		var r ResourceTemplate
		if err := json.Unmarshal(item, &r); err != nil {
			return nil, invalid("Invalid entry in MCP resources/templates/list result")
		}
		if r.Name == "" {
			var f rawMessage
			_ = json.Unmarshal(item, &f)
			if _, has := f["name"]; !has {
				r.Name = r.URITemplate
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func validateReadResourceResult(raw json.RawMessage) (*ReadResourceResult, error) {
	var fields rawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || jsonType(fields["contents"]) != "array" {
		return nil, invalid("Invalid MCP resources/read result")
	}
	var contents []rawMessage
	if json.Unmarshal(fields["contents"], &contents) != nil {
		return nil, invalid("Invalid MCP resources/read result")
	}
	for _, item := range contents {
		if item == nil || jsonType(item["uri"]) != "string" ||
			jsonType(item["text"]) != "string" && jsonType(item["blob"]) != "string" {
			return nil, invalid("Invalid contents in MCP resources/read result")
		}
	}
	var res ReadResourceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, invalid("Invalid MCP resources/read result")
	}
	return &res, nil
}

// validateCallToolResult: content is required by the spec, but servers that
// only return structuredContent omit it (the SDK defaults it too).
// structuredContent may be any JSON value, as MCP 2026-07-28 allows.
func validateCallToolResult(raw json.RawMessage) (*CallToolResult, error) {
	var fields rawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "Invalid MCP tools/call result"}
	}
	if t := jsonType(fields["content"]); t != "undefined" && t != "array" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "Invalid MCP tools/call result"}
	}
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "Invalid MCP tools/call result"}
	}
	res.StructuredContent = fields["structuredContent"]
	if res.Content == nil {
		res.Content = []ContentBlock{}
	}
	return &res, nil
}
