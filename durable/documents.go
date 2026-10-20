package durable

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Document definitions and addresses (pi packages/durable/src/documents.ts).
//
// pi spells a definition as one of six token types and an address as an
// overloaded argument list (owner ID, family key, seed), which the type
// checker validates. Go has neither, so a definition is one Doc whose
// semantics are checked when it is defined, and an address is a DocRef built
// explicitly — doc.Session(), doc.Conversation(id), doc.Task(id), then
// .Member(key, seed) for a family — whose misuse surfaces as the error of the
// call that uses it.

// CheckpointInfo is the stored replay state handed to a checkpoint predicate.
type CheckpointInfo struct {
	// DeltasSinceBase are the deltas already stored after the newest base,
	// not counting the change being evaluated.
	DeltasSinceBase int
}

// DocDef defines a singleton document or, with Family, a keyed document
// family. Values are chord values (chord.CopyJSON's representation).
type DocDef struct {
	// Kind is the stable persisted kind; part of the public protocol.
	Kind string
	// Version is the positive version of the stored value shape.
	Version int
	// Scope owns the document: ScopeSession, ScopeConversation or ScopeTask.
	Scope string
	// History and Fork are set for conversation documents only: History
	// "latest" with Fork "current" or "initial", or History "rewindable"
	// with Fork "asOf", "current" or "initial".
	History string
	Fork    string
	// Family makes the definition a keyed family; Initial then receives the
	// member's seed, and runs only when the member is absent.
	Family bool
	// Initial returns the first value; seed is nil for a singleton.
	Initial func(seed any) map[string]any
	// Migrate converts a value stored by an older version, when the
	// definition supports one.
	Migrate func(value map[string]any, fromVersion int) map[string]any
	// CheckpointWhen returns true to store an ordinary change as a complete
	// base instead of a delta. Required, with no default (pi 5c1736f8):
	// NeverCheckpoint keeps ordinary changes as deltas.
	CheckpointWhen func(value map[string]any, ops []delta.Op, info CheckpointInfo) bool
}

// NeverCheckpoint is the checkpoint policy that stores every ordinary change
// as a delta (pi's `() => false`): only the bases a document requires are
// written, so replay grows while the document is live.
func NeverCheckpoint(map[string]any, []delta.Op, CheckpointInfo) bool { return false }

// Doc is a validated document definition, passed explicitly to typed access.
type Doc struct{ def DocDef }

// DefineDoc validates a definition: what pi's overloads and its runtime
// version check enforce together.
func DefineDoc(def DocDef) (*Doc, error) {
	if def.Version < 1 {
		return nil, fmt.Errorf("Document %s version must be a positive integer", def.Kind)
	}
	if def.Initial == nil {
		return nil, fmt.Errorf("Document %s requires an initial value", def.Kind)
	}
	// pi requires it by type; a Go field can always be left nil.
	if def.CheckpointWhen == nil {
		return nil, fmt.Errorf("Document %s requires a checkpoint policy; NeverCheckpoint keeps ordinary changes as deltas", def.Kind)
	}
	switch def.Scope {
	case ScopeSession, ScopeTask:
		if def.History != "" || def.Fork != "" {
			return nil, fmt.Errorf("Document %s: only conversation documents declare history and fork", def.Kind)
		}
	case ScopeConversation:
		switch {
		case def.History == HistoryLatest && (def.Fork == ForkCurrent || def.Fork == ForkInitial):
		case def.History == HistoryRewindable && (def.Fork == ForkAsOf || def.Fork == ForkCurrent || def.Fork == ForkInitial):
		default:
			return nil, fmt.Errorf("Document %s: invalid conversation semantics history %q, fork %q", def.Kind, def.History, def.Fork)
		}
	default:
		return nil, fmt.Errorf("Document %s: invalid scope %q", def.Kind, def.Scope)
	}
	return &Doc{def: def}, nil
}

// MustDefineDoc is DefineDoc for package-level definitions; it panics on an
// invalid one.
func MustDefineDoc(def DocDef) *Doc {
	d, err := DefineDoc(def)
	if err != nil {
		panic(err)
	}
	return d
}

// Definition returns the definition.
func (d *Doc) Definition() DocDef { return d.def }

// DocRef is one logical document address under a definition: its owner and,
// for a family member, its key and creation seed. Build it with Doc.Session,
// Doc.Conversation or Doc.Task, then Member for a family.
type DocRef struct {
	doc     *Doc
	address DocumentAddress
	id      string
	seed    any
	member  bool
	err     error
}

func (d *Doc) ref(scope DocumentScope) DocRef {
	r := DocRef{doc: d, address: DocumentAddress{Kind: d.def.Kind, Scope: scope}}
	if d.def.Scope != scope.Kind {
		r.err = fmt.Errorf("Document %s is %s-scoped, not %s-scoped", d.def.Kind, d.def.Scope, scope.Kind)
	}
	return r
}

// Session addresses a Session-scoped document.
func (d *Doc) Session() DocRef { return d.ref(SessionScope()) }

// Conversation addresses a conversation document.
func (d *Doc) Conversation(id ConversationID) DocRef { return d.ref(ConversationScope(id)) }

