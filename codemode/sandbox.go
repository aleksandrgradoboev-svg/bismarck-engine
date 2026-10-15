package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout is the overall deadline of an execution, tools included.
const DefaultTimeout = 300 * time.Second

var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

var reservedGlobals = map[string]bool{
	"tools": true, "ALL_TOOLS": true, "console": true, "text": true, "image": true,
	"exit": true, "globalThis": true, "store": true, "load": true,
}

// Options configures a Sandbox.
type Options struct {
	Tools []Tool
	// Globals are exposed as top-level functions instead of on `tools`, for
	// host helpers. They behave like tools but are not recorded in
	// Result.Calls. Names must be identifiers or `namespace.member` and may
	// not shadow the built-in globals.
	Globals []Tool
	// Timeout is the default deadline per execution, tools included: 0 means
	// DefaultTimeout, a negative value none.
	Timeout time.Duration
	// MemoryLimitBytes caps each VM's heap; allocations beyond it fail in the
	// script as `InternalError: out of memory`. 0 means no limit beyond
	// wasm32's 4 GiB.
	MemoryLimitBytes uint32
}

// ExecuteOptions configures one Execute.
type ExecuteOptions struct {
	// Timeout overrides the sandbox default: 0 keeps it, negative means none.
	Timeout time.Duration
	// Store holds the values the script reads with load(key); they must be
	// JSON-serializable. The script's store() calls come back as
	// Result.StoreWrites; persisting them is up to the caller.
	Store map[string]any
}

// Sandbox holds the tool table and defaults; each Execute gets its own VM.
// It is safe for concurrent use.
type Sandbox struct {
	mu       sync.Mutex
	tools    []Tool
	globals  []Tool
	timeout  time.Duration
	memLimit uint32
	running  map[*execution]struct{}
	closed   bool
}

// New creates a sandbox. It fails on duplicate tool names and invalid globals.
func New(opts Options) (*Sandbox, error) {
	s := &Sandbox{
		timeout:  opts.Timeout,
		memLimit: opts.MemoryLimitBytes,
		running:  map[*execution]struct{}{},
	}
	if s.timeout == 0 {
		s.timeout = DefaultTimeout
	}
	for _, tool := range opts.Tools {
		if err := s.RegisterTool(tool); err != nil {
			return nil, err
		}
	}
	declared := make([]DeclaredGlobal, len(opts.Globals))
	for i, g := range opts.Globals {
		declared[i] = DeclaredGlobal{Name: g.Name, Spread: g.Spread}
	}
	if err := checkGlobals(declared); err != nil {
		return nil, err
	}
	s.globals = append([]Tool(nil), opts.Globals...)
	return s, nil
}

// checkGlobals: identifiers or `namespace.member`, not shadowing a built-in
// global or each other.
func checkGlobals(globals []DeclaredGlobal) error {
	names := map[string]bool{}
	namespaces := []string{}
	for _, g := range globals {
		parts := strings.Split(g.Name, ".")
		valid := len(parts) <= 2 && !reservedGlobals[parts[0]]
		for _, part := range parts {
			valid = valid && identifierRE.MatchString(part)
		}
		if !valid {
			return fmt.Errorf("Invalid global name %q", g.Name)
		}
		if names[g.Name] {
			return fmt.Errorf("Global %q is already registered", g.Name)
		}
		if len(parts) == 2 {
			namespaces = append(namespaces, parts[0])
		}
		names[g.Name] = true
	}
	for _, ns := range namespaces {
		if names[ns] {
			return fmt.Errorf("Global %q conflicts with the namespace %q", ns, ns)
		}
	}
	return nil
}

// RegisterTool adds a tool; it fails if one with the same name exists.
func (s *Sandbox) RegisterTool(tool Tool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tools {
		if t.Name == tool.Name {
			return fmt.Errorf("Tool %q is already registered", tool.Name)
		}
	}
	s.tools = append(s.tools, tool)
	return nil
}

// UnregisterTool removes a tool and reports whether it existed.
func (s *Sandbox) UnregisterTool(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tools {
		if t.Name == name {
			s.tools = append(s.tools[:i:i], s.tools[i+1:]...)
			return true
		}
	}
	return false
}

