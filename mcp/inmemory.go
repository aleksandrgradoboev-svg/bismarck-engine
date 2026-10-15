package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// InMemoryTransport is one end of an in-process transport pair, for tests and
// for servers that live in the same process (pi InMemoryTransport). Delivery
// is asynchronous and in order, as pi's queueMicrotask delivery is.
type InMemoryTransport struct {
	transportEvents
	mu      sync.Mutex
	peer    *InMemoryTransport
	started bool
	closed  bool
	queue   chan Message
	done    chan struct{}
}

// NewInMemoryTransportPair returns two connected transports.
func NewInMemoryTransportPair() (client, server *InMemoryTransport) {
	client = newInMemoryTransport()
	server = newInMemoryTransport()
	client.peer = server
	server.peer = client
	return client, server
}

func newInMemoryTransport() *InMemoryTransport {
	t := &InMemoryTransport{queue: make(chan Message, 1024), done: make(chan struct{})}
	go t.deliverLoop()
	return t
}

func (t *InMemoryTransport) deliverLoop() {
	for {
		select {
		case msg := <-t.queue:
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if !closed {
				t.emitMessage(msg)
			}
		case <-t.done:
			return
		}
	}
}

// Start implements Transport.
func (t *InMemoryTransport) Start(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &ConnectionClosedError{}
	}
	t.started = true
	return nil
}

// Send implements Transport. The message is copied, as pi's structuredClone
// copies it, so neither side can mutate the other's.
func (t *InMemoryTransport) Send(_ context.Context, msg Message) error {
	t.mu.Lock()
	ok := t.started && !t.closed
	peer := t.peer
	t.mu.Unlock()
	if !ok {
		return &ConnectionClosedError{}
	}
	peer.mu.Lock()
	peerOK := peer.started && !peer.closed
	peer.mu.Unlock()
	if !peerOK {
		return &ConnectionClosedError{Msg: "In-memory MCP peer is not connected"}
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	var copied Message
	if err := json.Unmarshal(data, &copied); err != nil {
		return err
	}
	select {
	case peer.queue <- copied:
		return nil
	default:
		return errors.New("in-memory MCP transport queue is full")
	}
}

// Close implements Transport and closes the peer too.
func (t *InMemoryTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	peer := t.peer
	t.mu.Unlock()
	t.emitClose()
	if peer != nil {
		return peer.Close()
	}
	return nil
}

// EmitError lets tests simulate a transport-level failure.
func (t *InMemoryTransport) EmitError(err error) { t.emitError(err) }