// Task addresses a task document.
func (d *Doc) Task(id TaskID) DocRef { return d.ref(TaskScope(id)) }

// Member addresses one member of a family. seed is used only by Tx.Doc, and
// only when it creates the member; reads and retirement ignore it.
func (r DocRef) Member(key string, seed any) DocRef {
	if r.err == nil && !r.doc.def.Family {
		r.err = fmt.Errorf("Document %s is not a family", r.doc.def.Kind)
	}
	r.address.Key = &key
	r.seed = seed
	r.member = true
	return r
}

// resolve returns the definition and address, or the error of a misbuilt
// reference.
func (r DocRef) resolve() (DocDef, DocumentAddress, string, error) {
	if r.doc == nil {
		return DocDef{}, DocumentAddress{}, "", errors.New("Document reference has no definition")
	}
	if r.err != nil {
		return DocDef{}, DocumentAddress{}, "", r.err
	}
	if r.doc.def.Family && !r.member {
		return DocDef{}, DocumentAddress{}, "", fmt.Errorf("Document %s is a family and requires a member key", r.doc.def.Kind)
	}
	return r.doc.def, r.address, addressID(r.address), nil
}

// addressID is the stable string identity of one logical address. pi joins
// the parts as JSON; strconv.Quote keeps distinct strings distinct, which
// JSON does not for malformed UTF-8.
func addressID(address DocumentAddress) string {
	var owner int64
	switch address.Scope.Kind {
	case ScopeConversation:
		owner = int64(address.Scope.ConversationID)
	case ScopeTask:
		owner = int64(address.Scope.TaskID)
	}
	key := "-"
	if address.Key != nil {
		key = strconv.Quote(*address.Key)
	}
	return strconv.Quote(address.Kind) + " " + address.Scope.Kind + " " + strconv.FormatInt(owner, 10) + " " + key
}

func createAddress(record DocumentCreate) DocumentAddress {
	return DocumentAddress{Kind: record.Kind, Scope: record.Scope, Key: record.Key}
}

// documentCreate builds the storage create record for a new incarnation at
// an address.
func documentCreate(def DocDef, address DocumentAddress, id DocumentID) DocumentCreate {
	record := DocumentCreate{ID: id, Kind: address.Kind, Key: address.Key, Scope: address.Scope}
	if address.Scope.Kind == ScopeConversation {
		record.History = def.History
		record.Fork = def.Fork
	}
	return record
}

// checkRecordScope rejects typed access whose definition disagrees with the
// persisted scope, history or fork semantics.
func checkRecordScope(def DocDef, record DocumentCreate) error {
	if record.Scope.Kind != def.Scope ||
		(record.Scope.Kind == ScopeConversation && (record.History != def.History || record.Fork != def.Fork)) {
		return fmt.Errorf("Document %d (%s) does not match the supplied definition semantics", record.ID, record.Kind)
	}
	return nil
}

// checkRecordVersion rejects typed access to a stored version the definition
// cannot use.
func checkRecordVersion(def DocDef, record DocumentCreate, version int) error {
	if version > def.Version {
		return fmt.Errorf("Document %d (%s) has newer version %d than %d", record.ID, record.Kind, version, def.Version)
	}
	if version < def.Version && def.Migrate == nil {
		return fmt.Errorf("Document %d (%s) requires migration from version %d", record.ID, record.Kind, version)
	}
	return nil
}

// materializeDocument validates and materializes a detached stored value for
// typed access.
func materializeDocument(def DocDef, stored *StoredDocument) (map[string]any, error) {
	return materializeDocumentValue(def, stored.Record.Create(), stored.Version, stored.Value)
}

// materializeDocumentValue validates and materializes one detached value,
// also before its first persisted incarnation.
func materializeDocumentValue(def DocDef, record DocumentCreate, version int, value map[string]any) (map[string]any, error) {
	if err := checkRecordScope(def, record); err != nil {
		return nil, err
	}
	if err := checkRecordVersion(def, record, version); err != nil {
		return nil, err
	}
	if version == def.Version {
		return value, nil
	}
	migrated, err := callHook(func() map[string]any { return def.Migrate(value, version) })
	if err != nil {
		return nil, err
	}
	migrated, err = CloneValue(migrated)
	if err == nil && migrated == nil {
		err = fmt.Errorf("Document %d (%s) migration returned no JSON object", record.ID, record.Kind)
	}
	return migrated, err
}

// initialValue runs a definition's Initial and copies the result.
func initialValue(def DocDef, seed any) (map[string]any, error) {
	if !def.Family {
		seed = nil
	}
	value, err := callHook(func() map[string]any { return def.Initial(seed) })
	if err != nil {
		return nil, err
	}
	value, err = CloneValue(value)
	if err == nil && value == nil {
		err = fmt.Errorf("Document %s initial value is not a JSON object", def.Kind)
	}
	return value, err
}

// callHook runs a definition's hook; a panic is its error, as a throw is in
// pi, so it rejects the operation and leaves the Session usable.
func callHook[T any](hook func() T) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return hook(), nil
}
