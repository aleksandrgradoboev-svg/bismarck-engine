package codemode

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/search"
)

const (
	argsPreviewChars  = 200
	errorPreviewChars = 500
	// defaultSearchLimit is searchTools()'s default limit.
	defaultSearchLimit = 8
)

// discoveryGlobals find tools, answered from the start data, so a replay sees
// what the first run saw.
var discoveryGlobals = []string{"searchTools", "describeTool", "describeNamespace"}

// cut shortens text to max UTF-16 code units, ending in "...".
func cut(text string, max int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= max {
		return text
	}
	return string(utf16.Decode(units[:max-3])) + "..."
}

func errorResult(message string) harness.ToolExecutionResult {
	return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: message}}, IsError: true}
}

func toValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var value any
	err = json.Unmarshal(raw, &value)
	return value, err
}

func fromValue(value any, out any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func nowMs(api harness.ToolExecutionAPI) (float64, error) {
	now, err := api.Now()
	if err != nil {
		return 0, err
	}
	return float64(now.UnixNano()) / float64(time.Millisecond), nil
}

// sameJSON is structural JSON equality; key order does not matter, and nil
// (undefined) equals only nil.
func sameJSON(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// describeCall is a call as a divergence names it.
func describeCall(call sandbox.HostCall) string {
	prefix := ""
	if call.Target == "tool" {
		prefix = "tools."
	}
	return prefix + call.Name + "(" + string(call.Args) + ")"
}

type queued struct {
	id      int
	at      float64
	outcome sandbox.Outcome
	global  *journalGlobal
}

type delivery struct {
	seq, id int
	at      float64
	outcome sandbox.Outcome
}

// call is one execution of the code mode tool: a first run, or a rerun after
// a restart that replays the journal. VM callbacks arrive one at a time on
// the VM's goroutine; they change state under mu and leave commits, nested
// calls and deliveries to goroutines, which take mu only between them.
type call struct {
	options Options
	api     harness.ToolExecutionAPI
	ctx     context.Context
	cancel  context.CancelCauseFunc
	// gate closes once the VM's fingerprint is checked or recorded; nothing
	// is delivered or admitted before.
	gate     chan struct{}
	gateOnce sync.Once

	mu    sync.Mutex
	run   *sandbox.Run
	start startData
	// replayed is the journal as this attempt found it; replay delivers
	// exactly these.
	replayed []journalEntry
	// journalLength counts entries in the journal, replayed and live; the
	// next delivery is journalLength + 1.
	journalLength int
	// nextReplayed counts replayed entries delivered so far.
	nextReplayed int
	replaying    bool
	// rerun: an earlier attempt ran this call, so its nested call keys may
	// already exist.
	rerun bool
	// released: live calls may launch; before, they are held.
	released bool
	// firstIssued is the count an earlier attempt recorded after the
	// script's first run.
	firstIssued *int
	// durableIssued is the highest issued count known to be committed: calls
	// up to it may launch.
	durableIssued int
	// waiting are live calls waiting for their issued count to be committed.
	waiting []sandbox.HostCall
	// starting are covered calls not yet started; one launcher starts them in
	// call order, each once the one before it has started (as Pi starts a
	// covered batch synchronously).
	starting    []sandbox.HostCall
	launching   bool
	replayIndex map[int]int
	// issued counts the calls the VM issued so far; its call IDs are 1, 2, ...
	issued int
	// ready are replay results of journaled calls, ready to deliver in
	// journal order.
	ready map[int]sandbox.Outcome
	// held are replay calls the journal does not have, admitted once replay
	// is done.
	held []sandbox.HostCall
	// queue are finished live calls waiting for their journal commit.
	queue []queued
	// acks are acknowledged counts to record, by result number (0: the
	// script's first run).
	acks       map[int]int
	committing bool
	// ended is set once the call decided an outcome other than the script's
	// own (divergence, mismatch); failure once it failed.
	ended   *harness.ToolExecutionResult
	failure error
	// aborted: this call cancelled the VM, as opposed to the host closing
	// the sandbox.
	aborted bool
	// rows are the script's tool calls for details, by VM call ID.
	rows map[int]*NestedCall
	// outputDir is the run's output directory once known.
	outputDir string
}

func newCall(options Options, api harness.ToolExecutionAPI) *call {
	return &call{
		options:     options,
		api:         api,
		gate:        make(chan struct{}),
		replayIndex: map[int]int{},
		ready:       map[int]sandbox.Outcome{},
		acks:        map[int]int{},
		rows:        map[int]*NestedCall{},
	}
}

func (c *call) openGate() { c.gateOnce.Do(func() { close(c.gate) }) }

// stoppedLocked: nothing more may happen, as the run has its outcome or the
// call decided or failed.
func (c *call) stoppedLocked() bool {
	return c.ended != nil || c.failure != nil || (c.run != nil && c.run.Finished())
}

func (c *call) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stoppedLocked()
}

func (c *call) abort(cause error) {
	c.mu.Lock()
	c.aborted = true
	c.mu.Unlock()
	c.cancel(cause)
}

func (c *call) fail(err error) {
	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return
	}
	c.failure = err
	c.aborted = true
	c.mu.Unlock()
	c.cancel(err)
}

