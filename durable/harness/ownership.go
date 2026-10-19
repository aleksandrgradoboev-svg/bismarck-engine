package harness

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// The ownership tree of tasks and conversations (spec §5.5): a task's parent
// is its owner task, or its conversation; a conversation's parent is its
// owner task, if any. Everything here runs under scheduler.mu.

// nodeKey is a node of the tree: a task, or a conversation when task is 0.
type nodeKey struct {
	task         durable.TaskID
	conversation durable.ConversationID
}

func taskKey(id durable.TaskID) nodeKey                 { return nodeKey{task: id} }
func conversationKey(id durable.ConversationID) nodeKey { return nodeKey{conversation: id} }
func (n nodeKey) isConversation() bool                  { return n.task == 0 }

// taskNode is the immutable ownership fields of a task.
type taskNode struct {
	conversationID durable.ConversationID
	// owner is the owner task; 0 for a conversation-owned task.
	owner      durable.TaskID
	background bool
}

func nodeOf(record durable.TaskRecord) taskNode {
	node := taskNode{conversationID: record.ConversationID, background: record.Background}
	if record.Owner != nil {
		node.owner = *record.Owner
	}
	return node
}

// parentOf is the node a task hangs from: its owner task, or its
// conversation. It is also where a walk up from the task continues.
func parentOf(node taskNode) nodeKey {
	if node.owner != 0 {
		return taskKey(node.owner)
	}
	return conversationKey(node.conversationID)
}

// overlay is the candidate records a commit staged; they override committed
// records in ownership walks.
type overlay struct {
	tasks map[durable.TaskID]durable.TaskRecord
	// order lists tasks in staging order.
	order []durable.TaskID
	// edges holds the owner task of staged conversations, 0 when ownerless.
	edges map[durable.ConversationID]durable.TaskID
	// links to the staged tasks below does not list, by parent node, for walks
	// down; built at first use.
	links map[nodeKey]*set[nodeKey]
}

func overlayOf(tx *durable.Tx) *overlay {
	o := &overlay{tasks: map[durable.TaskID]durable.TaskRecord{}, edges: map[durable.ConversationID]durable.TaskID{}}
	for _, record := range tx.StagedTasks() {
		if _, ok := o.tasks[record.ID]; !ok {
			o.order = append(o.order, record.ID)
		}
		o.tasks[record.ID] = record
	}
	for _, record := range tx.StagedConversations() {
		var owner durable.TaskID
		if record.Owner != nil {
			owner = record.Owner.TaskID
		}
		o.edges[record.ID] = owner
	}
	return o
}

func (o *overlay) task(id durable.TaskID) (durable.TaskRecord, bool) {
	if o == nil {
		return durable.TaskRecord{}, false
	}
	record, ok := o.tasks[id]
	return record, ok
}

// record returns the overlay's candidate, or else the live record.
func (s *scheduler) record(id durable.TaskID, o *overlay) (durable.TaskRecord, bool) {
	if record, ok := o.task(id); ok {
		return record, true
	}
	record, ok := s.live[id]
	return record, ok
}

// setEdge records a conversation's owner task, 0 when ownerless.
func (s *scheduler) setEdge(conversationID durable.ConversationID, owner durable.TaskID) {
	s.edges[conversationID] = owner
	// The conversation now hangs from its owner, or is a root; with nothing
	// below it, it waits for the sweep.
	s.relist(conversationKey(conversationID))
	s.dropIfUnneeded(conversationKey(conversationID))
}

// settle knows a terminal task a chain load read, so walks pass through it;
// its own chain is not loaded yet.
func (s *scheduler) settle(id durable.TaskID, node taskNode) {
	s.settled[id] = node
	s.unloaded.add(id)
	s.relist(taskKey(id))
	s.dropIfUnneeded(taskKey(id))
}

// dropIfUnneeded queues an ended task or a conversation for the sweep when
// nothing is listed below it.
func (s *scheduler) dropIfUnneeded(node nodeKey) {
	if !node.isConversation() {
		if _, live := s.live[node.task]; live {
			return
		}
	}
	if _, listed := s.below[node]; listed {
		return
	}
	s.dropQueue.add(node)
	s.scheduleSweep()
}

