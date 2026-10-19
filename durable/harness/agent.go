package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// InstructionsKey is the reserved section key of the agent's instructions.
const InstructionsKey = "instructions"

// ModelRef is a provider and model ID resolved through ai.Models.
type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// Selection is a stored or requested choice of names (or of the objects they
// name): exactly Items, in order, when Exact; otherwise an edit of a default
// by Add and Remove. Its JSON is an array when Exact and an object otherwise,
// with "add" and "remove" present when non-nil.
type Selection[T any] struct {
	Exact  bool
	Items  []T
	Add    []T
	Remove []T
}

// Exactly selects exactly items, in order.
func Exactly[T any](items ...T) Selection[T] {
	if items == nil {
		items = []T{}
	}
	return Selection[T]{Exact: true, Items: items}
}

// Edit edits the default selection: remove drops names, add appends them.
func Edit[T any](add, remove []T) Selection[T] { return Selection[T]{Add: add, Remove: remove} }

// MarshalJSON writes an array when Exact and an object otherwise.
func (s Selection[T]) MarshalJSON() ([]byte, error) {
	if s.Exact {
		items := s.Items
		if items == nil {
			items = []T{}
		}
		return json.Marshal(items)
	}
	// Written by hand: omitempty would drop an empty non-nil list, which is
	// present.
	var buf bytes.Buffer
	buf.WriteByte('{')
	write := func(key string, items []T) error {
		if items == nil {
			return nil
		}
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		value, err := json.Marshal(items)
		if err != nil {
			return err
		}
		buf.WriteString(`"` + key + `":`)
		buf.Write(value)
		return nil
	}
	if err := write("add", s.Add); err != nil {
		return nil, err
	}
	if err := write("remove", s.Remove); err != nil {
		return nil, err
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads an array as Exact and an object as an edit.
func (s *Selection[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []T
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		*s = Exactly(items...)
		return nil
	}
	var edit struct {
		Add    *[]T `json:"add"`
		Remove *[]T `json:"remove"`
	}
	if err := json.Unmarshal(trimmed, &edit); err != nil {
		return err
	}
	*s = Selection[T]{}
	if edit.Add != nil {
		s.Add = append([]T{}, *edit.Add...)
	}
	if edit.Remove != nil {
		s.Remove = append([]T{}, *edit.Remove...)
	}
	return nil
}

// AgentState is the stored choices of one conversation, in pi.agent: names,
// not objects. Unset fields follow the host.
type AgentState struct {
	Model         *ModelRef             `json:"model,omitempty"`
	ThinkingLevel ai.ModelThinkingLevel `json:"thinkingLevel,omitempty"`
	// Extensions selects exactly these extensions when Exact, or edits the
	// host default selection.
	Extensions *Selection[string] `json:"extensions,omitempty"`
	// Tools filters the selected extensions' tools, the enabled tools:
	// exactly these when Exact, or all but Remove.
	Tools *Selection[string] `json:"tools,omitempty"`
	// ModelTools filters the enabled tools the model may call, the tools
	// offered to it, and never widens:
	//   - nil: every such tool except deferred ones;
	//   - Exact: exactly these, in order, deferred ones too;
	//   - an edit: every such tool except deferred ones and those in Remove,
	//     in registry order, followed by the deferred tools in Add that are
	//     not in Remove, in Add order.
	ModelTools *Selection[string] `json:"modelTools,omitempty"`
	// Instructions render after every extension section, as the section
	// "instructions".
	Instructions *string `json:"instructions,omitempty"`
	// Cwd is a directory within the environment's file system, passed to the
	// Harness env builder.
	Cwd *string `json:"cwd,omitempty"`
}

// Change is one field of an AgentChange: left alone (the zero Change), set to
// a value (Set), or cleared (Clear).
type Change[T any] struct {
	value T
	state uint8
}

const (
	changeSet = 1 + iota
	changeClear
)

// Set replaces the stored field with value.
func Set[T any](value T) Change[T] { return Change[T]{value: value, state: changeSet} }

// Clear clears the stored field.
func Clear[T any]() Change[T] { return Change[T]{state: changeClear} }

// AgentChange is a change to pi.agent: a set field replaces the stored one, a
// cleared one is removed, a zero one changes nothing.
type AgentChange struct {
	Model         Change[ModelRef]
	ThinkingLevel Change[ai.ModelThinkingLevel]
	Extensions    Change[Selection[*Extension]]
	// Tools is Exactly(...) or an edit with only Remove.
	Tools Change[Selection[*ToolRegistration]]
	// ModelTools replaces the stored filter as a whole.
	ModelTools   Change[Selection[*ToolRegistration]]
	Instructions Change[string]
	Cwd          Change[string]
}

// Agent is a conversation's agent resolved against a registry snapshot and the
// settings.
type Agent struct {
	Model         *ModelRef
	ThinkingLevel ai.ModelThinkingLevel
	Extensions    []*Extension
	// Tools are the tools a request offers, in order: enabled, callable by the
	// model, and selected by ModelTools.
	Tools []*ToolRegistration
	// Callable are the tools nested calls resolve among, in order: enabled and
	// callable by tools.
	Callable []*ToolRegistration
	// Deferred are the enabled deferred tools the model may call and is not
	// offered yet, in registry order: what tool_search loads.
	Deferred []*ToolRegistration
	// Sections are the extension sections, then the instructions when set.
	Sections     []*PromptSection
	Instructions *string
	Cwd          *string
}

// AgentDoc is the built-in agent document; rewindable so forks start from the
// agent at their fork entry.
var AgentDoc = durable.MustDefineDoc(durable.DocDef{
	Kind:           "pi.agent",
	Version:        1,
	Scope:          durable.ScopeConversation,
	History:        "rewindable",
	Fork:           "asOf",
	Initial:        func(any) map[string]any { return map[string]any{} },
	CheckpointWhen: func(map[string]any, []delta.Op, durable.CheckpointInfo) bool { return true },
})

// Configure applies one change to the conversation's pi.agent in tx.
func Configure(tx *durable.Tx, conversationID durable.ConversationID, change AgentChange) error {
	if change.Tools.state == changeSet && !change.Tools.value.Exact && change.Tools.value.Add != nil {
		return errors.New("A tools change either lists the tools or removes some; it cannot add")
	}
	draft, err := tx.Doc(AgentDoc.Conversation(conversationID))
	if err != nil {
		return err
	}
	fields := []struct {
		key   string
		state uint8
		value func() any
	}{
		{"model", change.Model.state, func() any { return change.Model.value }},
		{"thinkingLevel", change.ThinkingLevel.state, func() any { return change.ThinkingLevel.value }},
		{"extensions", change.Extensions.state, func() any { return selectionNames(change.Extensions.value, extensionName) }},
		{"tools", change.Tools.state, func() any { return selectionNames(change.Tools.value, toolName) }},
		{"modelTools", change.ModelTools.state, func() any { return selectionNames(change.ModelTools.value, toolName) }},
		{"instructions", change.Instructions.state, func() any { return change.Instructions.value }},
		{"cwd", change.Cwd.state, func() any { return change.Cwd.value }},
	}
	for _, field := range fields {
		switch field.state {
		case changeSet:
			value, err := toValue(field.value())
			if err != nil {
				return err
			}
			if err := draft.Set(field.key, value); err != nil {
				return err
			}
		case changeClear:
			if err := draft.Delete(field.key); err != nil {
				return err
			}
		}
	}
	return nil
}

func extensionName(e *Extension) string   { return e.Name }
func toolName(t *ToolRegistration) string { return t.Name }

func selectionNames[T any](s Selection[T], name func(T) string) Selection[string] {
	names := func(items []T) []string {
		if items == nil {
			return nil
		}
		out := make([]string, len(items))
		for i, item := range items {
			out[i] = name(item)
		}
		return out
	}
	return Selection[string]{Exact: s.Exact, Items: names(s.Items), Add: names(s.Add), Remove: names(s.Remove)}
}

// createAgent is the built-in part of every commit that creates or forks a
// conversation, for pi.agent: a fork keeps its asOf copy; a new task-owned
// conversation copies the stored agent of its owner task's conversation; a
// new ownerless one starts empty.
func createAgent(tx *durable.Tx, conversation durable.ConversationRecord) error {
	if conversation.Parent != nil {
		return nil
	}
	agent, err := tx.Doc(AgentDoc.Conversation(conversation.ID))
	if err != nil {
		return err
	}
	if conversation.Owner == nil {
		return nil
	}
	owner, err := tx.Doc(AgentDoc.Conversation(conversation.Owner.ConversationID))
	if err != nil {
		return err
	}
	value, err := owner.Snapshot()
	if err != nil {
		return err
	}
	copied, _ := value.(map[string]any)
	for _, key := range sortedKeys(copied) {
		if err := agent.Set(key, copied[key]); err != nil {
			return err
		}
	}
	return nil
}

// extensionHooks are one selected extension's handlers of a task's hooks.
type extensionHooks struct {
	extension string
	handlers  any
}

// agentHooks returns the handlers of the selected extensions' hooks for a
// task name, in extension order, each with its extension's name.
func agentHooks(agent *Agent, taskName string) []extensionHooks {
	var handlers []extensionHooks
	for _, extension := range agent.Extensions {
		for _, hook := range extension.Hooks {
			if hook.Task == taskName {
				handlers = append(handlers, extensionHooks{extension: extension.Name, handlers: hook.Handlers})
			}
		}
	}
	return handlers
}

// sectionExtension is the extension whose code a section of agent runs: the
// last selected one that provides or wraps it; "" when none does.
func sectionExtension(agent *Agent, key string) string {
	found := ""
	for _, extension := range agent.Extensions {
		provides := false
		for _, section := range extension.Sections {
			provides = provides || section.Key == key
		}
		for _, wrap := range extension.Wraps {
			provides = provides || (wrap.section != "" && wrap.section == key)
		}
		if provides {
			found = extension.Name
		}
	}
	return found
}

// resolveAgent resolves an agent from its stored state (nil: every field
// unset), a registry snapshot, and resolved settings. A wrapper that fails or
// renames drops its target and is reported; a wrapper without a target does
// nothing. report gets a wrapper's failure with the name of the wrapper's
// extension.
func resolveAgent(state *AgentState, snapshot *RegistrySnapshot, settings Settings, report func(err error, extension string)) *Agent {
	if state == nil {
		state = &AgentState{}
	}
	extensions := selectExtensions(state.Extensions, snapshot, settings)

	tools := newOrdered[*ToolRegistration]()
	sections := newOrdered[*PromptSection]()
	for _, extension := range extensions {
		for _, tool := range extension.Tools {
			tools.set(tool.Name, tool)
		}
	}
	for _, extension := range extensions {
		for _, section := range extension.Sections {
			sections.set(section.Key, section)
		}
	}
	for _, extension := range extensions {
		name := extension.Name
		wrapReport := func(err error) { report(err, name) }
		for _, wrap := range extension.Wraps {
			if wrap.wrapT != nil {
				applyWrap(tools, wrap.tool, wrap.wrapT, toolName, wrapReport)
			} else if wrap.wrapS != nil {
				applyWrap(sections, wrap.section, wrap.wrapS, func(s *PromptSection) string { return s.Key }, wrapReport)
			}
		}
	}

	enabled := filterTools(tools.values(), state.Tools)
	var modelCallable, callable []*ToolRegistration
	for _, tool := range enabled {
		if tool.callableBy(CallerModel) {
			modelCallable = append(modelCallable, tool)
		}
		if tool.callableBy(CallerTools) {
			callable = append(callable, tool)
		}
	}
	offered := offeredTools(modelCallable, state.ModelTools)
	isOffered := map[*ToolRegistration]bool{}
	for _, tool := range offered {
		isOffered[tool] = true
	}
	var deferred []*ToolRegistration
	for _, tool := range modelCallable {
		if tool.Deferred && !isOffered[tool] {
			deferred = append(deferred, tool)
		}
	}

	agentSections := sections.values()
	if state.Instructions != nil {
		instructions := *state.Instructions
		agentSections = append(agentSections, &PromptSection{
			Key: InstructionsKey,
			Render: func(context.Context, PromptInput) (string, bool, error) {
				return instructions, true, nil
			},
		})
	}
	thinking := state.ThinkingLevel
	if thinking == "" {
		thinking = "off"
	}
	return &Agent{
		Model:         state.Model,
		ThinkingLevel: thinking,
		Extensions:    extensions,
		Tools:         offered,
		Callable:      callable,
		Deferred:      deferred,
		Sections:      agentSections,
		Instructions:  state.Instructions,
		Cwd:           state.Cwd,
	}
}

// offeredTools is the tools offered to the model, as AgentState.ModelTools
// describes.
func offeredTools(tools []*ToolRegistration, filter *Selection[string]) []*ToolRegistration {
	if filter != nil && filter.Exact {
		return filterTools(tools, filter)
	}
	removed := map[string]bool{}
	var add []string
	if filter != nil {
		for _, name := range filter.Remove {
			removed[name] = true
		}
		add = filter.Add
	}
	var kept []*ToolRegistration
	byName := map[string]*ToolRegistration{}
	for _, tool := range tools {
		byName[tool.Name] = tool
		if !tool.Deferred && !removed[tool.Name] {
			kept = append(kept, tool)
		}
	}
	// Loaded deferred tools follow in load order, so loading one appends to
	// the declared tools.
	seen := map[string]bool{}
	for _, name := range add {
		if seen[name] {
			continue
		}
		seen[name] = true
		if tool := byName[name]; tool != nil && tool.Deferred && !removed[name] {
			kept = append(kept, tool)
		}
	}
	return kept
}

// filterTools passes tools through a stored filter: an exact selection keeps
// exactly its names, in its order; an edit drops the removed names.
func filterTools(tools []*ToolRegistration, filter *Selection[string]) []*ToolRegistration {
	if filter == nil {
		return tools
	}
	if filter.Exact {
		byName := map[string]*ToolRegistration{}
		for _, tool := range tools {
			byName[tool.Name] = tool
		}
		var out []*ToolRegistration
		seen := map[string]bool{}
		for _, name := range filter.Items {
			if seen[name] {
				continue
			}
			seen[name] = true
			if tool := byName[name]; tool != nil {
				out = append(out, tool)
			}
		}
		return out
	}
	removed := map[string]bool{}
	for _, name := range filter.Remove {
		removed[name] = true
	}
	var out []*ToolRegistration
	for _, tool := range tools {
		if !removed[tool.Name] {
			out = append(out, tool)
		}
	}
	return out
}

// selectExtensions returns the selected installed extensions: the stored
// exact list, or the default selection edited by Add and Remove.
func selectExtensions(stored *Selection[string], snapshot *RegistrySnapshot, settings Settings) []*Extension {
	var selected []string
	if stored != nil && stored.Exact {
		selected = stored.Items
	} else {
		if settings.Extensions != nil {
			for _, extension := range settings.Extensions {
				selected = append(selected, extension.Name)
			}
		} else {
			for _, extension := range snapshot.extensions {
				selected = append(selected, extension.Name)
			}
		}
		removed := map[string]bool{}
		if stored != nil {
			selected = append(selected, stored.Add...)
			for _, name := range stored.Remove {
				removed[name] = true
			}
		}
		kept := selected[:0:0]
		for _, name := range selected {
			if !removed[name] {
				kept = append(kept, name)
			}
		}
		selected = kept
	}
	var extensions []*Extension
	seen := map[string]bool{}
	for _, name := range selected {
		if seen[name] {
			continue
		}
		seen[name] = true
		if extension := snapshot.Extension(name); extension != nil {
			extensions = append(extensions, extension)
		}
	}
	return extensions
}

func applyWrap[T any](items *ordered[T], target string, wrap func(T) (T, error), nameOf func(T) string, report func(error)) {
	item, ok := items.get(target)
	if !ok {
		return
	}
	wrapped, err := callWrapper(wrap, item)
	if err == nil && nameOf(wrapped) != target {
		err = fmt.Errorf("Wrapper renamed %s to %s", target, nameOf(wrapped))
	}
	if err != nil {
		items.delete(target)
		report(err)
		return
	}
	items.set(target, wrapped)
}

func callWrapper[T any](wrap func(T) (T, error), item T) (wrapped T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return wrap(item)
}

// RetryPolicy is the durable generation attempt retries.
type RetryPolicy struct {
	Enabled       bool
	MaxRetries    int
	BaseDelay     time.Duration
	MaxAgentDelay time.Duration
}

// CompactionPolicy is the automatic compaction thresholds; manual compaction
// ignores Enabled.
type CompactionPolicy struct {
	// Enabled is threshold and overflow compaction.
	Enabled bool
	// ReserveTokens is the room kept free for the answer: generation blocks
	// to compact above contextWindow - ReserveTokens.
	ReserveTokens int
	// KeepRecentTokens is the approximate size of the recent context a
	// summary keeps verbatim.
	KeepRecentTokens int
	// BackgroundTokens starts background compaction this far below the
	// blocking threshold; 0 disables it.
	BackgroundTokens int
}

// ProgressPolicy is how often running progress is committed. Each progress
// commit is a storage write; a host whose storage is remote can commit less
// often, so live answers and tool output appear in larger steps.
type ProgressPolicy struct {
	// PartialInterval is the minimum pause between commits of the answer
	// being generated.
	PartialInterval time.Duration
	// OutputInterval is the minimum pause between commits of running tool
	// output; large commits also pause in proportion to their size.
	OutputInterval time.Duration
}

// ConversationStreamOptions are the curated request options of generation;
// absent fields use the ai defaults. A request stores the ones in effect when
// its preparation committed, so a resend after recovery uses them unchanged.
type ConversationStreamOptions struct {
	Transport ai.Transport `json:"transport,omitempty"`
	TimeoutMs *int         `json:"timeoutMs,omitempty"`
	// MaxRetries are provider retries inside one request attempt.
	MaxRetries      *int              `json:"maxRetries,omitempty"`
	MaxRetryDelayMs *int              `json:"maxRetryDelayMs,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Metadata        map[string]any    `json:"metadata,omitempty"`
	CacheRetention  ai.CacheRetention `json:"cacheRetention,omitempty"`
	// Deferred asks a capable provider for a deferred response.
	Deferred *StreamDeferral `json:"deferred,omitempty"`
}

// StreamDeferral is pi's `deferred: true | { window }`: no Window is true.
type StreamDeferral struct {
	Window ai.DeferredWindow
}

// MarshalJSON writes true without a window, {"window": …} with one.
func (d StreamDeferral) MarshalJSON() ([]byte, error) {
	if d.Window == "" {
		return []byte("true"), nil
	}
	return json.Marshal(struct {
		Window ai.DeferredWindow `json:"window"`
	}{d.Window})
}

// UnmarshalJSON reads true or an object. false is refused: in Go no deferral
// is the absent field, and a pointer cannot decode itself to nil.
func (d *StreamDeferral) UnmarshalJSON(data []byte) error {
	var flag bool
	if json.Unmarshal(data, &flag) == nil {
		if !flag {
			return errors.New("deferred: false is written as an absent field")
		}
		*d = StreamDeferral{}
		return nil
	}
	var object struct {
		Window ai.DeferredWindow `json:"window"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	d.Window = object.Window
	return nil
}

// AfterFailedRun is what a failed run (model_error, no_model) does with the
// inputs queued behind it.
type AfterFailedRun string

const (
	// AfterFailedRunKeep leaves them queued until the next input or queued
	// write; the default.
	AfterFailedRunKeep AfterFailedRun = "keep"
	// AfterFailedRunContinue starts the next run with them, as an answer
	// would.
	AfterFailedRunContinue AfterFailedRun = "continue"
	// AfterFailedRunWithdraw settles them unanswered with reason run_failed.
	AfterFailedRunWithdraw AfterFailedRun = "withdraw"
)

// QueueMode is how many queued items of one mode a boundary places.
type QueueMode string

const (
	QueueAll         QueueMode = "all"
	QueueOneAtATime  QueueMode = "one-at-a-time"
	defaultRetention           = 10 * time.Minute
)

// HarnessSettings is the Harness-wide run policy. A nil field keeps its
// default.
type HarnessSettings struct {
	// Extensions is the default extension selection; nil is every installed
	// extension, in install order.
	Extensions []*Extension
	// Stream is the request options of every generation.
	Stream ConversationStreamOptions
	Retry  struct {
		Enabled       *bool
		MaxRetries    *int
		BaseDelay     *time.Duration
		MaxAgentDelay *time.Duration
	}
	Compaction struct {
		Enabled          *bool
		ReserveTokens    *int
		KeepRecentTokens *int
		BackgroundTokens *int
	}
	Progress struct {
		PartialInterval *time.Duration
		OutputInterval  *time.Duration
	}
	ToolExecution ToolExecutionMode
	SteeringMode  QueueMode
	FollowUpMode  QueueMode
	// ContextRetention is how long an idle conversation keeps its last
	// context read in memory, so its next run reads only newer entries. Busy
	// conversations always keep it; 0 drops it once the conversation is idle.
	ContextRetention *time.Duration
	// DocumentRetention is how long a loaded document that no watch or
	// document state observes stays in memory after its last use. Default
	// ten minutes; 0 drops one right after use; a very long one keeps them.
	DocumentRetention *time.Duration
	// AfterFailedRun is what a failed run does with the inputs queued behind
	// it; default AfterFailedRunKeep.
	AfterFailedRun AfterFailedRun
	// RestartLimit is how many restarts in a row without progress park a
	// task (spec §5.4); default DefaultRestartLimit, negative never parks
	// (pi's Infinity).
	RestartLimit *int
}

// DefaultRestartLimit is the restarts in a row after which a task is parked.
const DefaultRestartLimit = 5

// Settings is the resolved settings: every field over its built-in default.
type Settings struct {
	// Extensions is nil for every installed extension, in install order.
	Extensions        []*Extension
	Stream            ConversationStreamOptions
	Retry             RetryPolicy
	Compaction        CompactionPolicy
	Progress          ProgressPolicy
	ToolExecution     ToolExecutionMode
	SteeringMode      QueueMode
	FollowUpMode      QueueMode
	ContextRetention  time.Duration
	DocumentRetention time.Duration
	AfterFailedRun    AfterFailedRun
	// RestartLimit is negative when tasks are never parked.
	RestartLimit int
}

// The built-in defaults.
var (
	DefaultRetryPolicy      = RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelay: 2 * time.Second, MaxAgentDelay: time.Minute}
	DefaultCompactionPolicy = CompactionPolicy{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000, BackgroundTokens: 32768}
	DefaultProgressPolicy   = ProgressPolicy{PartialInterval: 100 * time.Millisecond, OutputInterval: 100 * time.Millisecond}
)

// resolveSettings resolves the host settings over the built-in defaults.
func resolveSettings(s HarnessSettings) Settings {
	pick := func(value *time.Duration, fallback time.Duration) time.Duration {
		if value != nil {
			return *value
		}
		return fallback
	}
	pickInt := func(value *int, fallback int) int {
		if value != nil {
			return *value
		}
		return fallback
	}
	pickBool := func(value *bool, fallback bool) bool {
		if value != nil {
			return *value
		}
		return fallback
	}
	orMode := func(value, fallback QueueMode) QueueMode {
		if value != "" {
			return value
		}
		return fallback
	}
	execution := s.ToolExecution
	if execution == "" {
		execution = ToolExecutionParallel
	}
	documentRetention := defaultRetention
	if s.DocumentRetention != nil && *s.DocumentRetention >= 0 {
		documentRetention = *s.DocumentRetention
	}
	afterFailedRun := s.AfterFailedRun
	if afterFailedRun == "" {
		afterFailedRun = AfterFailedRunKeep
	}
	return Settings{
		Extensions: s.Extensions,
		Stream:     s.Stream,
		Retry: RetryPolicy{
			Enabled:       pickBool(s.Retry.Enabled, DefaultRetryPolicy.Enabled),
			MaxRetries:    pickInt(s.Retry.MaxRetries, DefaultRetryPolicy.MaxRetries),
			BaseDelay:     pick(s.Retry.BaseDelay, DefaultRetryPolicy.BaseDelay),
			MaxAgentDelay: pick(s.Retry.MaxAgentDelay, DefaultRetryPolicy.MaxAgentDelay),
		},
		Compaction: CompactionPolicy{
			Enabled:          pickBool(s.Compaction.Enabled, DefaultCompactionPolicy.Enabled),
			ReserveTokens:    pickInt(s.Compaction.ReserveTokens, DefaultCompactionPolicy.ReserveTokens),
			KeepRecentTokens: pickInt(s.Compaction.KeepRecentTokens, DefaultCompactionPolicy.KeepRecentTokens),
			BackgroundTokens: pickInt(s.Compaction.BackgroundTokens, DefaultCompactionPolicy.BackgroundTokens),
		},
		Progress: ProgressPolicy{
			PartialInterval: pick(s.Progress.PartialInterval, DefaultProgressPolicy.PartialInterval),
			OutputInterval:  pick(s.Progress.OutputInterval, DefaultProgressPolicy.OutputInterval),
		},
		ToolExecution:     execution,
		SteeringMode:      orMode(s.SteeringMode, QueueOneAtATime),
		FollowUpMode:      orMode(s.FollowUpMode, QueueOneAtATime),
		ContextRetention:  pick(s.ContextRetention, defaultRetention),
		DocumentRetention: documentRetention,
		AfterFailedRun:    afterFailedRun,
		RestartLimit:      pickInt(s.RestartLimit, DefaultRestartLimit),
	}
}
