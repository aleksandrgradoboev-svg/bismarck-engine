package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxErrorBodyBytes            = 8 * 1024
	errorMessageBodyChars        = 500
	defaultReconnectInitialDelay = time.Second
	defaultReconnectMaxDelay     = 30 * time.Second
	defaultReconnectMaxRetries   = 5
)

// HTTPDoer sends HTTP requests; *http.Client satisfies it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// UnauthorizedContext is handed to an AuthProvider after a 401, or a 403
// whose challenge reports insufficient_scope.
type UnauthorizedContext struct {
	Response  *http.Response
	ServerURL *url.URL
	Client    HTTPDoer
	// Token is the access token the rejected request carried, if any. A
	// different current token means another request already refreshed it.
	Token string
}

// AuthProvider supplies bearer tokens to the Streamable HTTP transport (pi
// AuthProvider). Token may refresh over the network first, so the transport
// calls it only before requests whose answer it waits for; closing a session
// reuses the token of the last request.
type AuthProvider interface {
	Token(ctx context.Context) (string, error)
}

// UnauthorizedHandler is an AuthProvider that can recover from a 401; the
// transport retries the request once afterwards with whatever credentials it
// left behind.
type UnauthorizedHandler interface {
	OnUnauthorized(ctx context.Context, uc UnauthorizedContext) error
}

// ReconnectOptions configure reconnection of dropped SSE streams: the GET
// stream, and response streams that carry event ids.
type ReconnectOptions struct {
	// InitialDelay is the delay before the first attempt unless the server sent
	// a retry field. Zero means 1s.
	InitialDelay time.Duration
	// MaxDelay bounds the exponential backoff. Zero means 30s.
	MaxDelay time.Duration
	// MaxRetries is the number of consecutive failed attempts before giving up
	// on a stream. Zero means 5.
	MaxRetries int
}

// StreamableHTTPTransportOptions configure a StreamableHTTPTransport.
type StreamableHTTPTransportOptions struct {
	URL     string
	Headers map[string]string
	// Client sends the requests; nil means http.DefaultClient.
	Client HTTPDoer
	// NoGetStream skips the server-to-client GET stream that otherwise opens
	// after initialization.
	NoGetStream     bool
	MaxMessageBytes int
	AuthProvider    AuthProvider
	Reconnect       ReconnectOptions
}

// HTTPError is a failed MCP HTTP request (pi McpHttpError).
type HTTPError struct {
	Status int
	Msg    string
	Body   string
}

func (e *HTTPError) Error() string { return e.Msg }

// AuthRequiredError is a 401 (pi McpAuthRequiredError).
type AuthRequiredError struct {
	HTTPError
	WWWAuthenticate string
}

// SessionExpiredError is a 404 for a session the server no longer knows (pi
// McpSessionExpiredError).
type SessionExpiredError struct{ HTTPError }

// StreamableHTTPTransport speaks MCP's Streamable HTTP transport (pi
// StreamableHttpTransport).
type StreamableHTTPTransport struct {
	transportEvents
	opts      StreamableHTTPTransportOptions
	serverURL *url.URL
	client    HTTPDoer
	ctx       context.Context
	cancel    context.CancelFunc

	mu               sync.Mutex
	started          bool
	closed           bool
	sessionID        string
	protocolVersion  string
	getStreamStarted bool
	lastToken        string
}

// NewStreamableHTTPTransport returns a transport for opts.URL.
func NewStreamableHTTPTransport(opts StreamableHTTPTransportOptions) (*StreamableHTTPTransport, error) {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, err
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithCancel(context.Background())
	headers := make(map[string]string, len(opts.Headers))
	for k, v := range opts.Headers {
		headers[k] = v
	}
	opts.Headers = headers
	return &StreamableHTTPTransport{opts: opts, serverURL: u, client: client, ctx: ctx, cancel: cancel}, nil
}

