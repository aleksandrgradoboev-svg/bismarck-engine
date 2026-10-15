package client

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// TestNewUnixTransportFactoryRejectsInvalidOptions locks the validation that
// happens once, at construction, so a misconfigured path cannot fail later at
// reconnect time.
func TestNewUnixTransportFactoryRejectsInvalidOptions(t *testing.T) {
	zero := 0
	negative := -1
	for _, tc := range []struct {
		name string
		opts UnixTransportOptions
		want string
	}{
		{"empty path", UnixTransportOptions{Path: ""}, "must not be empty"},
		{"over-long path", UnixTransportOptions{Path: "/tmp/" + strings.Repeat("x", 512)}, "too long"},
		{"zero pending bytes", UnixTransportOptions{Path: "/tmp/pi.sock", MaxPendingBytes: &zero}, "positive"},
		{"negative pending bytes", UnixTransportOptions{Path: "/tmp/pi.sock", MaxPendingBytes: &negative}, "positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewUnixTransportFactory(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestUnixTransportBoundsPendingBytesAndPreservesOrder uses a synchronous pipe
// so the bound is observed deterministically: with nothing draining the far
// end, queued bytes stay pending and the budget must be enforced rather than
// growing without limit.
func TestUnixTransportBoundsPendingBytesAndPreservesOrder(t *testing.T) {
	local, remote := net.Pipe()
	var mu sync.Mutex
	var inbound []byte
	closes, failures := 0, 0
	closed := make(chan struct{})
	transport := &unixTransport{
		conn:            local,
		maxPendingBytes: 8,
		handlers: TransportHandlers{
			OnData: func(chunk []byte) {
				mu.Lock()
				defer mu.Unlock()
				inbound = append(inbound, chunk...)
			},
			OnClose: func() {
				mu.Lock()
				closes++
				mu.Unlock()
				close(closed)
			},
			OnError: func(error) {
				mu.Lock()
				defer mu.Unlock()
				failures++
			},
		},
	}
	transport.cond = sync.NewCond(&transport.mu)
	go transport.readLoop()
	go transport.writeLoop()

	if err := transport.Send([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := transport.Send([]byte{5, 6, 7, 8}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	err := transport.Send([]byte{9})
	if err == nil || !strings.Contains(err.Error(), "pending byte limit") {
		t.Fatalf("over-budget Send = %v, want a pending-byte-limit refusal", err)
	}

	written := make([]byte, 8)
	if _, err := readFull(remote, written); err != nil {
		t.Fatalf("reading what was written: %v", err)
	}
	if !bytes.Equal(written, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("written = %v, want the chunks in invocation order", written)
	}

	// The remote end going away is one terminal event, reported once.
	if _, err := remote.Write([]byte{9}); err != nil {
		t.Fatalf("remote write: %v", err)
	}
	_ = remote.Close()
	select {
	case <-closed:
	case <-time.After(testTimeout):
		t.Fatal("the remote close was never reported")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if closes != 1 {
		t.Errorf("OnClose calls = %d, want 1", closes)
	}
	if failures != 0 {
		t.Errorf("OnError calls = %d, want 0", failures)
	}
	if !bytes.Equal(inbound, []byte{9}) {
		t.Errorf("inbound = %v, want [9]", inbound)
	}
	_ = transport.Close()
}

// TestUnixTransportCloseSuppressesTerminalHandlers: a close this side asked for
// is not news, so no terminal handler fires for it.
func TestUnixTransportCloseSuppressesTerminalHandlers(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	var mu sync.Mutex
	terminals := 0
	count := func() {
		mu.Lock()
		defer mu.Unlock()
		terminals++
	}
	transport := &unixTransport{
		conn:            local,
		maxPendingBytes: 64,
		handlers: TransportHandlers{
			OnData:  func([]byte) {},
			OnClose: count,
			OnError: func(error) { count() },
		},
	}
	transport.cond = sync.NewCond(&transport.mu)
	go transport.readLoop()
	go transport.writeLoop()

	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := transport.Send([]byte{1}); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Errorf("Send after Close = %v, want a closed-transport refusal", err)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if terminals != 0 {
		t.Errorf("terminal handler calls = %d, want 0 for a local close", terminals)
	}
}

func skipWithoutUnixSockets(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain sockets are not supported on Windows")
	}
}

// unixTestServer listens on a temporary socket and hands each connection to
// handle.
func unixTestServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "pi-client-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "pi.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			go handle(conn)
		}
	}()
	return path
}

// TestClientOverRealUnixSocket exercises the whole stack against a real kernel
// socket, with the server deliberately fragmenting its frames.
func TestClientOverRealUnixSocket(t *testing.T) {
	skipWithoutUnixSockets(t)
	snapshot := &protocol.ServerSnapshot{
		ServerID:        "unix-server",
		ProtocolVersion: protocol.ProtocolVersion,
		Revision:        4,
		Sessions:        []protocol.SessionMetadata{},
		Models:          []protocol.ModelMetadata{},
	}
	path := unixTestServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		decoder, err := protocol.NewClientMessageDecoder(nil)
		if err != nil {
			return
		}
		buffer := make([]byte, 4096)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				messages, pushErr := decoder.Push(buffer[:n])
				if pushErr != nil {
					return
				}
				for _, message := range messages {
					switch typed := message.(type) {
					case *protocol.ClientHello:
						// One byte at a time: reassembly must not depend on
						// the peer's chunking.
						frame, _ := protocol.EncodeServerMessage(&protocol.ServerHello{
							Type:         "hello",
							Version:      protocol.ProtocolVersion,
							ConnectionID: "unix-connection",
							Snapshot:     *snapshot,
						}, nil)
						for _, b := range frame {
							if _, err := conn.Write([]byte{b}); err != nil {
								return
							}
						}
					case *protocol.RequestEnvelope:
						frame, _ := protocol.EncodeServerMessage(&protocol.ResponseEnvelope{
							Type:   "response",
							ID:     typed.ID,
							OK:     true,
							Result: &protocol.ListResult{Command: "list", Sessions: []protocol.SessionMetadata{}},
						}, nil)
						split := len(frame) / 2
						if _, err := conn.Write(frame[:split]); err != nil {
							return
						}
						if _, err := conn.Write(frame[split:]); err != nil {
							return
						}
					}
				}
			}
			if err != nil {
				return
			}
		}
	})

	factory, err := NewUnixTransportFactory(UnixTransportOptions{Path: path})
	if err != nil {
		t.Fatalf("NewUnixTransportFactory: %v", err)
	}
	client, err := New(Options{TransportFactory: factory})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := client.Connect(testContext(t))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got.ServerID != "unix-server" || got.Revision != 4 {
		t.Errorf("handshake snapshot = %#v, want the server's", got)
	}

	// Two concurrent requests must both be correlated correctly.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions, err := client.ListSessions(testContext(t))
			if err == nil && len(sessions) != 0 {
				err = errors.New("expected an empty session list")
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("ListSessions[%d]: %v", i, err)
		}
	}
}