func (c *call) execute(parent context.Context, source string) (harness.ToolExecutionResult, error) {
	parsed, err := sandbox.ParseSource(source)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	api := c.api
	agent, err := api.Agent(parent)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	var tools []declaredTool
	var callable []*harness.ToolRegistration
	for _, tool := range agent.Callable {
		declared := declaredTool{Name: tool.Name, JSName: sandbox.Identifier(tool.Name), Description: sampleOf(tool),
			Structured: tool.StructuredOutputSchema != nil, Hidden: hasInternalPrefix(tool.Name)}
		tools = append(tools, declared)
		if !declared.Hidden {
			callable = append(callable, tool)
		}
	}
	globals := []startGlobal{}
	for _, name := range discoveryGlobals {
		globals = append(globals, startGlobal{Name: name, Spread: true})
	}
	for _, global := range c.options.Globals {
		globals = append(globals, startGlobal{Name: global.Name, Spread: global.Spread})
	}
	namespaces := []startNamespace{}
	for _, tool := range callable {
		namespace := namespaceOf(tool.Name, c.options.Namespaces)
		if namespace == nil {
			continue
		}
		index := slices.IndexFunc(namespaces, func(n startNamespace) bool { return n.Name == namespace.Name })
		if index == -1 {
			namespaces = append(namespaces, startNamespace{Name: namespace.Name, Description: namespace.Description, Instructions: namespace.Instructions, Tools: []string{}})
			index = len(namespaces) - 1
		}
		namespaces[index].Tools = append(namespaces[index].Tools, tool.Name)
	}
	now, err := nowMs(api)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	var seedBytes [4]byte
	if _, err := rand.Read(seedBytes[:]); err != nil {
		return harness.ToolExecutionResult{}, err
	}
	seed := binary.LittleEndian.Uint32(seedBytes[:])
	maxOutputTokens := DefaultMaxOutputTokens
	if parsed.Options.MaxOutputTokens != nil {
		maxOutputTokens = int(*parsed.Options.MaxOutputTokens)
	}

	var done *harness.ToolExecutionResult
	var journal struct {
		Entries     []journalEntry `json:"entries"`
		FirstIssued *int           `json:"firstIssued,omitempty"`
	}
	err = api.Commit(parent, func(tx *durable.Tx) error {
		run, err := tx.Doc(RunDoc.Task(api.TaskID()))
		if err != nil {
			return err
		}
		if run.Has("result") {
			stored, err := run.At("result").Snapshot()
			if err != nil {
				return err
			}
			result, err := restoreResult(stored)
			done = &result
			return err
		}
		_, started := run.Get("start")
		c.rerun = started
		if !started {
			store, err := tx.Doc(StoreDoc.Conversation(api.ConversationID()))
			if err != nil {
				return err
			}
			values, err := store.At("values").Snapshot()
			if err != nil {
				return err
			}
			copied, _ := values.(map[string]any)
			if copied == nil {
				copied = map[string]any{}
			}
			start := startData{Version: ModuleVersion, StartedAt: now, Seed: seed, Tools: tools, Namespaces: namespaces,
				Globals: globals, MaxOutputTokens: maxOutputTokens, Store: copied}
			if start.Tools == nil {
				start.Tools = []declaredTool{}
			}
			if parsed.Options.TimeoutMs != nil {
				deadline := now + float64(*parsed.Options.TimeoutMs)
				start.Deadline = &deadline
			}
			value, err := toValue(start)
			if err != nil {
				return err
			}
			if err := run.Set("start", value); err != nil {
				return err
			}
		}
		stored, err := run.At("start").Snapshot()
		if err != nil {
			return err
		}
		if err := fromValue(stored, &c.start); err != nil {
			return err
		}
		journalDraft, err := tx.Doc(JournalDoc.Task(api.TaskID()))
		if err != nil {
			return err
		}
		snapshot, err := journalDraft.Snapshot()
		if err != nil {
			return err
		}
		return fromValue(snapshot, &journal)
	})
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	if done != nil {
		return *done, nil
	}
	c.replayed = journal.Entries
	for i, entry := range journal.Entries {
		c.replayIndex[entry.ID] = i
	}
	c.journalLength = len(journal.Entries)
	c.replaying = len(journal.Entries) > 0
	c.firstIssued = journal.FirstIssued
	if journal.FirstIssued != nil {
		c.durableIssued = *journal.FirstIssued
	}
	for _, entry := range journal.Entries {
		if entry.IssuedAfter != nil {
			c.durableIssued = max(c.durableIssued, *entry.IssuedAfter)
		}
	}

	c.ctx, c.cancel = context.WithCancelCause(parent)
	defer c.cancel(nil)
	if c.start.Version != ModuleVersion {
		return c.decide(errorResult(fmt.Sprintf("The script cannot resume: it started under code mode version %d, this is %d.", c.start.Version, ModuleVersion)))
	}
	var timeout time.Duration
	if c.start.Deadline != nil {
		now, err := nowMs(api)
		if err != nil {
			return harness.ToolExecutionResult{}, err
		}
		remaining := *c.start.Deadline - now
		if remaining <= 0 {
			return c.decide(errorResult("Script timed out: its deadline passed while it was interrupted."))
		}
		timeout = time.Duration(remaining * float64(time.Millisecond))
	}

	var declared []sandbox.DeclaredTool
	for _, tool := range c.start.Tools {
		if !tool.Hidden {
			declared = append(declared, sandbox.DeclaredTool{Name: tool.Name, JSName: tool.JSName, Description: tool.Description})
		}
	}
	runGlobals := make([]sandbox.DeclaredGlobal, len(c.start.Globals))
	for i, global := range c.start.Globals {
		runGlobals[i] = sandbox.DeclaredGlobal{Name: global.Name, Spread: global.Spread}
	}
	c.mu.Lock()
	run, err := c.options.Sandbox.Start(c.ctx, parsed.Code, sandbox.RunOptions{
		StartedAt: c.start.StartedAt,
		Seed:      c.start.Seed,
		Tools:     declared,
		Globals:   runGlobals,
		Store:     c.start.Store,
		Timeout:   timeout,
		OnStarted: func(fingerprint string) { go c.checkFingerprint(fingerprint) },
		OnCall:    c.onCall,
		OnIdle:    c.onIdle,
	})
	c.run = run
	c.mu.Unlock()
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	result := run.Wait()

	c.mu.Lock()
	failure, ended, aborted := c.failure, c.ended, c.aborted
	c.mu.Unlock()
	if failure != nil {
		return harness.ToolExecutionResult{}, failure
	}
	if ended != nil {
		return *ended, nil
	}
	if err := parent.Err(); err != nil {
		return harness.ToolExecutionResult{}, context.Cause(parent)
	}
	// Aborted, but not by this call: the host closed the sandbox, for example
	// while shutting down before closing the Harness. That decides nothing
	// about the script: store nothing and end with this invocation, so a
	// later attempt replays it.
	if !result.OK && result.Error != nil && result.Error.Kind == sandbox.ErrorAborted && !aborted {
		<-parent.Done()
		return harness.ToolExecutionResult{}, context.Cause(parent)
	}
	// A replay that ends where the live run went on is a divergence.
	if during := result.During; c.rerun && during != nil && during.Seq <= len(c.replayed) {
		continued := false
		if during.Seq == 0 {
			continued = len(c.replayed) > 0 || c.firstIssued != nil
		} else {
			continued = c.replayed[during.Seq-1].IssuedAfter != nil
		}
		for _, entry := range c.replayed[during.Seq:] {
			if entry.ID > during.Issued {
				continued = true
			}
		}
		if continued {
			return c.decide(errorResult(fmt.Sprintf("The script took a different path when resumed after a restart: it ended after result %d, where it went on before.", during.Seq)))
		}
	}
	return c.complete(result)
}

