//go:build unix

package unix

import (
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/server"
)

// stubListener replays a scripted sequence of accept outcomes, which is the
// only practical way to reach the failures a real listener produces once the
// process is out of descriptors.
type stubListener struct {
	mu       sync.Mutex
	outcomes []func() (net.Conn, error)
	accepts  int
}

func (l *stubListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.accepts++
	if len(l.outcomes) == 0 {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	next := l.outcomes[0]
	l.outcomes = l.outcomes[1:]
	l.mu.Unlock()
	return next()
}

func (l *stubListener) Close() error   { return nil }
func (l *stubListener) Addr() net.Addr { return &net.UnixAddr{Name: "stub", Net: "unix"} }

func (l *stubListener) acceptCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// A transient accept failure must not retire the transport. Returning here
// leaves Address reporting a bound socket that answers nobody, for the life of
// the process — and every unserved peer is one more that never gets in.
func TestAcceptLoopSurvivesTransientFailures(t *testing.T) {
	t.Parallel()
	var reported []error
	var mu sync.Mutex
	l, err := NewListener(ListenerOptions{
		Path: "/tmp/pi-accept-loop.sock",
		OnError: func(err error) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new listener: %v", err)
	}

	served := make(chan struct{}, 1)
	l.accept = func(server.ByteConn) server.ConnHandler {
		served <- struct{}{}
		return server.ConnHandler{OnData: func([]byte) {}, OnClose: func() {}, OnError: func(error) {}}
	}
	l.stopAccept = make(chan struct{})

	peer, local := net.Pipe()
	t.Cleanup(func() { _ = peer.Close(); _ = local.Close() })
	netListener := &stubListener{outcomes: []func() (net.Conn, error){
		func() (net.Conn, error) { return nil, syscall.EMFILE },
		func() (net.Conn, error) { return nil, syscall.ECONNABORTED },
		func() (net.Conn, error) { return local, nil },
	}}

	done := make(chan struct{})
	go l.acceptLoop(netListener, done)

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatalf("no connection was accepted after a transient failure; accepts = %d", netListener.acceptCount())
	}
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 2 {
		t.Fatalf("reported = %v, want the two transient failures", reported)
	}
	for _, err := range reported {
		if !errors.Is(err, syscall.EMFILE) && !errors.Is(err, syscall.ECONNABORTED) {
			t.Fatalf("reported %v, want the accept failure wrapped", err)
		}
	}
}

// Send only queues, so a write that fails does so long after the caller that
// queued it has gone. pi rejects the send promise and the server reports it;
// the only place left to report it here is the listener's observer.
func TestWriteFailuresReachTheErrorObserver(t *testing.T) {
	t.Parallel()
	peer, local := net.Pipe()
	reported := make(chan error, 1)
	c := newConn(local, time.Second, 1024, func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	t.Cleanup(func() { _ = local.Close() })

	if err := peer.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	if err := c.Send([]byte("frame")); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "write failed") {
			t.Fatalf("reported %v, want the write failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write that failed after its send was queued never reached the error observer")
	}
}

// The loop stops for good once the listener is closed, and says nothing about
// it: a closed listener is not a failure.
func TestAcceptLoopStopsOnCloseWithoutReporting(t *testing.T) {
	t.Parallel()
	var reported []error
	var mu sync.Mutex
	l, err := NewListener(ListenerOptions{
		Path: "/tmp/pi-accept-loop-close.sock",
		OnError: func(err error) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new listener: %v", err)
	}
	l.stopAccept = make(chan struct{})

	done := make(chan struct{})
	go l.acceptLoop(&stubListener{}, done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the accept loop must end once the listener is closed")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 0 {
		t.Fatalf("reported = %v, want nothing for an orderly close", reported)
	}
}
