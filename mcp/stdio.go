package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxStderrBytes = 64 * 1024
	defaultCloseTimeout   = 2 * time.Second
	// stdinCloseGrace is how long a server gets to exit on its own after stdin
	// closes, before it is sent SIGTERM.
	stdinCloseGrace = 500 * time.Millisecond
)

// StdioTransportOptions configure a StdioTransport (pi StdioTransportOptions).
type StdioTransportOptions struct {
	Command string
	Args    []string
	Dir     string
	// Env is added to the inherited environment, or is the whole environment
	// when NoInheritEnv is set.
	Env          map[string]string
	NoInheritEnv bool
	// StderrInherit passes the server's stderr through instead of capturing it.
	StderrInherit bool
	// OnStderr receives captured stderr chunks.
	OnStderr        func(chunk string)
	MaxMessageBytes int
	MaxStderrBytes  int
	// CloseTimeout is how long to wait for the server to exit after SIGTERM
	// before SIGKILL. Zero means 2s.
	CloseTimeout time.Duration
}

// StdioTransport runs an MCP server as a child process and exchanges
// newline-delimited JSON-RPC over its stdin and stdout (pi StdioTransport).
type StdioTransport struct {
	transportEvents
	opts StdioTransportOptions

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	started bool
	closed  bool
	exited  chan struct{}
	stderr  []byte
}

// NewStdioTransport returns a transport that starts opts.Command on Start.
func NewStdioTransport(opts StdioTransportOptions) *StdioTransport {
	opts.Args = append([]string(nil), opts.Args...)
	return &StdioTransport{opts: opts, exited: make(chan struct{})}
}

// PID is the server's process id, or 0 before Start.
func (t *StdioTransport) PID() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

// Stderr is the captured tail of the server's stderr.
func (t *StdioTransport) Stderr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.stderr)
}

// Start implements Transport: it spawns the server.
func (t *StdioTransport) Start(context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("MCP stdio transport already started")
	}
	if t.closed {
		t.mu.Unlock()
		return &ConnectionClosedError{}
	}
	t.started = true
	t.mu.Unlock()

	cmd := exec.Command(t.opts.Command, t.opts.Args...)
	cmd.Dir = t.opts.Dir
	cmd.Env = t.environment()
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr io.ReadCloser
	if t.opts.StderrInherit {
		cmd.Stderr = os.Stderr
	} else if stderr, err = cmd.StderrPipe(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.mu.Lock()
	t.cmd = cmd
	t.stdin = stdin
	t.mu.Unlock()

	var readers sync.WaitGroup
	readers.Add(1)
	go func() { defer readers.Done(); t.readStdout(stdout) }()
	if stderr != nil {
		readers.Add(1)
		go func() { defer readers.Done(); t.readStderr(stderr) }()
	}
	go func() {
		readers.Wait()
		_ = cmd.Wait()
		close(t.exited)
		t.emitClose()
	}()
	return nil
}

func (t *StdioTransport) environment() []string {
	var env []string
	if !t.opts.NoInheritEnv {
		env = os.Environ()
	}
	for k, v := range t.opts.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// Send implements Transport: one JSON line on the server's stdin.
func (t *StdioTransport) Send(_ context.Context, msg Message) error {
	t.mu.Lock()
	stdin := t.stdin
	ok := t.started && !t.closed && stdin != nil
	t.mu.Unlock()
	if !ok {
		return &ConnectionClosedError{}
	}
	payload, err := marshalNoEscape(msg)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = stdin.Write(append(payload, '\n'))
	return err
}

// Close implements Transport. Shutdown follows the spec: close stdin and let
// the server exit, then SIGTERM, then SIGKILL; the server's process tree is
// terminated too, so wrappers such as npx or uvx do not leave it behind.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	cmd := t.cmd
	stdin := t.stdin
	t.mu.Unlock()
	if cmd == nil {
		t.emitClose()
		return nil
	}
	select {
	case <-t.exited:
		_ = stdin.Close()
		return nil
	default:
	}
	closeTimeout := t.opts.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}
	grace := min(stdinCloseGrace, closeTimeout)
	_ = stdin.Close()
	select {
	case <-t.exited:
	case <-time.After(grace):
		killProcessTree(cmd, false)
		select {
		case <-t.exited:
		case <-time.After(closeTimeout):
			killProcessTree(cmd, true)
			<-t.exited
		}
	}
	// Children of the server that ignored stdin closing would otherwise outlive it.
	killLeftovers(cmd)
	return nil
}

func (t *StdioTransport) readStdout(r io.Reader) {
	maxBytes := t.opts.MaxMessageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	var buf []byte
	chunk := make([]byte, 64*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				newline := bytes.IndexByte(buf, '\n')
				if newline < 0 {
					if len(buf) > maxBytes {
						buf = nil
						t.emitError(fmt.Errorf("MCP stdio message exceeds %d bytes", maxBytes))
					}
					break
				}
				line := buf[:newline]
				buf = buf[newline+1:]
				if len(line) > maxBytes {
					t.emitError(fmt.Errorf("MCP stdio message exceeds %d bytes", maxBytes))
					continue
				}
				text := trimCR(string(line))
				if strings.TrimSpace(text) == "" {
					continue
				}
				msg, perr := ParseMessage([]byte(text))
				if perr != nil {
					t.emitError(perr)
					continue
				}
				t.emitMessage(msg)
			}
		}
		if err != nil {
			if strings.TrimSpace(string(buf)) != "" {
				t.emitError(errors.New("MCP stdio server closed with an incomplete JSON-RPC message"))
			}
			return
		}
	}
}

func (t *StdioTransport) readStderr(r io.Reader) {
	maxBytes := t.opts.MaxStderrBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxStderrBytes
	}
	chunk := make([]byte, 8*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			data := append([]byte(nil), chunk[:n]...)
			t.mu.Lock()
			t.stderr = append(t.stderr, data...)
			if len(t.stderr) > maxBytes {
				t.stderr = t.stderr[len(t.stderr)-maxBytes:]
			}
			t.mu.Unlock()
			if t.opts.OnStderr != nil {
				t.opts.OnStderr(string(data))
			}
		}
		if err != nil {
			return
		}
	}
}