// scheduleSweep sweeps in a job of its own on the Session line, after the
// operation underway: a rejected commit, one with nothing to write, and a read
// publish nothing, so the sweep at the end of observe would not run for what
// they loaded.
func (s *scheduler) scheduleSweep() {
	if s.sweepScheduled || s.closing.Load() {
		return
	}
	s.sweepScheduled = true
	go func() {
		// Rejected only once the Session is closing or failed, when nothing is
		// kept anyway.
		_ = s.session.ReadOnLine(func() error {
			s.mu.Lock()
			defer s.unlock()
			s.sweepScheduled = false
			s.sweep()
			return nil
		})
	}()
}

// sweep drops the queued nodes that are still unneeded: not live, nothing
// listed below. Unlisting already queued every node on the way up that lost
// its last listed node.
func (s *scheduler) sweep() {
	for _, node := range s.dropQueue.values() {
		if _, listed := s.below[node]; listed {
			continue
		}
		if node.isConversation() {
			delete(s.edges, node.conversation)
			s.roots.delete(node.conversation)
		} else if _, live := s.live[node.task]; !live {
			delete(s.settled, node.task)
			s.unloaded.delete(node.task)
		}
	}
	s.dropQueue.clear()
}

// track brings the scheduler's sets in line with a task's committed record. A
// task that ended may free the nearest live task above it: one holding its
// outcome finalizes, an abort-marked one runs its abort handler; its waiters
// may run.
func (s *scheduler) track(previous *durable.TaskRecord, record durable.TaskRecord) {
	id := record.ID
	if previous != nil && previous.State.Status == durable.TaskWaiting {
		for _, on := range previous.State.On {
			deleteFrom(s.waitersOn, on, id)
		}
	}
	if record.State.Status == durable.TaskTerminal {
		node := nodeOf(record)
		delete(s.live, id)
		s.sleeping.delete(id)
		s.intent.delete(id)
		s.runnable.delete(id)
		s.finalizeChecks.delete(id)
		s.settled[id] = node
		// Before anything is forgotten: the walk up to the task it held passes
		// the ended tasks between them.
		s.release(node, nil)
		if waiting := s.waitersOn[id]; waiting != nil {
			for _, waiter := range waiting.values() {
				s.runnable.add(waiter)
			}
		}
		s.relist(taskKey(id))
		s.dropIfUnneeded(taskKey(id))
		return
	}
	s.live[id] = record
	if previous == nil {
		if !s.known(parentOf(nodeOf(record))) {
			s.unloaded.add(id)
		}
		s.relist(taskKey(id))
	}
	if cancellationIntent(record) {
		s.intent.add(id)
	} else {
		s.intent.delete(id)
	}
	if record.State.Status == durable.TaskCompleting {
		s.runnable.delete(id)
		s.finalizeChecks.add(id)
	} else {
		s.runnable.add(id)
	}
	if record.State.Status == durable.TaskWaiting {
		for _, on := range record.State.On {
			addTo(s.waitersOn, on, id)
		}
	}
	if record.State.Status == durable.TaskSleeping {
		s.sleeping.add(id)
	} else {
		s.sleeping.delete(id)
	}
}

// release frees the nearest live task above ended ordinary work: one holding
// its outcome finalizes, an aborting one may run.
func (s *scheduler) release(node taskNode, o *overlay) {
	above, ok := s.nearestLiveAbove(node, o)
	if !ok {
		return
	}
	if above.State.Status == durable.TaskCompleting {
		s.finalizeChecks.add(above.ID)
	} else if above.AbortRequested {
		s.runnable.add(above.ID)
	}
}

