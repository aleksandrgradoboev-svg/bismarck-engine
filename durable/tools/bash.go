package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// maxTimeoutSeconds is the largest timeout: INT32_MAX milliseconds.
const maxTimeoutSeconds = 2_147_483_647.0 / 1000

// ShellStructuredOutput is what a program that calls bash or powershell
// receives.
type ShellStructuredOutput struct {
	// Output is the combined stdout and stderr: the retained tail, as the
	// model sees it.
	Output string `json:"output"`
	// Truncated is whether earlier output was dropped.
	Truncated bool `json:"truncated"`
	// FullOutputPath is the file with the complete output, when it was
	// spilled.
	FullOutputPath string `json:"fullOutputPath,omitempty"`
	ExitCode       int    `json:"exitCode"`
}

func shellStructuredOutputSchema() *ai.Schema {
	return ai.Object(
		ai.Prop("output", ai.String("Combined stdout and stderr: the retained tail, as the model sees it")),
		ai.Prop("truncated", ai.Boolean("Whether earlier output was dropped")),
		ai.Opt("fullOutputPath", ai.String("File with the complete output, when it was spilled")),
		ai.Prop("exitCode", ai.Number("")),
	)
}

// BashExecution is a command about to run, which Prepare may change: the
// script, its working directory and environment.
type BashExecution struct {
	Command    string
	Cwd        string
	Env        map[string]string
	InheritEnv bool
}

// BashPrepare changes a command before it runs; an error fails the call.
type BashPrepare func(ctx context.Context, execution *BashExecution, api harness.ToolExecutionAPI) error

// BashToolOptions configure the bash tool.
type BashToolOptions struct {
	// CommandPrefix is lines run before each command.
	CommandPrefix string
	Prepare       BashPrepare
}

// PowerShellToolOptions configure the powershell tool.
type PowerShellToolOptions struct {
	// CommandPrefix is lines run before each command.
	CommandPrefix string
	Prepare       BashPrepare
	// Programs are the PowerShell programs to try in order; nil is pwsh,
	// then powershell. A program that cannot start is skipped.
	Programs []string
}

func timeoutArg(args map[string]any) (*float64, error) {
	timeout, ok := numberArg(args, "timeout")
	if !ok {
		return nil, nil
	}
	if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return nil, errors.New("Invalid timeout: must be a finite number of seconds")
	}
	if timeout > maxTimeoutSeconds {
		return nil, fmt.Errorf("Invalid timeout: maximum is %s seconds", jsNumber(maxTimeoutSeconds))
	}
	return &timeout, nil
}

// prepareExecution is the execution of one call: the command with its
// prefix, in the environment's working directory, then prepare.
func prepareExecution(ctx context.Context, command, prefix string, prepare BashPrepare, api harness.ToolExecutionAPI) (*BashExecution, error) {
	e, err := requireEnv(api)
	if err != nil {
		return nil, err
	}
	if prefix != "" {
		command = prefix + "\n" + command
	}
	execution := &BashExecution{Command: command, Cwd: e.Cwd(), Env: map[string]string{}, InheritEnv: true}
	if prepare != nil {
		if err := prepare(ctx, execution, api); err != nil {
			return nil, err
		}
	}
	return execution, nil
}

// runCommand runs each command form in turn until one starts, streaming
// output to api.Output within the retained window, and turns the result into
// the tool's result: a spill diagnostic, an error result for a nonzero exit,
// and an error for a failure such as a timeout. The structured output repeats
// the retained output with the exit code.
func runCommand(ctx context.Context, commands []env.Command, execution *BashExecution, timeout *float64, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
	e, err := requireEnv(api)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	inherit := execution.InheritEnv
	options := &env.ExecOptions{
		Cwd:        execution.Cwd,
		Env:        execution.Env,
		InheritEnv: &inherit,
		OnOutput: func(_ context.Context, text string, info env.OutputInfo) error {
			var skipped *output.Skip
			if s := info.Skipped; s != nil {
				skipped = &output.Skip{Bytes: int(s.Bytes), Newlines: int(s.Newlines), EndsWithNewline: s.EndsWithNewline}
			}
			return api.Output([]byte(text), skipped)
		},
		Spill: &env.SpillOptions{AfterBytes: DefaultMaxBytes, AfterLines: DefaultMaxLines},
		// An environment may then omit output outside the retained tail and
		// report the omission.
		Window: api.OutputWindow(),
	}
	if timeout != nil {
		options.Timeout = time.Duration(*timeout * float64(time.Second))
	}
	if len(commands) == 0 {
		return harness.ToolExecutionResult{}, errors.New("No command to run")
	}
	var result env.ExecResult
	for _, command := range commands {
		result, err = e.Exec(ctx, command, options)
		// A program that could not start produced no output; the next form
		// may.
		if err == nil || env.ExecCode(err) != env.ExecSpawnError {
			break
		}
	}
	spillPath := result.SpillPath
	var execErr *env.ExecutionError
	if err != nil && errors.As(err, &execErr) {
		spillPath = execErr.SpillPath
	}
	if spillPath != "" {
		if derr := api.Diagnostic(harness.ToolDiagnostic{Severity: "info", Code: "full_output", Message: "Full output: " + spillPath}); derr != nil {
			return harness.ToolExecutionResult{}, derr
		}
	}
	if err != nil {
		switch code := env.ExecCode(err); {
		case code == env.ExecAborted && ctx.Err() != nil:
			return harness.ToolExecutionResult{}, err
		case code == env.ExecTimeout:
			return harness.ToolExecutionResult{}, fmt.Errorf("Command timed out after %s seconds", timeoutText(timeout))
		case code == env.ExecAborted:
			return harness.ToolExecutionResult{}, errors.New("Command aborted")
		}
		return harness.ToolExecutionResult{}, err
	}
	text, truncated, err := api.RetainedOutput()
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	structured := ShellStructuredOutput{Output: text, Truncated: truncated, FullOutputPath: spillPath, ExitCode: result.ExitCode}
	if result.ExitCode == 0 {
		return harness.ToolExecutionResult{StructuredOutput: structured}, nil
	}
	return harness.ToolExecutionResult{
		StructuredOutput: structured,
		IsError:          true,
		Diagnostics: []harness.ToolDiagnostic{{
			Severity: "error", Code: "exit_code", Message: fmt.Sprintf("Command exited with code %d", result.ExitCode),
		}},
	}, nil
}

