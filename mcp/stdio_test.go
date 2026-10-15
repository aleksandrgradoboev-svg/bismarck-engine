package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

// The test binary doubles as the stdio MCP servers of pi's fixtures
// (stdio-server.mjs, stubborn-server.mjs): TestMain runs a server instead of
// the tests when MCP_TEST_SERVER is set.
func TestMain(m *testing.M) {
	switch os.Getenv("MCP_TEST_SERVER") {
	case "":
		os.Exit(m.Run())
	case "echo":
		runEchoServer()
	case "stubborn":
		runStubbornServer()
	case "sleeper":
		signal.Ignore(syscall.SIGTERM)
		select {}
	}
	os.Exit(0)
}

func writeResult(w *bufio.Writer, id json.RawMessage, result any) {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	w.Write(append(data, '\n'))
	w.Flush()
}

func runEchoServer() {
	fmt.Fprintln(os.Stderr, "stdio fixture ready")
	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments struct{ Text any } `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		switch msg.Method {
		case "initialize":
			writeResult(out, msg.ID, map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "stdio-fixture", "version": "1.0.0"},
			})
		case "tools/list":
			writeResult(out, msg.ID, map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			writeResult(out, msg.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(msg.Params.Arguments.Text)}}})
		case "ping":
			writeResult(out, msg.ID, map[string]any{})
		default:
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "not found"}})
			out.Write(append(data, '\n'))
			out.Flush()
		}
	}
}

// runStubbornServer answers initialize, starts a grandchild that ignores
// SIGTERM, and itself ignores stdin EOF and SIGTERM.
func runStubbornServer() {
	signal.Ignore(syscall.SIGTERM)
	grandchild := exec.Command(os.Args[0])
	grandchild.Env = append(os.Environ(), "MCP_TEST_SERVER=sleeper")
	if err := grandchild.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "grandchild failed:", err)
	} else {
		fmt.Fprintf(os.Stderr, "grandchild %d\n", grandchild.Process.Pid)
	}
	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &msg) == nil && msg.Method == "initialize" {
			writeResult(out, msg.ID, map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "stubborn-fixture", "version": "1.0.0"},
			})
		}
	}
	select {}
}

func TestStdioTransportConnectsAndCapturesStderr(t *testing.T) {
	var stderr strings.Builder
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	transport := NewStdioTransport(StdioTransportOptions{
		Command:  os.Args[0],
		Env:      map[string]string{"MCP_TEST_SERVER": "echo"},
		OnStderr: func(chunk string) { <-mu; stderr.WriteString(chunk); mu <- struct{}{} },
	})
	c := NewClient(ClientOptions{Implementation: Implementation{Name: "stdio-test", Version: "1.0.0"}})
	ctx := context.Background()
	if _, err := c.Connect(ctx, transport); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(ctx, nil)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hello"}, nil)
	if err != nil || len(res.Content) != 1 || res.Content[0].Text != "hello" {
		t.Fatalf("call = %+v, %v", res, err)
	}
	if transport.PID() == 0 {
		t.Fatal("PID is 0 after start")
	}
	waitFor(t, "stderr", func() bool { return strings.Contains(transport.Stderr(), "stdio fixture ready") })
	<-mu
	got := stderr.String()
	mu <- struct{}{}
	if !strings.Contains(got, "stdio fixture ready") {
		t.Fatalf("OnStderr got %q", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if c.State() != StateClosed {
		t.Fatalf("state = %s", c.State())
	}
}