// relist lists start under its parent node in below while it is a live task
// or has nodes listed below it, and unlists it otherwise; then its parent, as
// far up as that changes. A loop, as ownership chains can be deeper than the
// call stack.
func (s *scheduler) relist(start nodeKey) {
	node := start
	for {
		needed := false
		if !node.isConversation() {
			_, needed = s.live[node.task]
		}
		if _, listed := s.below[node]; listed {
			needed = true
		}
		var parent nodeKey
		if node.isConversation() {
			owner, ok := s.edges[node.conversation]
			if !ok {
				return
			}
			if owner == 0 {
				if needed {
					s.roots.add(node.conversation)
				} else {
					s.roots.delete(node.conversation)
				}
				return
			}
			parent = taskKey(owner)
		} else {
			known, ok := s.taskNode(node.task, nil)
			if !ok {
				return
			}
			parent = parentOf(known)
		}
		children := s.below[parent]
		if (children != nil && children.has(node)) == needed {
			return
		}
		if needed {
			first := children == nil
			addTo(s.below, parent, node)
			if !first {
				return
			}
		} else {
			deleteFrom(s.below, parent, node)
			if _, listed := s.below[parent]; listed {
				return
			}
			s.dropIfUnneeded(parent)
		}
		node = parent
	}
}

// nearestLiveAbove returns the nearest live task above ended ordinary work
// node, in the overlay's view: the one that work held, if any. Work at or
// below a background task holds nothing above it.
func (s *scheduler) nearestLiveAbove(node taskNode, o *overlay) (durable.TaskRecord, bool) {
	for current := node; !current.background; {
		owner := current.owner
		if owner == 0 {
			edge, known := s.edge(current.conversationID, o)
			if !known || edge == 0 {
				return durable.TaskRecord{}, false
			}
			owner = edge
		}
		record, ok := s.record(owner, o)
		if ok && record.State.Status != durable.TaskTerminal {
			return record, true
		}
		var above taskNode
		if ok {
			above = nodeOf(record)
		} else if settled, ok := s.settled[owner]; ok {
			above = settled
		} else {
			return durable.TaskRecord{}, false
		}
		current = above
	}
	return durable.TaskRecord{}, false
}

// known reports whether the chain above node is loaded, without walking:
// known tasks outside unloaded have a loaded chain.
func (s *scheduler) known(node nodeKey) bool {
	if !node.isConversation() {
		_, live := s.live[node.task]
		_, settled := s.settled[node.task]
		return (live || settled) && !s.unloaded.has(node.task)
	}
	owner, ok := s.edges[node.conversation]
	return ok && (owner == 0 || s.known(taskKey(owner)))
}

// edge returns the owner task of a conversation, 0 when ownerless; known is
// false while it is not loaded.
func (s *scheduler) edge(id durable.ConversationID, o *overlay) (owner durable.TaskID, known bool) {
	if o != nil {
		if owner, ok := o.edges[id]; ok {
			return owner, true
		}
	}
	owner, known = s.edges[id]
	return owner, known
}

func (s *scheduler) taskNode(id durable.TaskID, o *overlay) (taskNode, bool) {
	if record, ok := s.record(id, o); ok {
		return nodeOf(record), true
	}
	node, ok := s.settled[id]
	return node, ok
}

// upStep is one step of a walk up: an owner task with its node, a
// conversation, or an owner edge that is not loaded yet.
type upStep struct {
	task         durable.TaskID
	node         taskNode
	conversation durable.ConversationID
	unknown      bool
}

// above walks up from start: owner tasks and conversations, ending at an
// ownerless root or an edge not loaded yet. visit returns false to stop.
func (s *scheduler) above(start nodeKey, o *overlay, visit func(upStep) bool) {
	at, more := start, true
	for more {
		if !at.isConversation() {
			node, ok := s.taskNode(at.task, o)
			if !ok {
				visit(upStep{unknown: true})
				return
			}
			if !visit(upStep{task: at.task, node: node}) {
				return
			}
			at = parentOf(node)
			continue
		}
		if !visit(upStep{conversation: at.conversation}) {
			return
		}
		owner, known := s.edge(at.conversation, o)
		if !known {
			visit(upStep{unknown: true})
			return
		}
		if owner == 0 {
			more = false
		} else {
			at = taskKey(owner)
		}
	}
}

// walkOptions shape a walk down.
type walkOptions struct {
	overlay         *overlay
	crossBackground bool
	// enter rejects tasks the walk must not enter.
	enter func(durable.TaskID) bool
}

