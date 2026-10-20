// Package harness is pi's durable agent harness (packages/durable/src/harness):
// conversations with a stored agent, extensions in a registry, and the
// built-in tasks that run the agent, over a durable.Session.
//
// The port arrives in stages. This one holds the registry, agent resolution,
// settings, system prompt planning, model context derivation, task
// scheduling, submissions with the inbox, and the core of generation; tool
// calls and compaction follow.
package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// EnvTarget is what the env builder builds an environment for.
type EnvTarget struct {
	ConversationID durable.ConversationID
	// Cwd is the conversation's agent Cwd.
	Cwd  *string
	Read DocumentReader
}

// Options configure a Harness.
type Options struct {
	// Models is the model access generation uses.
	Models ai.Models
	// ModelsFunc, when set, replaces Models and is read at every access:
	// each model request and each Models() of a TaskRuntime or tool API, so
	// a host can swap catalogs, credentials, or transforms while the Harness
	// runs. It must not block.
	ModelsFunc func() ai.Models
	// Registry is required; it may keep changing while the Harness runs.
	Registry RegistryReader
	// Settings returns the Harness-wide run policy; it is read at every
	// resolution and never copied. nil is every default. It must not block:
	// some readers run on the Session line.
	Settings func() HarnessSettings
	// Env builds a conversation's environment at each use; nil leaves tools
	// without one. Never called on the Session line.
	Env func(ctx context.Context, target EnvTarget) (env.ExecutionEnv, error)
	// ConversationCreated runs in every commit that creates or forks a
	// conversation, raw Tx.CreateConversation included, after the built-in
	// pi.* documents and before the conveniences apply the agent change and
	// run init. A fork already has its copies. An error fails the creating
	// commit.
	ConversationCreated func(tx *durable.Tx, conversation durable.ConversationRecord) error
	// Now is the clock; nil is time.Now. Sleeping tasks store deadlines on it,
	// so every process that opens the same storage must use the same clock.
	Now func() time.Time
	// SetTimer arms every timer the Harness uses: freeing idle memory
	// (background), and, without OnWake, resuming sleeping tasks; and the
	// timers of TaskRuntime.SetTimer. nil is durable.NativeTimer. Tests can
	// pass fake timers; a host can keep timers off a process that should
	// hibernate.
	SetTimer durable.SetTimer
	// OnWake wakes the process for sleeping tasks instead of a SetTimer timer.
	// Without it, while scheduling runs, a timer resumes sleeping tasks when
	// they are due. With it, the Harness arms no timer for them: it calls
	// OnWake after open and whenever Harness.Wake changes, and the host arms
	// its own wakeup and calls Resume when it fires. It is called on its own
	// goroutine, one call at a time; it may call Wake and Resume, and must
	// not block. A panic is reported, and the same wake is offered again at
	// the next change or pass.
	OnWake func(Wake)
	// OnReport receives failures that do not fail the calling operation, such
	// as a failing hook or listener, and the error that fails the Session,
	// once; info says where each came from. A host acts on reports outside
	// the scheduler, for example counting faults of one task kind toward a
	// breaker. Its own panic is dropped.
	OnReport func(error, durable.ReportInfo)
	// Telemetry is the host's adapter for content-free admission and phase
	// telemetry; its metrics and propagation are optional. Nil is
	// telemetry.NoopContext.
	Telemetry telemetry.Context
}

// ConversationInit runs inside the creating commit, after the creation hook
// and the agent change. The conversation creation is already a table write,
// so table reads here fail with ReadAfterWrite; document access remains
// available.
type ConversationInit func(tx *durable.Tx, conversationID durable.ConversationID) error

// ConversationCreateOptions are the options of a created or forked
// conversation.
type ConversationCreateOptions struct {
	Ownership durable.ConversationOwnership
	// Agent is applied in the creating commit after the creation hook's copy,
	// before Init.
	Agent *AgentChange
	Init  ConversationInit
}