// SessionID is the session the server assigned, if any.
func (t *StreamableHTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Start implements Transport.
func (t *StreamableHTTPTransport) Start(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("MCP Streamable HTTP transport already started")
	}
	if t.closed {
		return &ConnectionClosedError{}
	}
	t.started = true
	return nil
}

// SetProtocolVersion implements ProtocolVersionSetter.
func (t *StreamableHTTPTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.mu.Unlock()
}

func (t *StreamableHTTPTransport) isOpen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started && !t.closed
}

func (t *StreamableHTTPTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Send implements Transport. ctx only stops dispatch (see Transport); the
// request itself lives as long as the transport.
func (t *StreamableHTTPTransport) Send(ctx context.Context, msg Message) error {
	if !t.isOpen() {
		return &ConnectionClosedError{}
	}
	body, err := marshalNoEscape(msg)
	if err != nil {
		return err
	}
	resp, err := t.authorizedFetch(ctx, http.MethodPost, map[string]string{
		"Accept": "application/json, text/event-stream", "Content-Type": "application/json",
	}, body)
	if err != nil {
		return err
	}
	if err := t.checkResponse(resp); err != nil {
		return err
	}
	t.captureSession(resp)

	if !msg.IsRequest() {
		// Notifications and responses are acknowledged with 202 and carry no
		// reply; ignore any body.
		discard(resp)
		// The server-to-client stream may only open once the session is
		// initialized.
		if msg.Method == "notifications/initialized" {
			t.startGetStream()
		}
		return nil
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		discard(resp)
		return &HTTPError{Status: resp.StatusCode, Msg: fmt.Sprintf("MCP server accepted request %s without a response", msg.Method)}
	}
	switch contentType(resp) {
	case "application/json":
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(data)
		var items []json.RawMessage
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json.Unmarshal(trimmed, &items); err != nil {
				return err
			}
		} else {
			items = []json.RawMessage{trimmed}
		}
		for _, item := range items {
			parsed, err := ParseMessageValue(item)
			if err != nil {
				return err
			}
			t.emitMessage(parsed)
		}
		return nil
	case "text/event-stream":
		go t.consumeResponseStream(resp.Body, msg.ID)
		return nil
	}
	discard(resp)
	ct := contentType(resp)
	if ct == "" {
		ct = "missing"
	}
	return &HTTPError{Status: resp.StatusCode, Msg: "Unsupported MCP response content type: " + ct}
}

// Close implements Transport. A session the server assigned is deleted, best
// effort: the auth provider is not asked (it may refresh over the network), so
// closing never waits for a refresh.
func (t *StreamableHTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	started, sessionID, token := t.started, t.sessionID, t.lastToken
	t.mu.Unlock()
	t.cancel()
	if started && sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.serverURL.String(), nil)
		if err == nil {
			t.applyHeaders(req, nil, token)
			if resp, err := t.client.Do(req); err == nil {
				discard(resp)
			}
		}
		cancel()
	}
	t.emitClose()
	return nil
}

// authorizedFetch sends a request with auth headers. A 401 (or a 403 asking
// for more scope) is handed to the auth provider once, and the request is
// retried with whatever credentials it left behind. sendCtx only stops
// dispatch: a request its sender gave up on while a token was fetched must not
// reach the server.
func (t *StreamableHTTPTransport) authorizedFetch(sendCtx context.Context, method string, headers map[string]string, body []byte) (*http.Response, error) {
	handler, _ := t.opts.AuthProvider.(UnauthorizedHandler)
	for attempt := 0; ; attempt++ {
		token, err := t.token()
		if err != nil {
			return nil, err
		}
		if sendCtx != nil && sendCtx.Err() != nil {
			return nil, &AbortError{}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.ctx, method, t.serverURL.String(), reader)
		if err != nil {
			return nil, err
		}
		t.applyHeaders(req, headers, token)
		resp, err := t.client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt > 0 || handler == nil || !needsAuthorization(resp) {
			return resp, nil
		}
		uerr := handler.OnUnauthorized(t.ctx, UnauthorizedContext{Response: resp, ServerURL: t.serverURL, Client: t.client, Token: token})
		discard(resp)
		if uerr != nil {
			return nil, uerr
		}
	}
}