// Tools returns the registered tools in registration order.
func (s *Sandbox) Tools() []Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Tool(nil), s.tools...)
}

// Globals returns the configured globals.
func (s *Sandbox) Globals() []Tool { return append([]Tool(nil), s.globals...) }

// ErrClosed is returned by Execute and Start after Close.
var ErrClosed = errors.New("Sandbox is closed")

// register tracks a new execution before it starts, so Close sees it even if
// it finishes at once; it fails after Close.
func (s *Sandbox) register(e *execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.running[e] = struct{}{}
	return nil
}

func (s *Sandbox) release(e *execution) {
	s.mu.Lock()
	delete(s.running, e)
	s.mu.Unlock()
}

// Execute runs code, an async function body, and returns when it settles.
// Script failures come back as a Result with OK false, not as an error; the
// error is for a closed sandbox or a store that does not serialize. Canceling
// ctx aborts the execution, with ctx's cause as the message.
func (s *Sandbox) Execute(ctx context.Context, code string, opts ExecuteOptions) (Result, error) {
	store, err := encodeStore(opts.Store)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	tools := make(map[string]Tool, len(s.tools))
	declared := make([]vmTool, len(s.tools))
	for i, t := range s.tools {
		tools[t.Name] = t
		declared[i] = vmTool{Name: t.Name, JSName: Identifier(t.Name), Description: t.Description}
	}
	s.mu.Unlock()
	globals := make(map[string]Tool, len(s.globals))
	declaredGlobals := make([]vmGlobal, len(s.globals))
	for i, g := range s.globals {
		globals[g.Name] = g
		declaredGlobals[i] = vmGlobal{Name: g.Name, Spread: g.Spread}
	}
	timeout := s.timeout
	if opts.Timeout != 0 {
		timeout = opts.Timeout
	}
	e, err := newExecution(executionOptions{
		data: vmStartData{
			code:        code,
			tools:       declared,
			globals:     declaredGlobals,
			memoryLimit: s.memLimit,
			store:       store,
		},
		tools:    tools,
		globals:  globals,
		timeout:  timeout,
		ctx:      ctx,
		register: s.register,
		release:  s.release,
	})
	if err != nil {
		return Result{}, err
	}
	return <-e.result, nil
}

// HostCall is a call the script made in a Start run, answered with Run.Deliver.
type HostCall struct {
	// ID is the VM's own number for the call, 1, 2, ... in the order the
	// script made its calls, tools and globals together.
	ID     int
	Target string // "tool" or "global"
	Name   string
	// Args is what the script passed, as JSON; for spread globals the
	// argument list. nil for undefined.
	Args json.RawMessage
}

// DeclaredTool is a tool as a Start run's script sees it.
type DeclaredTool struct {
	Name string
	// JSName is the identifier the script uses, `tools.<JSName>`; empty means
	// Identifier(Name). A host replaying a run passes the one the run started
	// with, so a changed mapping cannot rename a tool under a resumed script.
	JSName      string
	Description string
}

// DeclaredGlobal is a global as a Start run's script sees it.
type DeclaredGlobal struct {
	Name   string
	Spread bool
}

// Outcome answers a call. A rejection (OK false) becomes an Error with Message
// in the script, carrying the properties of Properties (JSON values; name,
// message and stack are ignored).
type Outcome struct {
	OK         bool
	Value      any
	Message    string
	Properties map[string]any
}

// RunOptions configures Start.
type RunOptions struct {
	// StartedAt is the VM clock's start in ms since the epoch: Date.now()
	// until the first result arrives.
	StartedAt float64
	// Seed seeds Math.random() and the VM's other randomness.
	Seed    uint32
	Tools   []DeclaredTool
	Globals []DeclaredGlobal
	Store   map[string]any
	// Timeout is this run's deadline; 0 or negative means none (not the
	// sandbox's default).
	Timeout time.Duration
	// OnStarted receives the VM's fingerprint before the script runs.
	OnStarted func(fingerprint string)
	// OnCall receives each call the script makes; answer it with Run.Deliver,
	// now or later. Callbacks run on the VM's goroutine, one at a time.
	OnCall func(HostCall)
	// OnIdle: the VM processed result seq (0: the script's first run) and
	// waits again, having issued `issued` calls in total.
	OnIdle func(seq, issued int)
}

