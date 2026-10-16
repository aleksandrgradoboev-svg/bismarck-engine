// Package harness is pi's durable agent harness (packages/durable/src/harness):
// conversations with a stored agent, extensions in a registry, and the
// built-in tasks that run the agent, over a durable.Session.
//
// The port arrives in stages. This one holds the registry, agent resolution,
// settings, system prompt planning, model context derivation, and the Harness
// with its conversation handles; task scheduling, submissions, generation,
// tool calls and compaction follow.
package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
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
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// OnReport receives failures that do not fail the calling operation, such
	// as a failing hook or listener, and the error that fails the Session,
	// once. Its own panic is dropped.
	OnReport func(error)
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
	report  func(error)
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
	h := &Harness{options: options, report: safeReport(options.OnReport)}
	h.Session = durable.NewSession(storage, durable.SessionOptions{
		Now: safeNow(options.Now, h.report),
		// Read at each use, as all settings are, but only this field: it is
		// read on every commit.
		DocumentRetention: func() time.Duration { return h.settings().DocumentRetention },
		Hooks: durable.SessionHooks{
			Report:              h.report,
			ConversationCreated: h.conversationCreated,
		},
	})
	return h, nil
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
	return resolveAgent(state, snapshot, h.settings(), h.report), nil
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

// Fork forks this conversation at a visible entry.
func (c *Conversation) Fork(ctx context.Context, at durable.EntryID, options ConversationCreateOptions) (*Conversation, error) {
	return c.h.create(ctx, createTarget{kind: "fork", ownership: options.Ownership, parentID: c.ID, at: at}, options.Agent, options.Init)
}

// safeReport returns an OnReport that cannot fail: it is the last place an
// error goes, so its own panic is dropped.
func safeReport(onReport func(error)) func(error) {
	return func(err error) {
		if onReport == nil {
			return
		}
		defer func() { _ = recover() }()
		onReport(err)
	}
}

// safeNow returns the host clock, falling back to time.Now when it panics: a
// broken clock must not stop commits. The first panic is reported; the clock
// is read in every task change, so later ones are not.
func safeNow(now func() time.Time, report func(error)) func() time.Time {
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
					report(panicError(r))
				}
				t = time.Now()
			}
		}()
		return now()
	}
}