// timeoutText is the timeout as pi's template renders it, "undefined"
// without one.
func timeoutText(timeout *float64) string {
	if timeout == nil {
		return "undefined"
	}
	return jsNumber(*timeout)
}

func shellParameters(what string) *ai.Schema {
	return ai.Object(
		ai.Prop("command", ai.String(what+" command to execute")),
		ai.Opt("timeout", ai.Number("Timeout in seconds (optional, no default timeout)")),
	)
}

func shellDescription(what string) string {
	return fmt.Sprintf("Execute a %s command in the current working directory. Returns combined stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
		what, DefaultMaxLines, DefaultMaxBytes/1024)
}

// NewBashTool is the bash tool (pi createBashTool): a command through the
// environment's shell. Its output streams to api.Output, where the Harness
// keeps the tail within the default limits; the result's output is that
// retained output. The retained window goes to the environment, which may
// omit output outside it and report how much it omitted, so dropped counts
// stay exact. Output beyond the limits is spilled to a file whose path is
// reported as a diagnostic. A nonzero exit is an error result with the exit
// code; a timeout fails the call, which makes an error result that still
// carries the output and diagnostics.
func NewBashTool(options BashToolOptions) *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        "bash",
			Description: shellDescription("bash"),
			Parameters:  shellParameters("Bash"),
		},
		StructuredOutputSchema: shellStructuredOutputSchema(),
		OutputLimits:           &harness.OutputLimits{Retain: "tail"},
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			timeout, err := timeoutArg(args)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			command, _ := args["command"].(string)
			execution, err := prepareExecution(ctx, command, options.CommandPrefix, options.Prepare, api)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			return runCommand(ctx, []env.Command{env.Script(execution.Command)}, execution, timeout, api)
		},
	}
}

// utf8Output makes PowerShell write UTF-8 whatever the console's code page,
// as the coding agent's powershell tool does.
const utf8Output = "try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}"

var powerShellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

// NewPowerShellTool is the powershell tool (pi createPowerShellTool): like
// bash but through PowerShell instead of the environment's shell — pwsh, else
// Windows PowerShell, started directly with the command as an argument, so
// no other shell parses it.
func NewPowerShellTool(options PowerShellToolOptions) *harness.ToolRegistration {
	programs := options.Programs
	if programs == nil {
		programs = []string{"pwsh", "powershell"}
	}
	return &harness.ToolRegistration{
		Tool: ai.Tool{
			Name:        "powershell",
			Description: shellDescription("PowerShell"),
			Parameters:  shellParameters("PowerShell"),
		},
		StructuredOutputSchema: shellStructuredOutputSchema(),
		OutputLimits:           &harness.OutputLimits{Retain: "tail"},
		Execute: func(ctx context.Context, args map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			timeout, err := timeoutArg(args)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			command, _ := args["command"].(string)
			execution, err := prepareExecution(ctx, command, options.CommandPrefix, options.Prepare, api)
			if err != nil {
				return harness.ToolExecutionResult{}, err
			}
			script := utf8Output + "\n" + execution.Command
			commands := make([]env.Command, len(programs))
			for i, program := range programs {
				commands[i] = env.Argv(append(append([]string{program}, powerShellArgs...), script)...)
			}
			return runCommand(ctx, commands, execution, timeout, api)
		},
	}
}
