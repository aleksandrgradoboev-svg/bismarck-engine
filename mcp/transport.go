package mcp

import (
	"context"
	"sync"
)

// DefaultMaxMessageBytes caps one incoming message.
const DefaultMaxMessageBytes = 16 * 1024 * 1024

// Transport carries JSON-RPC messages between the client and one server (pi
// McpTransport).
//
// Send's context plays the role of pi's McpTransportSendOptions.signal: it is
// cancelled once the sender no longer waits for a reply (the request was
// cancelled, timed out, answered, or the client closed). A transport must not
// dispatch the message after that, for example when retrying it after an
// authorization refresh. It does not undo a message already dispatched.
type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, msg Message) error
	Close() error
	// OnMessage, OnError and OnClose register a listener and return a function
	// that removes it.
	OnMessage(func(Message)) func()
	OnError(func(error)) func()
	OnClose(func()) func()
}

// ProtocolVersionSetter is implemented by transports that need the negotiated
// protocol version (Streamable HTTP sends it as a header).
type ProtocolVersionSetter interface {
	SetProtocolVersion(version string)
}

// transportEvents is listener bookkeeping shared by transports (pi
// TransportEvents). emitClose fires at most once per transport.
type transportEvents struct {
	mu         sync.Mutex
	nextID     int
	onMessage  map[int]func(Message)
	onError    map[int]func(error)
	onClose    map[int]func()
	closeFired bool
}

func (e *transportEvents) OnMessage(listener func(Message)) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.onMessage == nil {
		e.onMessage = map[int]func(Message){}
	}
	id := e.nextID
	e.nextID++
	e.onMessage[id] = listener
	return func() { e.mu.Lock(); delete(e.onMessage, id); e.mu.Unlock() }
}

func (e *transportEvents) OnError(listener func(error)) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.onError == nil {
		e.onError = map[int]func(error){}
	}
	id := e.nextID
	e.nextID++
	e.onError[id] = listener
	return func() { e.mu.Lock(); delete(e.onError, id); e.mu.Unlock() }
}

func (e *transportEvents) OnClose(listener func()) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.onClose == nil {
		e.onClose = map[int]func(){}
	}
	id := e.nextID
	e.nextID++
	e.onClose[id] = listener
	return func() { e.mu.Lock(); delete(e.onClose, id); e.mu.Unlock() }
}

func (e *transportEvents) emitMessage(msg Message) {
	e.mu.Lock()
	listeners := make([]func(Message), 0, len(e.onMessage))
	for _, l := range e.onMessage {
		listeners = append(listeners, l)
	}
	e.mu.Unlock()
	for _, l := range listeners {
		l(msg)
	}
}

func (e *transportEvents) emitError(err error) {
	e.mu.Lock()
	listeners := make([]func(error), 0, len(e.onError))
	for _, l := range e.onError {
		listeners = append(listeners, l)
	}
	e.mu.Unlock()
	for _, l := range listeners {
		l(err)
	}
}

func (e *transportEvents) emitClose() {
	e.mu.Lock()
	if e.closeFired {
		e.mu.Unlock()
		return
	}
	e.closeFired = true
	listeners := make([]func(), 0, len(e.onClose))
	for _, l := range e.onClose {
		listeners = append(listeners, l)
	}
	e.mu.Unlock()
	for _, l := range listeners {
		l()
	}
}