func restoreResult(stored any) (harness.ToolExecutionResult, error) {
	var result storedResult
	if err := fromValue(stored, &result); err != nil {
		return harness.ToolExecutionResult{}, err
	}
	out := harness.ToolExecutionResult{Output: result.Output, IsError: result.IsError}
	if out.Output == nil {
		out.Output = ai.ContentList{}
	}
	if result.Details != nil {
		out.Details = result.Details
	}
	return out, nil
}

// storeResult writes the outcome into the run document unless one is there;
// it returns the stored one.
func storeResult(tx *durable.Tx, taskID durable.TaskID, result harness.ToolExecutionResult) (harness.ToolExecutionResult, bool, error) {
	run, err := tx.Doc(RunDoc.Task(taskID))
	if err != nil {
		return result, false, err
	}
	if run.Has("result") {
		stored, err := run.At("result").Snapshot()
		if err != nil {
			return result, false, err
		}
		existing, err := restoreResult(stored)
		return existing, false, err
	}
	details, err := json.Marshal(result.Details)
	if err != nil {
		return result, false, err
	}
	value, err := toValue(storedResult{Output: result.Output, IsError: result.IsError, Details: details})
	if err != nil {
		return result, false, err
	}
	return result, true, run.Set("result", value)
}

// decide stores an outcome the call decided and returns it.
func (c *call) decide(decided harness.ToolExecutionResult) (harness.ToolExecutionResult, error) {
	decided.Details = c.details()
	err := c.api.Commit(c.ctx, func(tx *durable.Tx) error {
		_, _, err := storeResult(tx, c.api.TaskID(), decided)
		return err
	})
	return decided, err
}

// diverge ends the call with message: it stores it as the outcome, then
// stops the VM with an error the script cannot catch.
func (c *call) diverge(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.divergeLocked(message)
}

// divergeLocked is diverge with mu held: the call stops at once, and the
// outcome is stored and the VM stopped on another goroutine.
func (c *call) divergeLocked(message string) {
	if c.ended != nil || c.failure != nil {
		return
	}
	result := errorResult("The script took a different path when resumed after a restart: " + message)
	c.ended = &result
	go func() {
		if _, err := c.decide(result); err != nil {
			c.fail(err)
			return
		}
		c.abort(errors.New(message))
	}()
}