// Run is a running Start execution.
type Run struct {
	e      *execution
	once   sync.Once
	result Result
}

// Wait blocks until the run has its result.
func (r *Run) Wait() Result {
	r.once.Do(func() { r.result = <-r.e.result })
	return r.result
}

// Finished reports whether the run has its outcome. It is set when that is
// decided, before Wait returns (which waits for the VM to stop); calls can no
// longer be answered then.
func (r *Run) Finished() bool { return r.e.isFinished() }

// Deliver answers call id as result number seq. Results are numbered 1, 2, ...
// without gaps, and the VM applies them strictly in that order, whatever order
// they arrive in. While it processes this one, its clock reads now (ms since
// the epoch), or the previous value if now is earlier. It fails for a call
// that is not waiting, a seq used before, a now that is not finite, or an
// outcome that does not serialize (the call then stays waiting).
func (r *Run) Deliver(seq int, now float64, id int, outcome Outcome) error {
	return r.e.deliver(seq, now, id, outcome)
}

// Start runs code deterministically, with the caller answering its calls: the
// same script, start time, seed, tools, globals and store, answered with the
// same results in the same seq order, makes the same calls with the same
// arguments and produces the same output, on any VM with the same fingerprint,
// so a host can replay it after a restart. The VM runs in UTC, its clock reads
// StartedAt and then the now of the result being processed, its randomness
// comes from Seed, and WeakRef and FinalizationRegistry do not exist.
//
// The sandbox's own tools and globals are not used: the run declares its own,
// and every call goes to OnCall. Canceling ctx ends the run with an error the
// script cannot catch.
func (s *Sandbox) Start(ctx context.Context, code string, opts RunOptions) (*Run, error) {
	if !isFinite(opts.StartedAt) || opts.StartedAt < 0 {
		return nil, errors.New("startedAt must be a non-negative number of milliseconds since the epoch")
	}
	if opts.OnCall == nil {
		return nil, errors.New("OnCall is required")
	}
	names := map[string]bool{}
	jsNames := map[string]bool{}
	declared := make([]vmTool, len(opts.Tools))
	for i, t := range opts.Tools {
		jsName := t.JSName
		if jsName == "" {
			jsName = Identifier(t.Name)
		}
		if names[t.Name] {
			return nil, errors.New("Tool names must be unique")
		}
		if jsNames[jsName] {
			return nil, errors.New("Tool script names must be unique")
		}
		names[t.Name], jsNames[jsName] = true, true
		declared[i] = vmTool{Name: t.Name, JSName: jsName, Description: t.Description}
	}
	if err := checkGlobals(opts.Globals); err != nil {
		return nil, err
	}
	declaredGlobals := make([]vmGlobal, len(opts.Globals))
	for i, g := range opts.Globals {
		declaredGlobals[i] = vmGlobal{Name: g.Name, Spread: g.Spread}
	}
	store, err := encodeStore(opts.Store)
	if err != nil {
		return nil, err
	}
	e, err := newExecution(executionOptions{
		data: vmStartData{
			code:          code,
			tools:         declared,
			globals:       declaredGlobals,
			memoryLimit:   s.memLimit,
			store:         store,
			deterministic: &deterministicStart{startedAt: opts.StartedAt, seed: opts.Seed},
		},
		timeout:  opts.Timeout,
		ctx:      ctx,
		control:  &control{onStarted: opts.OnStarted, onCall: opts.OnCall, onIdle: opts.OnIdle},
		register: s.register,
		release:  s.release,
	})
	if err != nil {
		return nil, err
	}
	return &Run{e: e}, nil
}

// Close aborts in-flight executions (they end with ErrorAborted, "Sandbox
// closed") and rejects new ones. It waits until they have stopped.
func (s *Sandbox) Close() {
	s.mu.Lock()
	s.closed = true
	running := make([]*execution, 0, len(s.running))
	for e := range s.running {
		running = append(running, e)
	}
	s.mu.Unlock()
	for _, e := range running {
		e.abort("Sandbox closed")
	}
	for _, e := range running {
		<-e.released
	}
}