func (t *StreamableHTTPTransport) token() (string, error) {
	var token string
	if t.opts.AuthProvider != nil {
		var err error
		if token, err = t.opts.AuthProvider.Token(t.ctx); err != nil {
			return "", err
		}
	}
	t.mu.Lock()
	t.lastToken = token
	t.mu.Unlock()
	return token, nil
}

func (t *StreamableHTTPTransport) applyHeaders(req *http.Request, extra map[string]string, token string) {
	for k, v := range t.opts.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sessionID, version := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (t *StreamableHTTPTransport) captureSession(resp *http.Response) {
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
}

func (t *StreamableHTTPTransport) checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	resp.Body.Close()
	body := string(data)
	if resp.StatusCode == http.StatusUnauthorized {
		return &AuthRequiredError{
			HTTPError:       HTTPError{Status: 401, Msg: "MCP server requires authentication", Body: body},
			WWWAuthenticate: resp.Header.Get("WWW-Authenticate"),
		}
	}
	if resp.StatusCode == http.StatusNotFound && t.SessionID() != "" {
		return &SessionExpiredError{HTTPError{Status: 404, Msg: "MCP session expired", Body: body}}
	}
	return &HTTPError{Status: resp.StatusCode, Msg: describeHTTPFailure(resp.StatusCode, body), Body: body}
}

type streamCursor struct {
	lastEventID string
	hasEventID  bool
	retry       time.Duration
	hasRetry    bool
	// received reports whether the stream delivered any event since it was
	// (re)opened.
	received bool
}

func (t *StreamableHTTPTransport) maxMessageBytes() int {
	if t.opts.MaxMessageBytes > 0 {
		return t.opts.MaxMessageBytes
	}
	return DefaultMaxMessageBytes
}

func (t *StreamableHTTPTransport) consumeSSE(body io.ReadCloser, cursor *streamCursor, onMessage func(Message)) error {
	defer body.Close()
	return ConsumeSSE(body, SSEOptions{
		MaxEventBytes: t.maxMessageBytes(),
		OnID:          func(id string) { cursor.lastEventID, cursor.hasEventID = id, true },
		OnRetry:       func(d time.Duration) { cursor.retry, cursor.hasRetry = d, true },
		OnEvent: func(ev SSEEvent) {
			cursor.received = true
			// Events without data prime resumption; other event types are not
			// JSON-RPC.
			if strings.TrimSpace(ev.Data) == "" || ev.Event != "" && ev.Event != "message" {
				return
			}
			msg, err := ParseMessage([]byte(ev.Data))
			if err != nil {
				t.emitError(err)
				return
			}
			if onMessage != nil {
				onMessage(msg)
			}
			t.emitMessage(msg)
		},
	})
}

// consumeResponseStream reads the SSE stream answering one request. When the
// stream ends or breaks before the response arrives and the server assigned
// event ids, it resumes with GET and Last-Event-ID, as the server may close
// response streams at will. Otherwise only this request fails.
func (t *StreamableHTTPTransport) consumeResponseStream(body io.ReadCloser, requestID json.RawMessage) {
	cursor := &streamCursor{}
	requestKey := IDKey(requestID)
	answered := false
	onMessage := func(msg Message) {
		if msg.IsResponse() && IDKey(msg.ID) == requestKey {
			answered = true
		}
	}
	stream := body
	var failure error
	for attempt := 0; ; {
		if stream != nil {
			failure = t.consumeSSE(stream, cursor, onMessage)
		}
		if answered || t.isClosed() {
			return
		}
		if failure != nil && !isRetryable(failure) {
			break
		}
		if !cursor.hasEventID || attempt >= t.maxRetries() {
			break
		}
		if cursor.received {
			attempt = 0
		}
		cursor.received = false
		if !t.sleep(t.reconnectDelay(attempt, cursor)) {
			return
		}
		attempt++
		var err error
		stream, err = t.openSSEStream(cursor)
		if err != nil {
			failure = err
			if !isRetryable(err) {
				break
			}
			stream = nil
		}
	}
	if t.isClosed() {
		return
	}
	reason := "stream ended without a response"
	if failure != nil {
		reason = failure.Error()
	}
	t.emitMessage(Message{JSONRPC: "2.0", ID: requestID, Error: &ErrorObject{
		Code: CodeInternalError, Message: "MCP response stream failed: " + reason,
	}})
}

