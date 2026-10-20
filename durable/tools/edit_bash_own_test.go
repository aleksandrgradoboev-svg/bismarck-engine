package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// Cases beyond pi's tests for edit, bash and the coding-tools extension: each
// covers a path no pi case reaches.

// TestGenerateDiffStringMatchesPi compares the display diff with pi's own
// generateDiffString, run by node on the texts of testdata/capture-edit-diff.mjs.
func TestGenerateDiffStringMatchesPi(t *testing.T) {
	b, err := os.ReadFile("testdata/edit-diff-d2e7d8b7.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Name             string `json:"name"`
			Old              string `json:"old"`
			New              string `json:"new"`
			Diff             string `json:"diff"`
			FirstChangedLine *int   `json:"firstChangedLine"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) < 500 {
		t.Fatalf("%d cases", len(golden.Cases))
	}
	for _, c := range golden.Cases {
		display, first := generateDiffString(c.Old, c.New, 4)
		if display != c.Diff || !reflect.DeepEqual(first, c.FirstChangedLine) {
			t.Errorf("%s:\n got  %q (first %v)\n want %q (first %v)", c.Name, display, ptrValue(first), c.Diff, ptrValue(c.FirstChangedLine))
		}
	}
}

func ptrValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestBashRejectsAZeroTimeoutAndOneAboveTheMaximum(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		timeout float64
		want    string
	}{
		{0, "Invalid timeout: must be a finite number of seconds"},
		{maxTimeoutSeconds + 0.001, "Invalid timeout: maximum is 2147483.647 seconds"},
	} {
		_, err := run(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "true", "timeout": c.timeout}, e)
		if err == nil || err.Error() != c.want {
			t.Errorf("timeout %v: err = %v, want %q", c.timeout, err, c.want)
		}
	}
	if _, err := run(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "true", "timeout": maxTimeoutSeconds}, e); err != nil {
		t.Fatalf("the maximum itself: %v", err)
	}
}

// abortingEnv fails every command as aborted, as an environment that stopped
// it on its own does.
type abortingEnv struct {
	*env.Local
	err *env.ExecutionError
}

func (e *abortingEnv) Exec(context.Context, env.Command, *env.ExecOptions) (env.ExecResult, error) {
	return env.ExecResult{}, e.err
}

func TestBashFailsWithTheEnvironmentsErrorWhenTheCallIsCancelledAndSaysAbortedOtherwise(t *testing.T) {
	e := &abortingEnv{Local: newEnv(t), err: &env.ExecutionError{Code: env.ExecAborted, Message: "killed: aborted"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := run(ctx, NewBashTool(BashToolOptions{}), map[string]any{"command": "x"}, e); !errors.Is(err, e.err) {
		t.Fatalf("cancelled call: err = %v, want the environment's", err)
	}
	if _, err := run(context.Background(), NewBashTool(BashToolOptions{}), map[string]any{"command": "x"}, e); err == nil || err.Error() != "Command aborted" {
		t.Fatalf("running call: err = %v", err)
	}
	// A real command cut short by the call's cancellation.
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := run(ctx, NewBashTool(BashToolOptions{}), map[string]any{"command": "sleep 5"}, newEnv(t))
	if env.ExecCode(err) != env.ExecAborted {
		t.Fatalf("cancelled sleep: err = %v", err)
	}
}

// truncatedAPI reports that earlier output was dropped.
type truncatedAPI struct {
	*fakeAPI
}

func (a *truncatedAPI) RetainedOutput() (string, bool, error) { return "tail", true, nil }

func TestBashStructuredOutputSaysWhenEarlierOutputWasDropped(t *testing.T) {
	api := &truncatedAPI{fakeAPI: &fakeAPI{env: newEnv(t)}}
	result, err := NewBashTool(BashToolOptions{}).Execute(context.Background(), map[string]any{"command": "true"}, api)
	if err != nil {
		t.Fatal(err)
	}
	if result.StructuredOutput != (ShellStructuredOutput{Output: "tail", Truncated: true}) {
		t.Fatalf("structured = %+v", result.StructuredOutput)
	}
}

func TestCodingToolsIsTheCodingToolsExtensionWithReadWriteEditAndBash(t *testing.T) {
	extension := NewCodingTools(ReadToolOptions{})
	var names []string
	for _, tool := range extension.Tools {
		names = append(names, tool.Name)
	}
	if extension.Name != "coding-tools" || !reflect.DeepEqual(names, []string{"read", "write", "edit", "bash"}) {
		t.Fatalf("extension %q with %v", extension.Name, names)
	}
	if NewCodingTools(ReadToolOptions{}) == extension {
		t.Fatal("extensions are shared")
	}
}