// walkDown visits the tasks below start that a walk down reaches: those below
// lists and, under an overlay, the commit's new tasks, with its candidates in
// place of committed records. live is nil for an ended task. Background tasks
// are reached but not entered, unless crossBackground, and neither are the
// tasks enter rejects. visit returns false to stop. A loop, as chains can be
// deeper than the call stack.
func (s *scheduler) walkDown(start nodeKey, options walkOptions, visit func(id durable.TaskID, node taskNode, live *durable.TaskRecord) bool) {
	var links map[nodeKey]*set[nodeKey]
	if options.overlay != nil {
		links = s.links(options.overlay)
	}
	pending := []nodeKey{start}
	for len(pending) > 0 {
		at := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, children := range []*set[nodeKey]{s.below[at], links[at]} {
			if children == nil {
				continue
			}
			for _, child := range children.values() {
				if child.isConversation() {
					pending = append(pending, child)
					continue
				}
				record, ok := s.record(child.task, options.overlay)
				var node taskNode
				var live *durable.TaskRecord
				if ok {
					node = nodeOf(record)
					if record.State.Status != durable.TaskTerminal {
						r := record
						live = &r
					}
				} else if settled, ok := s.settled[child.task]; ok {
					node = settled
				} else {
					continue
				}
				if !visit(child.task, node, live) {
					return
				}
				if (options.crossBackground || !node.background) && (options.enter == nil || options.enter(child.task)) {
					pending = append(pending, child)
				}
			}
		}
	}
}

// links returns the links walks down need for the commit's new tasks: each one
// under its parent node, then each node above it that below does not list yet,
// until one it lists, a live task (always listed), or the chain's end.
func (s *scheduler) links(o *overlay) map[nodeKey]*set[nodeKey] {
	if o.links != nil {
		return o.links
	}
	links := map[nodeKey]*set[nodeKey]{}
	for _, id := range o.order {
		record := o.tasks[id]
		if _, live := s.live[id]; live {
			continue
		}
		if _, settled := s.settled[id]; settled {
			continue
		}
		child := taskKey(id)
		parent, hasParent := parentOf(nodeOf(record)), true
		for hasParent && !(links[parent] != nil && links[parent].has(child)) {
			addTo(links, parent, child)
			if !parent.isConversation() {
				if _, live := s.live[parent.task]; live {
					break
				}
				if _, staged := o.tasks[parent.task]; staged {
					break
				}
			}
			var above nodeKey
			hasAbove := false
			if parent.isConversation() {
				owner, known := s.edge(parent.conversation, o)
				if known && owner != 0 {
					above, hasAbove = taskKey(owner), true
				}
			} else if fields, ok := s.settled[parent.task]; ok {
				above, hasAbove = parentOf(fields), true
			}
			if hasAbove && s.below[above] != nil && s.below[above].has(parent) {
				break
			}
			child = parent
			parent, hasParent = above, hasAbove
		}
	}
	o.links = links
	return links
}

// ownedLive returns the live ordinary owned work of owner (spec §5.5): the
// live non-background tasks below it that a walk down reaches without
// entering a background task; with first, only the first. The walk does not
// enter tasks in done, which have none. Owner chains must be loaded.
func (s *scheduler) ownedLive(owner durable.TaskID, o *overlay, first bool, done map[durable.TaskID]bool) []durable.TaskID {
	var found []durable.TaskID
	options := walkOptions{overlay: o}
	if done != nil {
		options.enter = func(id durable.TaskID) bool { return !done[id] }
	}
	s.walkDown(taskKey(owner), options, func(id durable.TaskID, node taskNode, live *durable.TaskRecord) bool {
		if live == nil || node.background {
			return true
		}
		found = append(found, id)
		return !first
	})
	return found
}

func (s *scheduler) hasOwnedLive(owner durable.TaskID, o *overlay, done map[durable.TaskID]bool) bool {
	return len(s.ownedLive(owner, o, true, done)) > 0
}

// scope is where ordinary ownership traversal starts: one conversation, or
// every ownerless conversation (conversation 0).
type scope struct{ conversation durable.ConversationID }

