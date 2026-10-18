package coding

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
)

// The exported constructors exist so a host can confine the tools; one that
// dropped its ops would read the real disk while looking confined.

func TestCreateReadToolUsesOps(t *testing.T) {
	cwd := t.TempDir()
	var asked string
	ops := ReadOperations{
		Access:   func(_ context.Context, p string) error { asked = p; return nil },
		ReadFile: func(context.Context, string) ([]byte, error) { return []byte("from host\n"), nil },
	}
	res, err := CreateReadTool(cwd, &ops).Execute(context.Background(), "1", map[string]any{"path": "a/b.txt"}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(resultText(res), "from host") {
		t.Fatalf("host ReadFile was not used: %q", resultText(res))
	}
	if want := filepath.Join(cwd, "a", "b.txt"); asked != want {
		t.Fatalf("Access got %q, want the resolved path %q", asked, want)
	}
}

func TestCreateReadToolRefusalStopsTheRead(t *testing.T) {
	refused := errors.New("outside the workspace")
	ops := ReadOperations{
		Access: func(context.Context, string) error { return refused },
		ReadFile: func(context.Context, string) ([]byte, error) {
			t.Fatal("ReadFile ran after Access refused")
			return nil, nil
		},
	}
	_, err := CreateReadTool(t.TempDir(), &ops).Execute(context.Background(), "1", map[string]any{"path": "../x"}, nil)
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the host's refusal", err)
	}
}

func TestCreateLsToolUsesOps(t *testing.T) {
	ops := LsOperations{
		Exists:  func(context.Context, string) (bool, error) { return true, nil },
		Stat:    func(_ context.Context, p string) (bool, error) { return !strings.HasSuffix(p, ".txt"), nil },
		Readdir: func(context.Context, string) ([]string, error) { return []string{"doc.txt", "sub"}, nil },
	}
	res, err := CreateLsTool(t.TempDir(), &ops).Execute(context.Background(), "1", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resultText(res); got != "doc.txt\nsub/" {
		t.Fatalf("listing = %q, want the host's entries", got)
	}
}

func TestCreateWriteAndEditToolsUseOps(t *testing.T) {
	cwd := t.TempDir()
	var wrote string
	write := WriteOperations{
		WriteFile: func(_ context.Context, p, _ string) error { wrote = p; return nil },
		Mkdir:     func(context.Context, string) error { return nil },
	}
	if _, err := CreateWriteTool(cwd, &write).Execute(context.Background(), "1", map[string]any{"path": "n.txt", "content": "x"}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if wrote != filepath.Join(cwd, "n.txt") {
		t.Fatalf("host WriteFile got %q", wrote)
	}
	var edited string
	edit := EditOperations{
		Access:    func(context.Context, string) error { return nil },
		ReadFile:  func(context.Context, string) ([]byte, error) { return []byte("old\n"), nil },
		WriteFile: func(_ context.Context, _, content string) error { edited = content; return nil },
	}
	args := map[string]any{"path": "e.txt", "edits": []any{map[string]any{"oldText": "old", "newText": "new"}}}
	if _, err := CreateEditTool(cwd, &edit).Execute(context.Background(), "1", args, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited != "new\n" {
		t.Fatalf("host WriteFile got %q", edited)
	}
}

func TestCreateShellToolsUseOps(t *testing.T) {
	cwd := t.TempDir()
	for name, create := range map[string]func(string, *BashOperations) agent.AgentTool{
		"bash": CreateBashTool, "powershell": CreatePowerShellTool,
	} {
		var gotCommand, gotCwd string
		ops := BashOperations{Exec: func(_ context.Context, command, dir string, o BashExecOptions) (*int, error) {
			gotCommand, gotCwd = command, dir
			o.OnData([]byte("from host\n"))
			code := 0
			return &code, nil
		}}
		tool := create(cwd, &ops)
		res, err := tool.Execute(context.Background(), "1", map[string]any{"command": "echo hi"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tool.Name != name || gotCwd != cwd || !strings.HasSuffix(gotCommand, "echo hi") || !strings.Contains(resultText(res), "from host") {
			t.Fatalf("%s: name %q, cwd %q, command %q, result %q", name, tool.Name, gotCwd, gotCommand, resultText(res))
		}
		if name == "powershell" && !strings.HasPrefix(gotCommand, utf8OutputPrefix) {
			t.Fatalf("powershell command lacks the UTF-8 prefix: %q", gotCommand)
		}
	}
}