// Harness is the durable agent harness over one Session, whose methods it
// carries.
type Harness struct {
	*durable.Session
	options Options
	// telemetry is Options.Telemetry, or telemetry.NoopContext.
	telemetry telemetry.Context
	// reportIn reports with an operation's context; report without one.
	reportIn    reportFunc
	report      func(error, durable.ReportInfo)
	tasks       *scheduler
	submissions *submissions
	views       *conversationViews
}

// Open opens a Harness over storage.
func Open(ctx context.Context, storage durable.Storage, options Options) (*Harness, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if options.Registry == nil {
		return nil, errors.New("Harness options need a registry")
	}
	snapshot := options.Registry.Snapshot()
	var missing []string
	for _, task := range builtinTasks {
		if snapshot.Task(task.Name()) == nil {
			missing = append(missing, task.Name())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Registry lacks built-in tasks %s; create it with NewRegistry()", strings.Join(missing, ", "))
	}
	adapter := options.Telemetry
	if adapter == nil {
		adapter = telemetry.NoopContext
	}
	h := &Harness{options: options, telemetry: adapter, reportIn: safeReport(options.OnReport, adapter)}
	h.report = h.reportIn.background()
	now := safeNow(options.Now, h.report)
	h.Session = durable.NewSession(storage, durable.SessionOptions{
		Now: now,
		// Read at each use, as all settings are, but only this field: it is
		// read on every commit.
		DocumentRetention: func() time.Duration { return h.settings().DocumentRetention },
		SetTimer:          options.SetTimer,
		Hooks: durable.SessionHooks{
			Report:              h.reportIn,
			ConversationCreated: h.conversationCreated,
			// Signal and join task invocations after the close listeners and
			// before Storage closes; writes no task outcome, only the stopped
			// tasks back as pending.
			BeforeClose: func(writeAtClose func(writes func() []durable.StorageWrite) error) { h.tasks.join(writeAtClose) },
			TaskGraphBlocking: &durable.TaskGraphBlocking{
				Blocked:   func(record durable.TaskRecord) string { return h.tasks.blockedReason(record) },
				Subscribe: func(listener func()) func() { return h.tasks.subscribeBlocked(listener) },
			},
		},
	})
	h.tasks = newScheduler(schedulerOptions{
		session:  h.Session,
		registry: options.Registry,
		models:   currentModels(options),
		agent: func(ctx context.Context, id durable.ConversationID, snapshot *RegistrySnapshot) (*Agent, error) {
			return h.resolveAgent(ctx, id, snapshot)
		},
		settings: h.settings,
		env:      h.buildEnv,
		now:      now,
		onWake:   options.OnWake,
		timers: durable.HostTimers(options.SetTimer, func(err error) {
			h.report(err, durable.HostError(durable.CallbackSetTimer, nil))
		}),
		report:        h.report,
		reportIn:      h.reportIn,
		telemetry:     adapter,
		settleOutcome: settleSchedulerOutcome,
		withdrawInputs: func(tx *durable.Tx, id durable.ConversationID) error {
			return withdrawQueuedInputs(tx, id, "aborted")
		},
		ctx: context.WithoutCancel(ctx),
	})
	submissions, err := newSubmissions(h)
	if err != nil {
		_ = h.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	h.submissions = submissions
	if h.views, err = newConversationViews(h); err != nil {
		_ = h.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := h.tasks.open(ctx); err != nil {
		// The caller's context may be what failed open: close without it, and
		// return the open error. A failing Storage close fails the Session,
		// which reports it.
		_ = h.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return h, nil
}

// Resume enables task scheduling. It is idempotent. The calls that ask for
// progress enable it too: WaitForTask, WaitForIdle, and a conversation's
// Abort and WaitForIdle; read-only viewers never do.
func (h *Harness) Resume() error {
	if h.tasks.closing.Load() {
		return closedError(h.Session)
	}
	h.tasks.resume()
	return nil
}

// Wake is when a Session next needs a running process. Active: some task can
// run now or is running, so a process that dies must be woken again soon, for
// example by a recovery alarm. At: the earliest sleep not yet due; zero when
// there is none. Neither: nothing needs a process until new work is
// submitted. Tasks blocked on a missing or unmigratable definition,
// completing tasks, and waits on live tasks do not count; the work they wait
// for does.
type Wake struct {
	Active bool
	At     time.Time
}

// Wake says when this Session next needs a running process, from the
// scheduler's memory; it answers also while scheduling is paused. A host that
// wakes the process itself arms from it and calls Resume when woken; see
// Options.OnWake.
func (h *Harness) Wake() (Wake, error) {
	if h.tasks.closing.Load() {
		return Wake{}, closedError(h.Session)
	}
	h.tasks.mu.Lock()
	defer h.tasks.unlock()
	return h.tasks.wakeLocked(), nil
}

// RetryTask runs a task parked by the restart limit again (spec §5.4): it
// clears its restart count and enables scheduling. It does nothing for a task
// that is not parked, and fails for an unknown one. A host shows parked tasks
// (Inspect, the task graph's Blocked) and offers this.
func (h *Harness) RetryTask(ctx context.Context, id durable.TaskID) error {
	if h.tasks.closing.Load() {
		return closedError(h.Session)
	}
	return h.tasks.retry(ctx, id)
}

// AbortTask commits the abort mark, signals and joins an active run
// invocation, and schedules the abort invocation. A task whose definition
// cannot take it settles as orphaned instead. It returns "marked" or
// "terminal".
func (h *Harness) AbortTask(ctx context.Context, id durable.TaskID) (string, error) {
	return h.tasks.abort(ctx, id, false)
}

// WaitForTask returns the task's terminal record; cancelling ctx cancels only
// this wait.
func (h *Harness) WaitForTask(ctx context.Context, id durable.TaskID) (durable.TaskRecord, error) {
	h.tasks.resume()
	return h.tasks.waitForTask(ctx, id)
}

// WaitForIdle returns when the ordinary ownership scope of every ownerless
// conversation has no live non-background task.
func (h *Harness) WaitForIdle(ctx context.Context) error {
	h.tasks.resume()
	return h.tasks.waitForIdle(ctx, 0)
}

// HarnessInspection is a point-in-time view of live work: unfinished tasks and
// submissions, read on the Session line.
type HarnessInspection struct {
	// Scheduling is "paused", "running" or "closing".
	Scheduling string
	Tasks      []TaskInspection
	// Submissions are the queued and placed submissions, in ID order.
	Submissions []durable.SubmissionRecord
}

// Inspect returns the live tasks and unsettled submissions. It writes nothing
// and runs no task code.
func (h *Harness) Inspect(ctx context.Context) (HarnessInspection, error) {
	var inspection HarnessInspection
	err := h.ReadOnLine(func() error {
		scheduling, tasks, err := h.tasks.inspect(h.options.Registry.Snapshot())
		if err != nil {
			return err
		}
		inspection.Scheduling, inspection.Tasks = scheduling, tasks
		for _, status := range []string{durable.SubmissionQueued, durable.SubmissionPlaced} {
			submissions, err := scanAll(func(cursor durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
				return h.Storage().ScanSubmissions(ctx, durable.SubmissionQuery{Status: status}, scanPageSize, cursor)
			})
			if err != nil {
				return err
			}
			inspection.Submissions = append(inspection.Submissions, submissions...)
		}
		sort.Slice(inspection.Submissions, func(i, j int) bool { return inspection.Submissions[i].ID < inspection.Submissions[j].ID })
		return nil
	})
	return inspection, err
}

// settings resolves the host settings.
func (h *Harness) settings() Settings {
	if h.options.Settings == nil {
		return resolveSettings(HarnessSettings{})
	}
	return resolveSettings(h.options.Settings())
}

// conversationCreated is the built-in creation hook, in every commit that
// creates or forks a conversation: empty pi.live, pi.inbox and pi.usage, a
// fresh pi.provider, the conversation's pi.agent (see createAgent), then
// Options.ConversationCreated.
func (h *Harness) conversationCreated(tx *durable.Tx, record durable.ConversationRecord) error {
	for _, doc := range []*durable.Doc{LiveDoc, InboxDoc, UsageDoc, ProviderDoc} {
		if _, err := tx.Doc(doc.Conversation(record.ID)); err != nil {
			return err
		}
	}
	if err := createAgent(tx, record); err != nil {
		return err
	}
	if h.options.ConversationCreated != nil {
		return h.options.ConversationCreated(tx, record)
	}
	return nil
}

// resolveAgent resolves a conversation's committed pi.agent against snapshot,
// or the current one, and the current settings.
func (h *Harness) resolveAgent(ctx context.Context, id durable.ConversationID, snapshot *RegistrySnapshot) (*Agent, error) {
	if snapshot == nil {
		snapshot = h.options.Registry.Snapshot()
	}
	value, err := h.Snapshot(ctx, AgentDoc.Conversation(id))
	if err != nil {
		return nil, err
	}
	var state *AgentState
	if value != nil {
		state = &AgentState{}
		if err := fromValue(value, state); err != nil {
			return nil, err
		}
	}
	return resolveAgent(state, snapshot, h.settings(), func(err error, extension string) {
		h.reportIn(ctx, err, durable.ReportInfo{Type: durable.ReportExtensionError, ConversationID: id, Extension: extension})
	}), nil
}

// buildEnv builds a conversation's environment from its current Cwd; nil
// without an env builder.
func (h *Harness) buildEnv(ctx context.Context, id durable.ConversationID) (env.ExecutionEnv, error) {
	if h.options.Env == nil {
		return nil, nil
	}
	value, err := h.Snapshot(ctx, AgentDoc.Conversation(id))
	if err != nil {
		return nil, err
	}
	var state AgentState
	if err := fromValue(value, &state); err != nil {
		return nil, err
	}
	return h.options.Env(ctx, EnvTarget{ConversationID: id, Cwd: state.Cwd, Read: h.Session})
}

// Submission returns the handle of an existing submission, or nil.
func (h *Harness) Submission(ctx context.Context, id durable.SubmissionID) (*Submission, error) {
	return h.submissions.get(ctx, id)
}

// AbortSubmission withdraws a queued submission and removes its inbox item:
// SubmissionAborted, SubmissionAlreadyPlaced, SubmissionSettled, or
// SubmissionNotFound, also for one outside conversationID when that is not
// zero.
func (h *Harness) AbortSubmission(ctx context.Context, id durable.SubmissionID, conversationID durable.ConversationID) (string, error) {
	return h.submissions.abort(ctx, id, conversationID)
}

// GetTask returns the committed task record, or nil.
func (h *Harness) GetTask(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	var record *durable.TaskRecord
	err := h.ReadOnLine(func() error {
		var err error
		record, err = h.Storage().Task(ctx, id)
		return err
	})
	return record, err
}

// Usage returns the Session total: every conversation's pi.usage summed. Each
// document is read at its own point; totals only grow.
func (h *Harness) Usage(ctx context.Context) (UsageState, error) {
	var conversations []durable.ConversationRecord
	err := h.ReadOnLine(func() error {
		var err error
		conversations, err = scanAll(func(cursor durable.Cursor) (durable.Page[durable.ConversationRecord], error) {
			return h.Storage().ScanConversations(ctx, durable.ConversationQuery{}, scanPageSize, cursor)
		})
		return err
	})
	if err != nil {
		return UsageState{}, err
	}
	total := UsageState{Models: map[string]ai.Usage{}, Tools: map[string]ai.Usage{}}
	for _, conversation := range conversations {
		value, err := h.Snapshot(ctx, UsageDoc.Conversation(conversation.ID))
		if err != nil {
			return UsageState{}, err
		}
		if value == nil {
			continue
		}
		var state UsageState
		if err := fromValue(value, &state); err != nil {
			return UsageState{}, err
		}
		addUsageState(&total, state)
	}
	return total, nil
}

// Root returns the reserved root conversation, creating it with agent and
// init in one commit when absent.
func (h *Harness) Root(ctx context.Context, agent *AgentChange, init ConversationInit) (*Conversation, error) {
	return h.create(ctx, createTarget{kind: "root"}, agent, init)
}

// Conversation returns the handle of an existing conversation, or nil.
func (h *Harness) Conversation(ctx context.Context, id durable.ConversationID) (*Conversation, error) {
	var record *durable.ConversationRecord
	err := h.ReadOnLine(func() error {
		var err error
		record, err = h.Storage().Conversation(ctx, id)
		return err
	})
	if err != nil || record == nil {
		return nil, err
	}
	return &Conversation{ID: record.ID, h: h}, nil
}

// CreateConversation creates an independent conversation.
func (h *Harness) CreateConversation(ctx context.Context, options ConversationCreateOptions) (*Conversation, error) {
	return h.create(ctx, createTarget{kind: "independent", ownership: options.Ownership}, options.Agent, options.Init)
}

type createTarget struct {
	kind      string
	ownership durable.ConversationOwnership
	parentID  durable.ConversationID
	at        durable.EntryID
}

func (h *Harness) create(ctx context.Context, target createTarget, agent *AgentChange, init ConversationInit) (*Conversation, error) {
	id, err := durable.CommitValue(ctx, h.Session, func(tx *durable.Tx) (durable.ConversationID, error) {
		var record durable.ConversationRecord
		var err error
		switch target.kind {
		case "root":
			existing, err := tx.Conversation(durable.RootConversationID)
			if err != nil {
				return 0, err
			}
			if existing != nil {
				return durable.RootConversationID, nil
			}
			record, err = tx.CreateRootConversation()
			if err != nil {
				return 0, err
			}
		case "fork":
			record, err = tx.ForkConversation(target.parentID, target.at, target.ownership)
		default:
			record, err = tx.CreateConversation(target.ownership)
		}
		if err != nil {
			return 0, err
		}
		if agent != nil {
			if err := Configure(tx, record.ID, *agent); err != nil {
				return 0, err
			}
		}
		if init != nil {
			if err := init(tx, record.ID); err != nil {
				return 0, err
			}
		}
		return record.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return &Conversation{ID: id, h: h}, nil
}

// Conversation is the stateless handle of one conversation, bound to the
// Harness that returned it. Compare handles by ID.
type Conversation struct {
	ID durable.ConversationID
	h  *Harness
}

// Agent returns the agent resolved with the current registry snapshot and
// settings.
func (c *Conversation) Agent(ctx context.Context) (*Agent, error) {
	return c.h.resolveAgent(ctx, c.ID, nil)
}

// Configure applies change in its own commit.
func (c *Conversation) Configure(ctx context.Context, change AgentChange) error {
	return c.h.Commit(ctx, func(tx *durable.Tx) error { return Configure(tx, c.ID, change) })
}

// Submit admits a submission in one commit and enables scheduling: idle input
// starts a run, busy input queues behind it, and a write is appended or
// queued (see SubmissionDraft).
func (c *Conversation) Submit(ctx context.Context, draft SubmissionDraft) (*Submission, error) {
	return c.h.submissions.submit(ctx, c.ID, draft)
}

// Reset admits a write of a pi.reset entry that starts a new context,
// carrying handoff as a user message unless nil (an empty string is an empty
// message, as pi tells undefined from ""). It returns after admission; while
// the conversation is busy, the reset is placed at the next boundary.
func (c *Conversation) Reset(ctx context.Context, handoff *string) error {
	entry := durable.EntryDraft{Kind: durable.ResetEntry.Kind, HeadSelf: true}
	if handoff != nil {
		entry.Model = []ai.Message{ai.NewUserText(*handoff, c.h.tasks.now().UnixMilli())}
	}
	_, err := c.h.submissions.submit(ctx, c.ID, SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &entry})
	return err
}

// Compact admits a manual compaction task and returns its ID; instructions,
// unless nil, are an additional focus for the summary. It summarizes while
// the conversation keeps working and places its summary through a write
// submission: at once when idle, otherwise at the next boundary (spec §8.7).
// It enables scheduling.
func (c *Conversation) Compact(ctx context.Context, instructions *string) (durable.TaskID, error) {
	c.h.tasks.resume()
	input := CompactionInput{Reason: CompactionManual, Instructions: instructions}
	var id durable.TaskID
	err := c.h.Commit(ctx, func(tx *durable.Tx) error {
		var err error
		id, err = createCompaction(tx, c.ID, input, nil)
		return err
	})
	return id, err
}

// Commit runs a Session commit whose Tx.CreateTask defaults to this
// conversation.
func (c *Conversation) Commit(ctx context.Context, change func(tx *durable.Tx) error) error {
	return c.h.CommitWith(ctx, durable.TransactionScope{ConversationID: c.ID}, change)
}

// Context returns the committed raw active transcript and model context. With
// at, the context as of that visible entry: the same view Fork(at) would start
// with, without creating a conversation.
func (c *Conversation) Context(ctx context.Context, at *durable.EntryID) (ContextView, error) {
	return readContext(ctx, c.h.Session, c.ID, at)
}

// Entries returns the fork-aware history of this conversation, newest first
// unless query.Order is ascending; query.ConversationID is ignored.
func (c *Conversation) Entries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	query.ConversationID = c.ID
	var page durable.Page[durable.EntryRecord]
	err := c.h.ReadOnLine(func() error {
		var err error
		page, err = c.h.Storage().ScanEntries(ctx, query, limit, cursor)
		return err
	})
	return page, err
}

// Abort marks every live non-background task of the conversation's ordinary
// ownership scope, signals them, and returns once the scope is idle.
// Background subtrees survive unless background is set: then traversal
// crosses them, and the wait covers every task it reached.
func (c *Conversation) Abort(ctx context.Context, background bool) error {
	c.h.tasks.resume()
	return c.h.tasks.abortConversation(ctx, c.ID, background)
}

// WaitForIdle returns when the conversation's ordinary ownership scope has no
// live non-background task: this conversation and the conversations owned,
// transitively, by its non-background tasks.
func (c *Conversation) WaitForIdle(ctx context.Context) error {
	c.h.tasks.resume()
	return c.h.tasks.waitForIdle(ctx, c.ID)
}

// Fork forks this conversation at a visible entry.
func (c *Conversation) Fork(ctx context.Context, at durable.EntryID, options ConversationCreateOptions) (*Conversation, error) {
	return c.h.create(ctx, createTarget{kind: "fork", ownership: options.Ownership, parentID: c.ID, at: at}, options.Agent, options.Init)
}

// safeNow returns the host clock, falling back to time.Now when it panics: a
// broken clock must not stop commits. The first panic is reported; the clock
// is read in every task change, so later ones are not.
func safeNow(now func() time.Time, report func(error, durable.ReportInfo)) func() time.Time {
	if now == nil {
		return time.Now
	}
	var reported atomic.Bool
	return func() (t time.Time) {
		defer func() {
			if r := recover(); r != nil {
				// Set first: a report handler that reads the clock again must not
				// report again.
				if reported.CompareAndSwap(false, true) {
					report(panicError(r), durable.HostError(durable.CallbackNow, nil))
				}
				t = time.Now()
			}
		}()
		return now()
	}
}

// currentModels is Options.ModelsFunc, or Options.Models returned at every
// call.
func currentModels(options Options) func() ai.Models {
	if options.ModelsFunc != nil {
		return options.ModelsFunc
	}
	models := options.Models
	return func() ai.Models { return models }
}