// inScope reports whether ordinary traversal from sc reaches start: walking
// up reaches the scope's conversation, or an ownerless one for the roots,
// without crossing a background owner, unless crossBackground. known is false
// while an edge is not loaded.
func (s *scheduler) inScope(start nodeKey, sc scope, crossBackground bool) (in, known bool) {
	in, known = sc.conversation == 0, true
	s.above(start, nil, func(step upStep) bool {
		switch {
		case step.unknown:
			in, known = false, false
			return false
		case step.task == 0:
			if sc.conversation != 0 && step.conversation == sc.conversation {
				in = true
				return false
			}
		case step.node.background && !crossBackground:
			in = false
			return false
		}
		return true
	})
	return in, known
}

// belowCancelled reports whether a live owner's cancellation intent reaches
// start: walking up finds an owner with intent before a background owner
// without it. Terminal owners never cascade (spec §5.4).
func (s *scheduler) belowCancelled(start nodeKey) bool {
	// Nothing cancels, the common case: no walk.
	if s.intent.len() == 0 {
		return false
	}
	cancelled := false
	s.above(start, nil, func(step upStep) bool {
		if step.unknown || step.task == 0 {
			return !step.unknown
		}
		if live, ok := s.live[step.task]; ok && cancellationIntent(live) {
			cancelled = true
			return false
		}
		return !step.node.background
	})
	return cancelled
}

// loadScopes loads the owner chains of the tasks in unloaded and, with
// queued, of every conversation with queued inputs; it returns the latter. It
// reads committed Storage directly, so it may run inside a commit callback.
func (s *scheduler) loadScopes(queued bool) ([]durable.ConversationID, error) {
	for _, id := range s.unloaded.values() {
		// An earlier load in this loop may have passed it.
		if s.unloaded.has(id) {
			if err := s.loadChain(taskKey(id), nil); err != nil {
				return nil, err
			}
		}
	}
	if !queued {
		return nil, nil
	}
	submissions, err := scanAll(func(cursor durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
		return s.session.Storage().ScanSubmissions(s.ctx, durable.SubmissionQuery{Status: durable.SubmissionQueued}, scanPageSize, cursor)
	})
	if err != nil {
		return nil, err
	}
	seen := map[durable.ConversationID]bool{}
	var conversations []durable.ConversationID
	for _, submission := range submissions {
		if submission.Type != durable.SubmissionTypeInput || seen[submission.ConversationID] {
			continue
		}
		seen[submission.ConversationID] = true
		conversations = append(conversations, submission.ConversationID)
	}
	for _, id := range conversations {
		if err := s.loadChain(conversationKey(id), nil); err != nil {
			return nil, err
		}
	}
	return conversations, nil
}

// loadChain loads the owner edges and task nodes from start up to its
// ownerless root. A known task outside unloaded ends the walk, as its chain
// is loaded; the unloaded tasks passed have a loaded chain afterwards. Loading
// a node can reveal a cancelled owner, so it makes the next reconcile derive
// cascade marks.
func (s *scheduler) loadChain(start nodeKey, o *overlay) error {
	ctx := context.WithoutCancel(s.ctx)
	var passed []durable.TaskID
	at, more := start, true
	for more {
		if !at.isConversation() {
			id := at.task
			node, ok := s.taskNode(id, nil)
			// Created in this commit: the load from its own record covers what
			// is above it.
			if !ok {
				if _, staged := o.task(id); staged {
					break
				}
			}
			if ok && !s.unloaded.has(id) {
				break
			}
			if !ok {
				record, err := s.session.Storage().Task(ctx, id)
				if err != nil {
					return err
				}
				if record == nil {
					return nil
				}
				node = nodeOf(*record)
				if record.State.Status == durable.TaskTerminal {
					s.settle(id, node)
				}
				s.scheduleReconcile()
			}
			passed = append(passed, id)
			at = parentOf(node)
			continue
		}
		owner, known := s.edge(at.conversation, o)
		if !known {
			record, err := s.session.Storage().Conversation(ctx, at.conversation)
			if err != nil {
				return err
			}
			if record != nil && record.Owner != nil {
				owner = record.Owner.TaskID
			}
			s.setEdge(at.conversation, owner)
			s.scheduleReconcile()
		}
		if owner == 0 {
			more = false
		} else {
			at = taskKey(owner)
		}
	}
	for _, id := range passed {
		s.unloaded.delete(id)
	}
	return nil
}
