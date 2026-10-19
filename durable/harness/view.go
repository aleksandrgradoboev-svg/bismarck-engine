package harness

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The conversation view (pi packages/durable/src/harness/view.ts, spec §9.3):
// a structural mount of one conversation's active transcript and built-in
// documents, observed as a chord value like a document.

// ConversationView is one conversation's view: its record, its raw active
// entries (as ContextView.Entries: the head marker, then the non-head entries
// from its head), and its built-in documents keyed by kind, absent ones
// absent.
type ConversationView struct {
	Conversation durable.ConversationRecord `json:"conversation"`
	Entries      []durable.EntryRecord      `json:"entries"`
	Docs         map[string]map[string]any  `json:"docs"`
}

// DecodeConversationView reads a conversation view value.
func DecodeConversationView(value map[string]any) (ConversationView, error) {
	var view ConversationView
	err := fromValue(value, &view)
	return view, err
}

// mountedDocs are the built-in documents of the view, in pi's order.
var mountedDocs = []*durable.Doc{AgentDoc, LiveDoc, InboxDoc, ProviderDoc, UsageDoc}

func isMountedKind(kind string) bool {
	return slices.ContainsFunc(mountedDocs, func(doc *durable.Doc) bool { return doc.Definition().Kind == kind })
}

// publicationHook sees every publication after the mount took it, with the
// revisions before and after and the mount's operations, possibly none (pi
// ViewObserver.publication, for the harness's event adapters).
type publicationHook func(ctx context.Context, before, after map[string]any, ops []delta.Op, publication durable.CommitPublication)

// viewObserver is one observer of a mount.
type viewObserver struct {
	observer    durable.Observer
	publication publicationHook
}

// viewMount is one conversation's mount: its current revision, the active
// entries it shows, the document incarnations it shows, and its observers.
type viewMount struct {
	value   map[string]any
	entries []durable.EntryRecord
	// docs is the mounted incarnation and definition version per kind;
	// another incarnation or version is set whole.
	docs      map[string]mountedDoc
	observers []*viewObserver
}

type mountedDoc struct {
	id      durable.DocumentID
	version int
}

// conversationViews are the Harness's view mounts: at most one per
// conversation, built on the Session line by its first observer and dropped
// with its last. Each mount advances from the Session's commit publications,
// which are durable.
type conversationViews struct {
	h      *Harness
	mu     sync.Mutex
	mounts map[durable.ConversationID]*viewMount
	closed bool
}

func newConversationViews(h *Harness) (*conversationViews, error) {
	v := &conversationViews{h: h, mounts: map[durable.ConversationID]*viewMount{}}
	if _, err := h.Session.ObserveCommits(func(publication durable.CommitPublication, ctx context.Context) { v.advance(publication, ctx) }); err != nil {
		return nil, err
	}
	if _, err := h.Session.SubscribeClose(v.close); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *conversationViews) report(err error) { v.h.report(err, durable.ListenerError) }

// contained runs a listener; its panic goes to report.
func contained(run func(), report func(error)) {
	defer func() {
		if r := recover(); r != nil {
			report(panicError(r))
		}
	}()
	run()
}

func (v *conversationViews) close() {
	v.mu.Lock()
	v.closed = true
	mounts := v.mounts
	v.mounts = map[durable.ConversationID]*viewMount{}
	v.mu.Unlock()
	failure := v.h.Session.Failure()
	for _, mount := range mounts {
		for _, o := range slices.Clone(mount.observers) {
			contained(func() { o.observer.CloseSession(failure) }, v.report)
		}
	}
}

// state attaches a disposable read-only chord state of the view.
func (v *conversationViews) state(ctx context.Context, id durable.ConversationID) (durable.DocumentState, error) {
	var state durable.DocumentState
	err := v.attach(ctx, id, func(value map[string]any, release func()) *viewObserver {
		var observer durable.Observer
		observer, state = durable.NewCommittedState(value, release, v.report)
		return &viewObserver{observer: observer}
	})
	if err != nil {
		// An observer made before attach refused it was never registered.
		if state != nil {
			state.Dispose()
		}
		return nil, err
	}
	return state, nil
}