func (t *StreamableHTTPTransport) startGetStream() {
	t.mu.Lock()
	if t.opts.NoGetStream || t.getStreamStarted || t.closed {
		t.mu.Unlock()
		return
	}
	t.getStreamStarted = true
	t.mu.Unlock()
	go t.runGetStream()
}

// runGetStream keeps the server-to-client stream open, reconnecting with
// backoff when it drops.
func (t *StreamableHTTPTransport) runGetStream() {
	cursor := &streamCursor{}
	for attempt := 0; !t.isClosed(); {
		stream, err := t.openSSEStream(cursor)
		if err == nil && stream == nil {
			return // the server does not offer a GET stream
		}
		if err == nil {
			opened := time.Now()
			err = t.consumeSSE(stream, cursor, nil)
			// A stream that stayed up for a while counts as healthy, even if idle.
			if cursor.received || time.Since(opened) > t.maxDelay() {
				attempt = 0
			}
		}
		if err != nil {
			if t.isClosed() {
				return
			}
			if !isRetryable(err) {
				t.emitError(err)
				return
			}
		}
		cursor.received = false
		if attempt >= t.maxRetries() {
			t.emitError(errors.New("MCP server-to-client stream dropped and could not be reopened"))
			return
		}
		if !t.sleep(t.reconnectDelay(attempt, cursor)) {
			return
		}
		attempt++
	}
}

// openSSEStream opens a GET SSE stream; it returns nil, nil when the server
// answers 405 (no GET stream).
func (t *StreamableHTTPTransport) openSSEStream(cursor *streamCursor) (io.ReadCloser, error) {
	headers := map[string]string{"Accept": "text/event-stream"}
	if cursor.hasEventID {
		headers["Last-Event-ID"] = cursor.lastEventID
	}
	resp, err := t.authorizedFetch(nil, http.MethodGet, headers, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		discard(resp)
		return nil, nil
	}
	if err := t.checkResponse(resp); err != nil {
		return nil, err
	}
	t.captureSession(resp)
	if contentType(resp) != "text/event-stream" {
		discard(resp)
		ct := contentType(resp)
		if ct == "" {
			ct = "missing"
		}
		return nil, &HTTPError{Status: resp.StatusCode, Msg: "Unsupported MCP GET response content type: " + ct}
	}
	return resp.Body, nil
}

func (t *StreamableHTTPTransport) reconnectDelay(attempt int, cursor *streamCursor) time.Duration {
	if cursor.hasRetry {
		return cursor.retry
	}
	initial := t.opts.Reconnect.InitialDelay
	if initial <= 0 {
		initial = defaultReconnectInitialDelay
	}
	delay := initial << attempt
	if delay <= 0 || delay > t.maxDelay() {
		return t.maxDelay()
	}
	return delay
}

func (t *StreamableHTTPTransport) maxDelay() time.Duration {
	if t.opts.Reconnect.MaxDelay > 0 {
		return t.opts.Reconnect.MaxDelay
	}
	return defaultReconnectMaxDelay
}

func (t *StreamableHTTPTransport) maxRetries() int {
	if t.opts.Reconnect.MaxRetries > 0 {
		return t.opts.Reconnect.MaxRetries
	}
	return defaultReconnectMaxRetries
}

// sleep waits d and reports false when the transport closed meanwhile.
func (t *StreamableHTTPTransport) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-t.ctx.Done():
		return false
	}
}

var insufficientScope = regexp.MustCompile(`(?i)(?:^|[\s,])error="?insufficient_scope"?`)