// TestClientRejectsTruncatedFrameFromUnixSocket: a server that ends mid-frame
// is a protocol failure, not a clean shutdown.
func TestClientRejectsTruncatedFrameFromUnixSocket(t *testing.T) {
	skipWithoutUnixSockets(t)
	snapshot := &protocol.ServerSnapshot{
		ServerID:        "unix-truncated",
		ProtocolVersion: protocol.ProtocolVersion,
		Revision:        1,
		Sessions:        []protocol.SessionMetadata{},
		Models:          []protocol.ModelMetadata{},
	}
	path := unixTestServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		decoder, err := protocol.NewClientMessageDecoder(nil)
		if err != nil {
			return
		}
		buffer := make([]byte, 4096)
		for {
			n, readErr := conn.Read(buffer)
			if n > 0 {
				messages, pushErr := decoder.Push(buffer[:n])
				if pushErr != nil {
					return
				}
				for _, message := range messages {
					if _, ok := message.(*protocol.ClientHello); ok {
						frame, _ := protocol.EncodeServerMessage(&protocol.ServerHello{
							Type:         "hello",
							Version:      protocol.ProtocolVersion,
							ConnectionID: "unix-truncated",
							Snapshot:     *snapshot,
						}, nil)
						if _, err := conn.Write(frame); err != nil {
							return
						}
						continue
					}
					// A length prefix promising two bytes, followed by one.
					_, _ = conn.Write([]byte{0, 0, 0, 2, 1})
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	})

	factory, err := NewUnixTransportFactory(UnixTransportOptions{Path: path})
	if err != nil {
		t.Fatalf("NewUnixTransportFactory: %v", err)
	}
	client, err := New(Options{TransportFactory: factory})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_, err = client.ListSessions(testContext(t))
	if err == nil {
		t.Fatal("ListSessions succeeded against a truncated stream")
	}
	// The decoder wraps every framing failure as a ValidationError, so that is
	// the only thing this can be; accepting a FrameError too would hide a
	// regression in which error the decoder produced.
	var validation *protocol.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %#v, want *protocol.ValidationError", err)
	}
	waitFor(t, func() bool { return client.ConnectionState() == Disconnected })
}

// TestUnixTransportFactoryReportsDialFailures: a socket that is not there has
// to surface as the dial error, not as a silent no-op transport.
func TestUnixTransportFactoryReportsDialFailures(t *testing.T) {
	skipWithoutUnixSockets(t)
	directory, err := os.MkdirTemp("", "pi-client-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	factory, err := NewUnixTransportFactory(UnixTransportOptions{
		Path: filepath.Join(directory, "missing.sock"),
	})
	if err != nil {
		t.Fatalf("NewUnixTransportFactory: %v", err)
	}

	_, err = factory(testContext(t), TransportHandlers{
		OnData:  func([]byte) {},
		OnClose: func() {},
		OnError: func(error) {},
	})
	if err == nil {
		t.Fatal("connecting to a missing socket succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error = %v, want a missing-socket report", err)
	}
}

// TestUnixTransportFactoryHonoursContext: the dial is the part of a connection
// attempt that can block longest — connect(2) on a socket whose accept backlog
// is full waits for the peer, not for us — so a caller that has given up must
// not be held by it.
func TestUnixTransportFactoryHonoursContext(t *testing.T) {
	skipWithoutUnixSockets(t)
	// A live listener: the dial would otherwise fail on its own and prove
	// nothing about the context.
	path := unixTestServer(t, func(conn net.Conn) { <-make(chan struct{}) })
	factory, err := NewUnixTransportFactory(UnixTransportOptions{Path: path})
	if err != nil {
		t.Fatalf("NewUnixTransportFactory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport, err := factory(ctx, TransportHandlers{
		OnData:  func([]byte) {},
		OnClose: func() {},
		OnError: func(error) {},
	})
	if err == nil {
		_ = transport.Close()
		t.Fatal("the factory dialled despite a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %#v, want it to carry context.Canceled", err)
	}
}

func readFull(conn net.Conn, buffer []byte) (int, error) {
	total := 0
	for total < len(buffer) {
		n, err := conn.Read(buffer[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// waitFor polls a condition that is settled by another goroutine.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was never met")
}