// watch acquires a serialized exact-frame watch of the view; cancelling ctx
// cancels acquisition, and afterwards the watch.
func (v *conversationViews) watch(ctx context.Context, id durable.ConversationID) (*durable.DocumentWatch, error) {
	var watch *durable.DocumentWatch
	err := v.attach(ctx, id, func(value map[string]any, release func()) *viewObserver {
		watch = durable.NewCommittedWatch(value, release, v.report)
		return &viewObserver{observer: watch}
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		watch.Cancel()
		return nil, context.Cause(ctx)
	}
	if ctx.Done() != nil {
		watch.ObserveCancellation(ctx)
	}
	return watch, nil
}

// attach registers an observer created from the current revision,
// atomically on the Session line: it sees every later publication and
// nothing earlier. release drops it, and the mount with its last observer.
func (v *conversationViews) attach(ctx context.Context, id durable.ConversationID, create func(value map[string]any, release func()) *viewObserver) error {
	return v.h.Session.ReadOnLine(func() error {
		v.mu.Lock()
		mount := v.mounts[id]
		closed := v.closed
		v.mu.Unlock()
		if closed {
			return closedError(v.h.Session)
		}
		if mount == nil {
			built, err := v.build(ctx, id)
			if err != nil {
				return err
			}
			mount = built
		}
		var observer *viewObserver
		release := func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			for i, o := range mount.observers {
				if o == observer {
					mount.observers = slices.Delete(mount.observers, i, i+1)
					break
				}
			}
			if len(mount.observers) == 0 && v.mounts[id] == mount {
				delete(v.mounts, id)
			}
		}
		observer = create(mount.value, release)
		v.mu.Lock()
		defer v.mu.Unlock()
		// Close or cancellation may begin while the mount builds; register
		// nothing then.
		if v.closed {
			return closedError(v.h.Session)
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		v.mounts[id] = mount
		mount.observers = append(mount.observers, observer)
		return nil
	})
}

// build reads a mount from committed Storage, on the line.
func (v *conversationViews) build(ctx context.Context, id durable.ConversationID) (*viewMount, error) {
	storage := v.h.Session.Storage()
	conversation, err := storage.Conversation(ctx, id)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, fmt.Errorf("Conversation %d does not exist", id)
	}
	bounds, err := captureContextBounds(ctx, storage, id, nil)
	if err != nil {
		return nil, err
	}
	entries := []durable.EntryRecord{}
	if bounds != nil {
		scanned, err := scanRange(ctx, storage, id, bounds)
		if err != nil {
			return nil, err
		}
		entries = selectActive(bounds.head, scanned)
	}
	mount := &viewMount{entries: entries, docs: map[string]mountedDoc{}}
	docs := map[string]any{}
	for _, doc := range mountedDocs {
		loaded, err := v.h.Session.ConversationDocumentOnLine(ctx, doc.Conversation(id))
		if err != nil {
			return nil, err
		}
		if loaded == nil {
			continue
		}
		docs[doc.Definition().Kind] = loaded.Value
		mount.docs[doc.Definition().Kind] = mountedDoc{id: loaded.Record.ID, version: loaded.Version}
	}
	record, err := toValue(conversation)
	if err != nil {
		return nil, err
	}
	values := make([]any, 0, len(entries))
	for _, entry := range entries {
		value, err := toValue(entry)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	mount.value = map[string]any{"conversation": record, "entries": values, "docs": docs}
	return mount, nil
}

// advance derives each mount's operations from one publication, applies
// them, and hands the revision to every observer; one that panics is
// reported, and the others still get it.
func (v *conversationViews) advance(publication durable.CommitPublication, ctx context.Context) {
	v.mu.Lock()
	type delivery struct {
		observers     []*viewObserver
		before, after map[string]any
		ops           []delta.Op
	}
	var deliveries []delivery
	for id, mount := range v.mounts {
		before := mount.value
		ops, err := mount.take(id, publication)
		if err != nil {
			v.report(err)
			continue
		}
		deliveries = append(deliveries, delivery{observers: slices.Clone(mount.observers), before: before, after: mount.value, ops: ops})
	}
	v.mu.Unlock()
	frame := context.WithoutCancel(ctx)
	for _, d := range deliveries {
		if len(d.ops) > 0 {
			for _, o := range d.observers {
				contained(func() { o.observer.Advance(frame, d.after, d.ops) }, v.report)
			}
		}
		for _, o := range d.observers {
			if o.publication != nil {
				contained(func() { o.publication(frame, d.before, d.after, d.ops, publication) }, v.report)
			}
		}
	}
}

// take applies one publication to the mount and returns its operations:
// documents first, then entries, as pi orders them.
func (m *viewMount) take(id durable.ConversationID, publication durable.CommitPublication) ([]delta.Op, error) {
	var docOps, entryOps []delta.Op
	entries := m.entries
	// Entry writes are published in ID order.
	for _, change := range publication.Changes {
		switch c := change.(type) {
		case durable.EntryWrite:
			if c.Value.ConversationID != id {
				continue
			}
			entry := c.Value
			value, err := toValue(entry)
			if err != nil {
				return nil, err
			}
			entriesPath := delta.Path{delta.Key("entries")}
			if entry.Head == nil {
				entryOps = append(entryOps, delta.Splice{Path: entriesPath, Index: len(entries), Remove: 0, Items: []any{value}})
				entries = append(slices.Clip(entries), entry)
				continue
			}
			// A head marker keeps the non-head entries from its head, which
			// are always a suffix, and goes in front.
			target := *entry.Head
			kept := slices.IndexFunc(entries, func(candidate durable.EntryRecord) bool {
				return candidate.Head == nil && candidate.ID >= target
			})
			if kept < 0 {
				kept = len(entries)
			}
			entryOps = append(entryOps, delta.Splice{Path: entriesPath, Index: 0, Remove: kept, Items: []any{value}})
			entries = append([]durable.EntryRecord{entry}, entries[kept:]...)
		case durable.DocumentChange:
			if c.ConversationID != id {
				continue
			}
			kind := c.Record.Kind
			if !isMountedKind(kind) || c.Record.Key != nil {
				continue
			}
			path := delta.Path{delta.Key("docs"), delta.Key(kind)}
			mounted, ok := m.docs[kind]
			switch {
			case c.Value == nil:
				if !ok || mounted.id != c.Record.ID {
					continue
				}
				delete(m.docs, kind)
				docOps = append(docOps, delta.Delete{Path: path})
			case ok && mounted.id == c.Record.ID && mounted.version == c.Version:
				for _, op := range c.Ops {
					docOps = append(docOps, prefixed(op, path))
				}
			default:
				m.docs[kind] = mountedDoc{id: c.Record.ID, version: c.Version}
				docOps = append(docOps, delta.Set{Path: path, Value: c.Value})
			}
		}
	}
	ops := append(docOps, entryOps...)
	if len(ops) == 0 {
		return nil, nil
	}
	value, err := delta.ApplyImmutable(m.value, ops)
	if err != nil {
		return nil, err
	}
	m.value, m.entries = value, entries
	return ops, nil
}

// prefixed is op moved under prefix; a root replacement becomes a set of the
// prefix.
func prefixed(op delta.Op, prefix delta.Path) delta.Op {
	at := func(path delta.Path) delta.Path { return append(slices.Clone(prefix), path...) }
	switch o := op.(type) {
	case delta.Replace:
		return delta.Set{Path: slices.Clone(prefix), Value: o.Value}
	case delta.Splice:
		return delta.Splice{Path: at(o.Path), Index: o.Index, Remove: o.Remove, Items: o.Items}
	case delta.Permute:
		return delta.Permute{Path: at(o.Path), Permutation: o.Permutation}
	case delta.Set:
		return delta.Set{Path: at(o.Path), Value: o.Value}
	case delta.Delete:
		return delta.Delete{Path: at(o.Path)}
	case delta.Append:
		return delta.Append{Path: at(o.Path), Text: o.Text}
	case delta.Truncate:
		return delta.Truncate{Path: at(o.Path), Count: o.Count}
	}
	return op
}

// ViewState attaches the conversation's view (spec §9.3) as a disposable
// read-only chord state; DecodeConversationView reads its value.
func (c *Conversation) ViewState(ctx context.Context) (durable.DocumentState, error) {
	return c.h.views.state(ctx, c.ID)
}

// Watch acquires the conversation's view as a serialized exact-frame watch
// with bounded pending frames; cancelling ctx cancels acquisition, and
// afterwards the watch.
func (c *Conversation) Watch(ctx context.Context) (*durable.DocumentWatch, error) {
	return c.h.views.watch(ctx, c.ID)
}