// needsAuthorization: 401, or 403 with an insufficient_scope bearer challenge
// (step-up authorization).
func needsAuthorization(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	return resp.StatusCode == http.StatusForbidden && insufficientScope.MatchString(resp.Header.Get("WWW-Authenticate"))
}

// isRetryable: network failures and transient statuses are retried; auth,
// session and protocol errors are not.
func isRetryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return isTransientStatus(httpErr.Status)
	}
	var authErr *AuthRequiredError
	var expired *SessionExpiredError
	if errors.As(err, &authErr) || errors.As(err, &expired) {
		return false
	}
	var netErr net.Error
	var urlErr *url.Error
	return errors.As(err, &netErr) || errors.As(err, &urlErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func isTransientStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func contentType(resp *http.Response) string {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	}
	return strings.ToLower(mt)
}

func discard(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func describeHTTPFailure(status int, body string) string {
	text := strings.TrimSpace(body)
	if utf8.RuneCountInString(text) > errorMessageBodyChars {
		runes := []rune(text)
		text = string(runes[:errorMessageBodyChars-3]) + "..."
	}
	msg := "MCP HTTP request failed with status " + strconv.Itoa(status)
	if text != "" {
		msg += ": " + text
	}
	return msg
}

// SSEEvent is one server-sent event.
type SSEEvent struct {
	Event string
	Data  string
	ID    string
}

// SSEOptions configure ConsumeSSE.
type SSEOptions struct {
	MaxEventBytes int
	OnEvent       func(SSEEvent)
	// OnID is called for every id field, including events without data (for
	// example resumption priming events).
	OnID func(id string)
	// OnRetry is called for every valid retry field.
	OnRetry func(time.Duration)
}

var retryDigits = regexp.MustCompile(`^\d+$`)

// ConsumeSSE parses a server-sent event stream until it ends (pi
// consumeSseStream). An event or an unterminated line longer than
// MaxEventBytes is an error, so a stream cannot grow without bound.
func ConsumeSSE(r io.Reader, opts SSEOptions) error {
	maxBytes := opts.MaxEventBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	var eventName, eventID string
	var dataLines []string
	dataBytes := 0
	dispatch := func() {
		if len(dataLines) == 0 {
			eventName, eventID = "", ""
			return
		}
		if opts.OnEvent != nil {
			opts.OnEvent(SSEEvent{Event: eventName, Data: strings.Join(dataLines, "\n"), ID: eventID})
		}
		eventName, eventID, dataLines, dataBytes = "", "", nil, 0
	}
	processLine := func(line string) error {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			dispatch()
			return nil
		}
		if strings.HasPrefix(line, ":") {
			return nil
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch {
		case field == "data":
			dataBytes += len(value)
			if len(dataLines) > 0 {
				dataBytes++
			}
			if dataBytes > maxBytes {
				return fmt.Errorf("MCP SSE event exceeds %d bytes", maxBytes)
			}
			dataLines = append(dataLines, value)
		case field == "event":
			eventName = value
		case field == "id" && !strings.Contains(value, "\x00"):
			eventID = value
			if opts.OnID != nil {
				opts.OnID(value)
			}
		case field == "retry" && retryDigits.MatchString(value):
			if ms, err := strconv.ParseInt(value, 10, 64); err == nil && opts.OnRetry != nil {
				opts.OnRetry(time.Duration(ms) * time.Millisecond)
			}
		}
		return nil
	}

	reader := bufio.NewReaderSize(r, 64*1024)
	var pending []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		pending = append(pending, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(pending) > maxBytes {
				return fmt.Errorf("MCP SSE event exceeds %d bytes", maxBytes)
			}
			continue
		}
		if err == nil {
			line := string(pending[:len(pending)-1])
			pending = pending[:0]
			if perr := processLine(line); perr != nil {
				return perr
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(pending) > 0 {
				if perr := processLine(string(pending)); perr != nil {
					return perr
				}
			}
			dispatch()
			return nil
		}
		return err
	}
}