// complete stores the script's own outcome with its store writes in one
// transaction.
func (c *call) complete(result sandbox.Result) (harness.ToolExecutionResult, error) {
	details := c.details()
	var calls []callSummary
	for _, row := range details.Calls {
		calls = append(calls, callSummary{name: row.Name, status: row.Status})
	}
	now, err := nowMs(c.api)
	if err != nil {
		return harness.ToolExecutionResult{}, err
	}
	content, fullOutputPath := formatResult(c.ctx, result, now-c.start.StartedAt, c.start.MaxOutputTokens, calls, toolOutputLimits, c.outputFiles())
	details.FullOutputPath = fullOutputPath
	tool := harness.ToolExecutionResult{Output: content, IsError: !result.OK, Details: details}
	var final harness.ToolExecutionResult
	err = c.api.Commit(c.ctx, func(tx *durable.Tx) error {
		stored, written, err := storeResult(tx, c.api.TaskID(), tool)
		final = stored
		if err != nil || !written || !result.OK {
			return err
		}
		writes := result.StoreWrites
		if len(writes.Set) == 0 && len(writes.Delete) == 0 {
			return nil
		}
		store, err := tx.Doc(StoreDoc.Conversation(c.api.ConversationID()))
		if err != nil {
			return err
		}
		values := store.At("values")
		for _, key := range writes.Delete {
			if values.Has(key) {
				if err := values.Delete(key); err != nil {
					return err
				}
			}
		}
		for _, key := range sortedKeys(writes.Set) {
			var value any
			if err := json.Unmarshal(writes.Set[key], &value); err != nil {
				return err
			}
			if err := values.Set(key, value); err != nil {
				return err
			}
		}
		return nil
	})
	return final, err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// outputFiles writes the result's files under fixed names into the run's
// output directory, so a rerun overwrites them.
func (c *call) outputFiles() outputFile {
	return func(ctx context.Context, name string, data []byte) (string, error) {
		e := c.api.Env()
		if e == nil {
			return "", errors.New("no execution environment")
		}
		dir, err := c.outputDirOf(ctx, e)
		if err != nil {
			return "", err
		}
		path, err := e.JoinPath(ctx, []string{dir, name})
		if err != nil {
			return "", err
		}
		if err := e.WriteFile(ctx, path, data); err != nil {
			return "", err
		}
		return path, nil
	}
}

// outputDirOf is the run's output directory: the recorded one, else a new
// temporary directory, recorded before any file is written.
func (c *call) outputDirOf(ctx context.Context, e env.ExecutionEnv) (string, error) {
	if c.outputDir != "" {
		return c.outputDir, nil
	}
	var recorded string
	err := c.api.Commit(ctx, func(tx *durable.Tx) error {
		run, err := tx.Doc(RunDoc.Task(c.api.TaskID()))
		if err != nil {
			return err
		}
		if dir, ok := run.Get("outputDir"); ok {
			recorded, _ = dir.(string)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if recorded == "" {
		created, err := e.CreateTempDir(ctx, "pi-codemode-")
		if err != nil {
			return "", err
		}
		err = c.api.Commit(ctx, func(tx *durable.Tx) error {
			run, err := tx.Doc(RunDoc.Task(c.api.TaskID()))
			if err != nil {
				return err
			}
			if dir, ok := run.Get("outputDir"); ok {
				recorded, _ = dir.(string)
				return nil
			}
			recorded = created
			return run.Set("outputDir", created)
		})
		if err != nil {
			return "", err
		}
	}
	c.outputDir = recorded
	return recorded, nil
}

func (c *call) checkFingerprint(fingerprint string) {
	recorded := c.start.Fingerprint
	if recorded != "" {
		if recorded != fingerprint {
			result := errorResult(fmt.Sprintf("The script cannot resume: the sandbox changed since it started (%s -> %s).", recorded, fingerprint))
			c.mu.Lock()
			c.ended = &result
			c.mu.Unlock()
			if _, err := c.decide(result); err != nil {
				c.fail(err)
			}
			c.abort(errors.New("Sandbox fingerprint changed"))
			return
		}
	} else {
		err := c.api.Commit(c.ctx, func(tx *durable.Tx) error {
			run, err := tx.Doc(RunDoc.Task(c.api.TaskID()))
			if err != nil {
				return err
			}
			if run.Has("start") {
				return run.At("start").Set("fingerprint", fingerprint)
			}
			return nil
		})
		if err != nil {
			c.fail(err)
			return
		}
	}
	c.openGate()
	// Nothing was delivered before the gate: start now. A rerun without a
	// journal releases its calls at the script's first acknowledgment
	// instead.
	c.mu.Lock()
	replaying, rerun := c.replaying, c.rerun
	c.mu.Unlock()
	if replaying {
		c.deliverReplayed()
	} else if !rerun {
		c.release()
	}
}

func (c *call) onCall(made sandbox.HostCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stoppedLocked() {
		return
	}
	if made.Target == "tool" {
		c.startedLocked(made)
	}
	c.issued = max(c.issued, made.ID)
	index := -1
	if c.replaying {
		if i, ok := c.replayIndex[made.ID]; ok {
			index = i
		}
	}
	if index == -1 {
		if c.released {
			c.waiting = append(c.waiting, made)
			c.launchCoveredLocked()
		} else {
			c.held = append(c.held, made)
		}
		return
	}
	entry := c.replayed[index]
	if entry.Global != nil {
		if made.Target != "global" || made.Name != entry.Global.Name || !sameJSON(made.Args, entry.Global.Args) {
			c.divergeLocked(fmt.Sprintf("call %d is %s, but was a call to %s", made.ID, describeCall(made), entry.Global.Name))
			return
		}
		c.ready[made.ID] = toOutcome(entry.Global.Outcome)
		go func() {
			<-c.gate
			c.deliverReplayed()
		}()
		return
	}
	if made.Target != "tool" {
		c.divergeLocked(fmt.Sprintf("call %d is %s, but was a tool call", made.ID, describeCall(made)))
		return
	}
	go func() {
		<-c.gate
		outcome, err := c.callTool(made, nil)
		if err != nil {
			if c.stopped() {
				return
			}
			if c.ctx.Err() != nil {
				c.fail(err)
			} else {
				// The key names another tool or other arguments.
				c.diverge(fmt.Sprintf("call %d is %s: %s", made.ID, describeCall(made), err.Error()))
			}
			return
		}
		c.mu.Lock()
		c.ready[made.ID] = outcome
		c.mu.Unlock()
		c.deliverReplayed()
	}()
}

func toOutcome(o journalOutcome) sandbox.Outcome {
	outcome := sandbox.Outcome{OK: o.OK, Message: o.Message, Properties: o.Properties}
	if o.Value != nil {
		outcome.Value = o.Value
	}
	return outcome
}

func fromOutcome(o sandbox.Outcome) (journalOutcome, error) {
	out := journalOutcome{OK: o.OK, Message: o.Message, Properties: o.Properties}
	if o.Value != nil {
		raw, err := json.Marshal(o.Value)
		if err != nil {
			return out, err
		}
		out.Value = raw
	}
	return out, nil
}

// deliverReplayed delivers journaled results in journal order, each once
// the VM has issued its call.
func (c *call) deliverReplayed() {
	c.mu.Lock()
	var deliveries []delivery
	for c.nextReplayed < len(c.replayed) && !c.stoppedLocked() {
		entry := c.replayed[c.nextReplayed]
		outcome, ok := c.ready[entry.ID]
		if entry.ID > c.issued || !ok {
			break
		}
		delete(c.ready, entry.ID)
		c.nextReplayed++
		deliveries = append(deliveries, delivery{seq: c.nextReplayed, id: entry.ID, at: entry.At, outcome: outcome})
	}
	run := c.run
	c.mu.Unlock()
	deliverAll(run, deliveries)
}

// deliverAll hands the VM a batch of results, last first. The VM applies
// results strictly in seq order, so it starts on the batch only once its
// first result arrives and then processes all of it before it waits again,
// as pi's VM does with results delivered in one turn: one acknowledgment,
// and one count commit, per batch rather than per result.
func deliverAll(run *sandbox.Run, deliveries []delivery) {
	for i := len(deliveries) - 1; i >= 0; i-- {
		d := deliveries[i]
		if run.Finished() {
			return
		}
		_ = run.Deliver(d.seq, d.at, d.id, d.outcome)
	}
}

func (c *call) onIdle(seq, issued int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stoppedLocked() {
		return
	}
	// A count an earlier attempt recorded must match; a missing one is
	// recorded now.
	if c.rerun && seq <= len(c.replayed) {
		recorded := c.firstIssued
		if seq > 0 {
			recorded = c.replayed[seq-1].IssuedAfter
		}
		if recorded != nil && *recorded != issued {
			message := fmt.Sprintf("after result %d it had made %d calls, where it had made %d", seq, issued, *recorded)
			if seq == 0 {
				message = fmt.Sprintf("its first run made %d calls, where it had made %d", issued, *recorded)
			}
			c.divergeLocked(message)
			return
		}
	}
	c.acks[seq] = issued
	if !c.replaying {
		// A rerun without a journal: every call an earlier attempt could have
		// admitted is made by now.
		if seq == 0 && c.rerun {
			go c.release()
		}
		// Committed at once when the script has processed everything
		// delivered: calls it made wait for it, and a script waiting on a
		// long call is where a restart usually finds it.
		if seq == c.journalLength {
			go c.pump()
		}
		return
	}
	// Caught up: the VM processed everything delivered and waits.
	if seq != c.nextReplayed {
		return
	}
	if seq < len(c.replayed) {
		if next := c.replayed[seq]; next.ID > issued {
			c.divergeLocked(fmt.Sprintf("it waits without making call %d, which it made before", next.ID))
		}
		return
	}
	// The last replayed result is acknowledged: continue live, admitting the
	// calls the journal did not have once their count is committed.
	c.replaying = false
	go c.pump()
	go c.release()
}

// release lets the held calls launch, and later live calls, each once its
// issued count is committed. On a rerun all held calls are checked first
// against the calls an earlier attempt admitted under their keys, and a
// divergence launches none.
func (c *call) release() {
	<-c.gate
	c.mu.Lock()
	if c.stoppedLocked() || c.released {
		c.mu.Unlock()
		return
	}
	calls := c.held
	c.held = nil
	rerun := c.rerun
	c.mu.Unlock()
	if rerun {
		for _, made := range calls {
			other, err := c.earlier(made)
			if err != nil {
				c.fail(err)
				return
			}
			if c.stopped() {
				return
			}
			if other != "" {
				c.diverge(fmt.Sprintf("call %d is %s, but was %s", made.ID, describeCall(made), other))
				return
			}
		}
	}
	c.mu.Lock()
	c.released = true
	c.waiting = append(append(c.waiting, calls...), c.held...)
	c.held = nil
	c.launchCoveredLocked()
	c.mu.Unlock()
}

// launchCoveredLocked launches the waiting calls whose issued count is
// committed.
func (c *call) launchCoveredLocked() {
	if !c.released || c.stoppedLocked() {
		return
	}
	var covered, rest []sandbox.HostCall
	for _, made := range c.waiting {
		if made.ID <= c.durableIssued {
			covered = append(covered, made)
		} else {
			rest = append(rest, made)
		}
	}
	c.waiting = rest
	if len(covered) == 0 {
		return
	}
	c.starting = append(c.starting, covered...)
	if !c.launching {
		c.launching = true
		go c.launcher()
	}
}

// launcher starts the covered calls in call order: a call has started once a
// tool call is admitted or a global is executing, or once it has ended.
func (c *call) launcher() {
	for {
		c.mu.Lock()
		if len(c.starting) == 0 {
			c.launching = false
			c.mu.Unlock()
			return
		}
		made := c.starting[0]
		c.starting = c.starting[1:]
		c.mu.Unlock()
		started := make(chan struct{})
		var once sync.Once
		go c.launch(made, func() { once.Do(func() { close(started) }) })
		<-started
	}
}

// earlier is the call an earlier attempt made under this call's key, when it
// is another one: another target, tool, or arguments; "" otherwise.
func (c *call) earlier(made sandbox.HostCall) (string, error) {
	snapshot, err := c.api.Snapshot(c.ctx, harness.NestedCallDoc.Task(c.api.TaskID()).Member("c"+strconv.Itoa(made.ID), nil))
	if err != nil {
		return "", err
	}
	taskID, ok := snapshot["taskId"].(float64)
	if !ok {
		return "", nil
	}
	record, err := c.api.GetTask(c.ctx, durable.TaskID(taskID))
	if err != nil || record == nil {
		return "", err
	}
	var input harness.ToolTaskInput
	if json.Unmarshal(record.Input, &input) != nil || input.Kind != "nested" || input.Call == nil {
		return "", nil
	}
	arguments, err := json.Marshal(input.Call.Arguments)
	if err != nil {
		return "", err
	}
	args := made.Args
	if args == nil {
		args = json.RawMessage("{}")
	}
	if made.Target == "tool" && input.Call.Name == made.Name && sameJSON(arguments, args) {
		return "", nil
	}
	return "tools." + input.Call.Name + "(" + string(arguments) + ")", nil
}

// launch runs one live call and journals its result.
func (c *call) launch(made sandbox.HostCall, started func()) {
	defer started()
	if c.stopped() {
		return
	}
	var outcome sandbox.Outcome
	var global *journalGlobal
	var err error
	if made.Target == "tool" {
		outcome, err = c.callTool(made, started)
	} else {
		outcome, err = c.callGlobal(made, started)
		if err == nil {
			var journaled journalOutcome
			journaled, err = fromOutcome(outcome)
			global = &journalGlobal{Name: made.Name, Args: made.Args, Outcome: journaled}
		}
	}
	if err != nil {
		// Stopped meanwhile: the outcome is decided.
		if c.stopped() {
			return
		}
		if c.ctx.Err() != nil {
			c.fail(err)
		} else {
			// A key an earlier attempt admitted with another tool or other
			// arguments: this attempt took another path.
			c.diverge(fmt.Sprintf("call %d is %s: %s", made.ID, describeCall(made), err.Error()))
		}
		return
	}
	at, err := nowMs(c.api)
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	// A result after the outcome is decided has nowhere to go.
	if c.stoppedLocked() {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, queued{id: made.ID, at: at, outcome: outcome, global: global})
	c.mu.Unlock()
	c.pump()
}

// callTool calls started, when set, once the nested call is admitted.
func (c *call) callTool(made sandbox.HostCall, started func()) (sandbox.Outcome, error) {
	// Passed as the script gave them; an argument list that is not an object
	// cannot reach the tool.
	args := map[string]any{}
	if made.Args != nil {
		if err := json.Unmarshal(made.Args, &args); err != nil {
			outcome := sandbox.Outcome{OK: false, Message: fmt.Sprintf("Arguments of %s must be an object", made.Name)}
			c.finishRow(made.ID, outcome, harness.NestedToolExecutionResult{IsError: true})
			return outcome, nil
		}
		if args == nil {
			args = map[string]any{}
		}
	}
	options := harness.NestedCallOptions{Key: "c" + strconv.Itoa(made.ID)}
	if started != nil {
		options.OnAdmitted = func(durable.TaskID) { started() }
	}
	result, err := c.api.ExecuteTool(c.ctx, made.Name, args, options)
	if err != nil {
		return sandbox.Outcome{}, err
	}
	structured := false
	for _, tool := range c.start.Tools {
		if tool.Name == made.Name {
			structured = tool.Structured
		}
	}
	outcome := outcomeOf(made.Name, structured, result)
	c.finishRow(made.ID, outcome, result)
	return outcome, nil
}

// outcomeOf is what a script gets for a nested call. A tool with a schema
// resolves with its value, also on an error result that carries one; a
// schema-less tool resolves with its text output, images left out. Any other
// error rejects, with its text: a schema-less tool's output, else its error
// diagnostics.
func outcomeOf(name string, structured bool, result harness.NestedToolExecutionResult) sandbox.Outcome {
	var value any
	hasValue := false
	if structured {
		if result.StructuredOutput != nil {
			value, hasValue = result.StructuredOutput, true
		}
	} else {
		value, hasValue = textOf(result.StructuredOutput), true
	}
	if !result.IsError || (structured && hasValue) {
		if !hasValue {
			return sandbox.Outcome{OK: true}
		}
		return sandbox.Outcome{OK: true, Value: value}
	}
	message := ""
	if !structured {
		message = value.(string)
	}
	var errs []string
	for _, d := range result.Diagnostics {
		if d.Severity == "error" {
			errs = append(errs, d.Message)
		}
	}
	if message == "" {
		message = strings.Join(errs, "\n")
	}
	if message == "" {
		message = "Tool " + name + " failed"
	}
	outcome := sandbox.Outcome{OK: false, Message: message}
	if hasValue {
		var property any = value
		if raw, ok := value.(json.RawMessage); ok {
			var decoded any
			_ = json.Unmarshal(raw, &decoded)
			property = decoded
		}
		outcome.Properties = map[string]any{"structuredOutput": property}
	}
	return outcome
}

// textOf is the text of a schema-less tool's collapsed output: a string, one
// image, or an array of text and image blocks.
func textOf(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var texts []string
	for _, item := range items {
		if item.Type == "text" && item.Text != nil {
			texts = append(texts, *item.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// startedLocked: the script made a tool call, so its row appears, running,
// also while it waits to launch.
func (c *call) startedLocked(made sandbox.HostCall) {
	if _, ok := c.rows[made.ID]; ok {
		return
	}
	name := made.Name
	if hasInternalPrefix(name) {
		name = name[len(internalPrefix):]
	}
	c.rows[made.ID] = &NestedCall{ID: made.ID, Name: name, Args: cut(string(made.Args), argsPreviewChars), Status: "running"}
}

// finishRow: an error result is an error row, also when the script got its
// value (bash's nonzero exit).
func (c *call) finishRow(id int, outcome sandbox.Outcome, result harness.NestedToolExecutionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[id]
	if !ok {
		return
	}
	row.Status = "ok"
	if result.IsError {
		row.Status = "error"
	}
	if result.DurationMs != nil {
		duration := *result.DurationMs
		row.DurationMs = &duration
	}
	var errs []string
	for _, d := range result.Diagnostics {
		if d.Severity == "error" {
			errs = append(errs, d.Message)
		}
	}
	text := strings.Join(errs, "\n")
	if !outcome.OK {
		text = outcome.Message
	}
	if result.IsError && text != "" {
		row.Error = cut(text, errorPreviewChars)
	}
}

// details are the rows in call order; calls still running when the script
// ended were cancelled.
func (c *call) details() ToolDetails {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := []NestedCall{}
	for _, row := range c.rows {
		copied := *row
		if copied.Status == "running" {
			copied.Status = "cancelled"
		}
		calls = append(calls, copied)
	}
	slices.SortFunc(calls, func(a, b NestedCall) int { return a.ID - b.ID })
	return ToolDetails{Calls: calls}
}

// callGlobal calls started as the global begins executing.
func (c *call) callGlobal(made sandbox.HostCall, started func()) (sandbox.Outcome, error) {
	var value any
	var err error
	started()
	switch {
	case slices.Contains(discoveryGlobals, made.Name):
		value, err = c.discover(made)
	default:
		index := slices.IndexFunc(c.options.Globals, func(g Global) bool { return g.Name == made.Name })
		if index == -1 {
			return sandbox.Outcome{OK: false, Message: "Unknown global " + made.Name}, nil
		}
		value, err = c.options.Globals[index].Execute(c.ctx, made.Args, c.api)
	}
	if err != nil {
		if c.ctx.Err() != nil {
			return sandbox.Outcome{}, err
		}
		return sandbox.Outcome{OK: false, Message: err.Error()}, nil
	}
	// nil stays undefined: the journal omits the value, and a replay
	// resolves with undefined again.
	return sandbox.Outcome{OK: true, Value: value}, nil
}

func (c *call) matchesNamespace(namespace startNamespace, query string) bool {
	return namespace.Name == query || sandbox.Identifier(namespace.Name) == sandbox.Identifier(query)
}

// discover answers searchTools(), describeTool() and describeNamespace()
// from the start data.
func (c *call) discover(made sandbox.HostCall) (any, error) {
	var args []any
	_ = json.Unmarshal(made.Args, &args)
	arg := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	var tools []declaredTool
	for _, tool := range c.start.Tools {
		if !tool.Hidden {
			tools = append(tools, tool)
		}
	}
	namespaces := c.start.Namespaces
	namespaceOfTool := func(name string) *startNamespace {
		for i := range namespaces {
			if slices.Contains(namespaces[i].Tools, name) {
				return &namespaces[i]
			}
		}
		return nil
	}
	switch made.Name {
	case "describeTool":
		name, ok := arg(0).(string)
		if !ok {
			return nil, errors.New("describeTool() expects a tool name")
		}
		for _, tool := range tools {
			if tool.Name == name || tool.JSName == name {
				return tool.Description, nil
			}
		}
		return nil, nil
	case "describeNamespace":
		name, ok := arg(0).(string)
		if !ok {
			return nil, errors.New("describeNamespace() expects a namespace name")
		}
		for _, namespace := range namespaces {
			if c.matchesNamespace(namespace, name) {
				ids := make([]string, len(namespace.Tools))
				for i, tool := range namespace.Tools {
					ids[i] = sandbox.Identifier(tool)
				}
				described := map[string]any{"name": namespace.Name, "tools": ids}
				if namespace.Description != "" {
					described["description"] = namespace.Description
				}
				if namespace.Instructions != "" {
					described["instructions"] = namespace.Instructions
				}
				return described, nil
			}
		}
		return nil, nil
	}
	query, ok := arg(0).(string)
	if !ok {
		return nil, errors.New("searchTools() expects a query string")
	}
	options, _ := arg(1).(map[string]any)
	limit := float64(defaultSearchLimit)
	if value, present := options["limit"]; present && value != nil {
		number, ok := value.(float64)
		if !ok || number != float64(int(number)) || number <= 0 {
			return nil, errors.New("searchTools() limit must be a positive integer")
		}
		limit = number
	}
	namespace, hasNamespace := options["namespace"]
	if hasNamespace && namespace != nil {
		if _, ok := namespace.(string); !ok {
			return nil, errors.New("searchTools() namespace must be a string")
		}
	}
	var documents []search.Document
	for _, tool := range tools {
		own := namespaceOfTool(tool.Name)
		if name, ok := namespace.(string); ok && (own == nil || !c.matchesNamespace(*own, name)) {
			continue
		}
		ownName, ownDescription := "", ""
		if own != nil {
			ownName, ownDescription = own.Name, own.Description
		}
		text := strings.Join([]string{tool.Name, strings.ReplaceAll(tool.Name, "_", " "), tool.Description, ownName, ownDescription}, " ")
		documents = append(documents, search.Document{Name: tool.Name, Text: text})
	}
	found := []any{}
	for _, match := range (search.Ranker{}).Rank(query, documents, int(limit)) {
		for _, tool := range tools {
			if tool.Name == match.Name {
				found = append(found, map[string]any{"name": tool.JSName, "description": tool.Description})
				break
			}
		}
	}
	return found, nil
}

// pump journals finished calls and acknowledged counts in one commit, then
// delivers the calls and launches the calls the counts cover; one commit in
// flight at a time, so what arrives meanwhile goes into the next.
func (c *call) pump() {
	c.mu.Lock()
	if c.committing || c.stoppedLocked() || (len(c.queue) == 0 && len(c.acks) == 0) {
		c.mu.Unlock()
		return
	}
	// Counts alone wait until the VM has processed everything delivered: its
	// idle for the last result pumps them as one commit, as pi commits the
	// counts of a batch processed in one turn.
	if _, caughtUp := c.acks[c.journalLength]; len(c.queue) == 0 && !caughtUp {
		c.mu.Unlock()
		return
	}
	c.committing = true
	batch := c.queue
	c.queue = nil
	acks := c.acks
	c.acks = map[int]int{}
	c.mu.Unlock()

	entries := make([]any, 0, len(batch))
	for _, item := range batch {
		value, err := toValue(journalEntry{ID: item.id, At: item.at, Global: item.global})
		if err != nil {
			c.fail(err)
			return
		}
		entries = append(entries, value)
	}
	err := c.api.Commit(c.ctx, func(tx *durable.Tx) error {
		journal, err := tx.Doc(JournalDoc.Task(c.api.TaskID()))
		if err != nil {
			return err
		}
		list := journal.At("entries")
		for _, seq := range sortedInts(acks) {
			issued := acks[seq]
			if seq == 0 {
				if !journal.Has("firstIssued") {
					if err := journal.Set("firstIssued", float64(issued)); err != nil {
						return err
					}
				}
				continue
			}
			if seq-1 < list.Len() {
				if entry := list.At(seq - 1); !entry.Has("issuedAfter") {
					if err := entry.Set("issuedAfter", float64(issued)); err != nil {
						return err
					}
				}
			}
		}
		if len(entries) > 0 {
			if _, err := list.Push(entries...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	for _, issued := range acks {
		c.durableIssued = max(c.durableIssued, issued)
	}
	var deliveries []delivery
	for _, item := range batch {
		c.journalLength++
		deliveries = append(deliveries, delivery{seq: c.journalLength, id: item.id, at: item.at, outcome: item.outcome})
	}
	run := c.run
	c.mu.Unlock()
	deliverAll(run, deliveries)
	c.mu.Lock()
	c.launchCoveredLocked()
	c.committing = false
	c.mu.Unlock()
	c.pump()
}

func sortedInts(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
