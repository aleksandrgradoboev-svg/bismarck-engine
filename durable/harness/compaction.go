package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Compaction (pi packages/durable/src/harness/compaction.ts, spec §8.7): the
// built-in task that selects an old prefix of the model context, summarizes
// it, and places a summary entry whose head is the first kept entry.

// CompactionReason is why a compaction runs: Conversation.Compact, a
// threshold in generation preparation, or a context overflow.
type CompactionReason string

const (
	CompactionManual    CompactionReason = "manual"
	CompactionThreshold CompactionReason = "threshold"
	CompactionOverflow  CompactionReason = "overflow"
)

// CompactionInput is the input of a compaction task.
type CompactionInput struct {
	Reason CompactionReason `json:"reason"`
	// Instructions are an additional focus for the summary; nil is none.
	Instructions *string `json:"instructions,omitempty"`
}

// CompactionResult is EntryID of a blocking compaction's summary, or the
// SubmissionID of a conversation-owned compaction's summary write; both nil
// when nothing was compacted.
type CompactionResult struct {
	EntryID      *durable.EntryID      `json:"entryId,omitempty"`
	SubmissionID *durable.SubmissionID `json:"submissionId,omitempty"`
}

// SummaryRequest is the pinned summarization request.
type SummaryRequest struct {
	Attempt       int                       `json:"attempt"`
	Model         ModelRef                  `json:"model"`
	ThinkingLevel ai.ModelThinkingLevel     `json:"thinkingLevel"`
	StreamOptions ConversationStreamOptions `json:"streamOptions"`
	MaxTokens     int                       `json:"maxTokens"`
	// Tail is the newest entry of the context the range was selected from.
	Tail durable.EntryID `json:"tail"`
	// FirstKept is the first entry kept verbatim; the summary's head.
	FirstKept durable.EntryID `json:"firstKept"`
}

// Compaction checkpoint phases.
const (
	phaseSelect    = "select"
	phaseSummarize = "summarize"
)

// CompactionCheckpoint is pi's CompactionCheckpoint: select {}; summarize
// {…request}; retry {…request, until}.
type CompactionCheckpoint struct {
	Phase string `json:"phase"`
	*SummaryRequest
	// Until is Unix milliseconds on the Harness clock.
	Until *int64 `json:"until,omitempty"`
}

// CompactionRequest is what BeforeCompact sees: Entries are the active
// entries the summary replaces, the head marker first, and Messages their
// model context, the summarizer's source; FirstKept is the first entry kept
// verbatim.
type CompactionRequest struct {
	Reason       CompactionReason
	Entries      []durable.EntryRecord
	Messages     []ai.Message
	FirstKept    durable.EntryID
	Instructions *string
}

// CompactionDecision is a BeforeCompact hook's decision: Decline completes
// without a summary; otherwise Summary is placed as the summary.
type CompactionDecision struct {
	Decline bool
	Summary string
}

// CompactionHooks are the hooks of the built-in compaction task; register a
// *CompactionHooks with Hook(CompactionTask, …). A nil field is no hook.
type CompactionHooks struct {
	// BeforeCompact runs after range selection, before summarizing; the first
	// non-nil decision wins. An error is reported and the next hook asked.
	BeforeCompact func(ctx context.Context, request CompactionRequest, rt *TaskRuntime) (*CompactionDecision, error)
}

// toolResultMaxChars is the longest tool result text a serialized summary
// source keeps.
const toolResultMaxChars = 2000

const (
	summaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	summarySuffix = "\n</summary>"
)

const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work. If the conversation starts with an earlier summary, preserve its information and fold the newer messages into it.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

type compactionTask = Running[CompactionInput, CompactionCheckpoint]

// CompactionTask is the built-in compaction task (spec §8.7). A compaction
// the generation owns blocks it and appends its summary directly; a
// conversation-owned one places its summary through a write submission.
var CompactionTask = MustDefineTask(TaskSpec[CompactionInput, CompactionCheckpoint, CompactionResult]{
	Name:    compactionKind,
	Version: 1,
	Initial: func(CompactionInput) CompactionCheckpoint { return CompactionCheckpoint{Phase: phaseSelect} },
	Phases: map[string]func(ctx context.Context, task *compactionTask, rt *TaskRuntime) error{
		phaseSelect:    selectCompaction,
		phaseSummarize: summarizeCompaction,
		phaseRetry:     retryCompaction,
	},
	Abort: func(ctx context.Context, _ *compactionTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			if err := removeCompactionStatus(live, rt.TaskID); err != nil {
				return nil, err
			}
			return Abort(""), nil
		})
	},
})

func init() {
	builtinTasks = append(builtinTasks, CompactionTask)
}

// selectCompaction selects the range, asks BeforeCompact, and pins the
// summarization request.
func selectCompaction(ctx context.Context, task *compactionTask, rt *TaskRuntime) error {
	agent, err := rt.Agent(ctx)
	if err != nil {
		return err
	}
	settings := rt.Settings()
	if agent.Model == nil {
		return failCompactionNoModel(ctx, rt, nil)
	}
	ref := *agent.Model
	model := resolveModel(rt, ref)
	if model == nil {
		return failCompactionNoModel(ctx, rt, &ref)
	}
	policy := compactionPolicy(settings, ref)
	view, err := rt.Context(ctx, rt.ConversationID, nil)
	if err != nil {
		return err
	}
	cut, ok := selectCut(view, policy.KeepRecentTokens)
	if !ok {
		return completeCompaction(ctx, rt)
	}
	firstKept := view.Entries[cut].ID
	request := CompactionRequest{
		Reason:       task.Input.Reason,
		Entries:      append([]durable.EntryRecord(nil), view.Entries[:cut]...),
		Messages:     summarizedMessages(view, cut),
		FirstKept:    firstKept,
		Instructions: task.Input.Instructions,
	}
	var decision *CompactionDecision
	if err := EachHook(ctx, rt, func(hooks *CompactionHooks) error {
		if decision != nil || hooks.BeforeCompact == nil {
			return nil
		}
		next, err := hooks.BeforeCompact(ctx, request, rt)
		if err == nil {
			decision = next
		}
		return err
	}); err != nil {
		return err
	}
	if decision != nil && decision.Decline {
		return completeCompaction(ctx, rt)
	}
	if decision != nil {
		return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			return placeSummary(tx, rt, current, live, firstKept, decision.Summary)
		})
	}
	maxTokens := int(math.Floor(0.8 * float64(policy.ReserveTokens)))
	if model.MaxTokens > 0 && model.MaxTokens < maxTokens {
		maxTokens = model.MaxTokens
	}
	tail := firstKept
	for _, entry := range view.Entries {
		if entry.ID > tail {
			tail = entry.ID
		}
	}
	pinned := &SummaryRequest{
		Attempt:       1,
		Model:         ref,
		ThinkingLevel: agent.ThinkingLevel,
		StreamOptions: settings.Stream,
		MaxTokens:     maxTokens,
		Tail:          tail,
		FirstKept:     firstKept,
	}
	return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
		return Next(CompactionCheckpoint{Phase: phaseSummarize, SummaryRequest: pinned})
	})
}

// summarizeCompaction sends the pinned request and places the summary,
// sleeps before a retry, or fails.
func summarizeCompaction(ctx context.Context, task *compactionTask, rt *TaskRuntime) error {
	request := task.Checkpoint.SummaryRequest
	if request == nil {
		return errors.New("Compaction summarize checkpoint lacks its request")
	}
	model := resolveModel(rt, request.Model)
	if model == nil {
		return failCompactionNoModel(ctx, rt, &request.Model)
	}
	// The context at Tail is immutable, so this is the range select chose.
	view, err := rt.Context(ctx, rt.ConversationID, &request.Tail)
	if err != nil {
		return err
	}
	cut := -1
	for i, entry := range view.Entries {
		if entry.ID == request.FirstKept {
			cut = i
			break
		}
	}
	if cut < 0 {
		return fmt.Errorf("Compaction: entry %d is not in the context at %d", request.FirstKept, request.Tail)
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	messages := []ai.Message{
		ai.NewSystemText(summarizationSystemPrompt, now.UnixMilli()),
		ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: summaryPrompt(summarizedMessages(view, cut), task.Input.Instructions)}}, Timestamp: now.UnixMilli()},
	}
	forwarded := request.StreamOptions
	forwarded.Deferred = nil
	sessionID, err := ensureProviderSessionID(ctx, rt)
	if err != nil {
		return err
	}
	options := streamOptions(&forwarded, sessionID, request.ThinkingLevel)
	stream := &options.SimpleStreamOptions.StreamOptions
	stream.CacheRetention = "none"
	maxTokens := request.MaxTokens
	stream.MaxTokens = &maxTokens
	stream.Telemetry = telemetry.SpanFrom(ctx)
	message := rt.Models().CompleteSimple(ctx, model, ai.Context{Messages: messages}, options)
	// An abort mark or close: the abort invocation or the reopened task
	// handles the committed state.
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if message == nil {
		return errors.New("Summarization ended without a message")
	}
	summary, summarized := summaryText(*message)
	policy := rt.Settings().Retry
	retry := message.StopReason == ai.StopError && ai.IsRetryableAssistantError(*message) && policy.Enabled && request.Attempt <= policy.MaxRetries
	var until int64
	if retry {
		after, err := rt.Now()
		if err != nil {
			return err
		}
		until = after.UnixMilli() + retryDelay(policy, request.Attempt)
	}
	return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
		if err := recordUsage(tx, rt.ConversationID, "models", message.Provider+"/"+message.Model, message.Usage); err != nil {
			return nil, err
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if summarized {
			return placeSummary(tx, rt, current, live, request.FirstKept, summary)
		}
		if retry {
			if status := compactionStatusDraft(live, rt.TaskID); status != nil {
				if err := setDraftField(status, "retry", LiveRetry{At: until, Error: message.ErrorMessage}); err != nil {
					return nil, err
				}
			}
			return Sleep(time.UnixMilli(until), CompactionCheckpoint{Phase: phaseRetry, SummaryRequest: request, Until: &until})
		}
		text := summaryFailure(*message)
		if err := failCompactionStatus(live, rt.TaskID, text); err != nil {
			return nil, err
		}
		return modelFailure(text, "model_error"), nil
	})
}

// retryCompaction runs after the backoff's sleep; a run before until (an
// early wake, or a record of an earlier version) sleeps again.
func retryCompaction(ctx context.Context, task *compactionTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	if checkpoint.Until == nil || checkpoint.SummaryRequest == nil {
		return errors.New("Compaction retry checkpoint lacks its deadline or request")
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	if *checkpoint.Until > now.UnixMilli() {
		return rt.SleepUntil(ctx, time.UnixMilli(*checkpoint.Until), checkpoint)
	}
	request := *checkpoint.SummaryRequest
	request.Attempt++
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if status := compactionStatusDraft(live, rt.TaskID); status != nil {
			if err := status.Set("attempt", float64(request.Attempt)); err != nil {
				return nil, err
			}
			if err := status.Delete("retry"); err != nil {
				return nil, err
			}
		}
		return Next(CompactionCheckpoint{Phase: phaseSummarize, SummaryRequest: &request})
	})
}

// createCompaction creates a compaction task with its status in this commit.
// owner is the generation that waits for it (a blocking compaction); without
// one it is conversation-owned, and background unless it is manual.
func createCompaction(tx *durable.Tx, conversationID durable.ConversationID, input CompactionInput, owner *durable.TaskID) (durable.TaskID, error) {
	ownership := durable.TaskOwnership{Kind: durable.OwnershipConversation}
	if owner != nil {
		ownership = durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: *owner}
	}
	background := owner == nil && input.Reason != CompactionManual
	taskID, err := CreateTask(tx, CompactionTask, input, durable.TaskOptions{Ownership: ownership, ConversationID: conversationID, Background: background})
	if err != nil {
		return 0, err
	}
	live, err := liveDraft(tx, conversationID)
	if err != nil {
		return 0, err
	}
	return taskID, addCompactionStatus(live, CompactionStatus{TaskID: taskID, Reason: input.Reason, Blocking: owner != nil, Attempt: 1})
}

// selectCut is the index in view.Entries of the first entry a summary keeps,
// false when there is nothing to compact (spec §8.7). It walks back from the
// tail until keepRecentTokens are kept, then cuts at the first candidate at or
// after that entry: an entry whose contribution starts with a user or
// assistant message, never a tool result, and never a user entry that a
// result of the preceding assistant's calls still follows.
func selectCut(view ContextView, keepRecentTokens int) (int, bool) {
	contributions := view.Contributions
	start := 0
	if view.Head != nil {
		start = 1
	}
	var candidates []int
	for index := start; index < len(contributions); index++ {
		if isCutCandidate(contributions, index) {
			candidates = append(candidates, index)
		}
	}
	kept := 0
	cut := -1
	for index := len(contributions) - 1; index >= start; index-- {
		for _, message := range contributions[index] {
			kept += ai.EstimateMessageTokens(message)
		}
		if kept < keepRecentTokens {
			continue
		}
		for _, candidate := range candidates {
			if candidate >= index {
				cut = candidate
				break
			}
		}
		if cut < 0 && len(candidates) > 0 {
			cut = candidates[len(candidates)-1]
		}
		break
	}
	if cut < 0 {
		return 0, false
	}
	for index := start; index < cut; index++ {
		if len(contributions[index]) > 0 {
			return cut, true
		}
	}
	return 0, false
}

func isCutCandidate(contributions [][]ai.Message, index int) bool {
	if len(contributions[index]) == 0 {
		return false
	}
	switch contributions[index][0].MessageRole() {
	case ai.RoleAssistant:
		return true
	case ai.RoleUser:
	default:
		return false
	}
	// A result of the preceding assistant's calls that follows this entry,
	// before the next assistant, belongs before it.
	calls := map[string]bool{}
	for before := index - 1; before >= 0; before-- {
		messages := contributions[before]
		found := false
		for i := len(messages) - 1; i >= 0; i-- {
			if assistant, ok := assistantOf(messages[i]); ok {
				for _, content := range assistant.Content {
					if call, ok := toolCallOf(content); ok {
						calls[call.ID] = true
					}
				}
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if len(calls) == 0 {
		return true
	}
	for after := index; after < len(contributions); after++ {
		for position, message := range contributions[after] {
			if message.MessageRole() == ai.RoleAssistant && (after > index || position > 0) {
				return true
			}
			if result, ok := toolResultOf(message); ok && calls[result.ToolCallID] {
				return false
			}
		}
	}
	return true
}

// summarizedMessages are the model messages of the entries before cut, the
// head marker first, ordered like model context (spec §2.1).
func summarizedMessages(view ContextView, cut int) []ai.Message {
	var flat []sourced
	for _, messages := range view.Contributions[:cut] {
		for _, message := range messages {
			flat = append(flat, sourced{message: message})
		}
	}
	ordered := orderToolResults(flat)
	out := make([]ai.Message, len(ordered))
	for i, message := range ordered {
		out[i] = message.message
	}
	return out
}

// estimateContext is the size of a request over view followed by extra (spec
// §8.3): the usage of the newest assistant appended after the head marker,
// whose request included the marker, plus estimates of the messages after
// it; without one, estimates of every message.
func estimateContext(view ContextView, extra []ai.Message) int {
	var measured *ai.AssistantMessage
	var at *origin
	for index := len(view.Entries) - 1; index >= 0 && measured == nil; index-- {
		if view.Head != nil && view.Entries[index].ID <= view.Head.ID {
			continue
		}
		contribution := view.Contributions[index]
		for j := len(contribution) - 1; j >= 0; j-- {
			if assistant, ok := assistantOf(contribution[j]); ok && ai.CalculateContextTokens(assistant.Usage) > 0 {
				measured = &assistant
				if view.contributionOrigins != nil {
					o := view.contributionOrigins[index][j]
					at = &o
				}
				break
			}
		}
	}
	from, tokens := 0, 0
	if measured != nil {
		tokens = ai.CalculateContextTokens(measured.Usage)
		// pi's lastIndexOf: the measured message's position in the model
		// context, or none.
		for i := len(view.messageOrigins) - 1; i >= 0 && at != nil; i-- {
			if o := view.messageOrigins[i]; o != nil && *o == *at {
				from = i + 1
				break
			}
		}
	}
	for _, message := range view.Messages[from:] {
		tokens += ai.EstimateMessageTokens(message)
	}
	for _, message := range extra {
		tokens += ai.EstimateMessageTokens(message)
	}
	return tokens
}

// summaryText is the summary of a clean stop with text and no tool call;
// false when the message is not a summary.
func summaryText(message ai.AssistantMessage) (string, bool) {
	if message.StopReason != ai.StopStop {
		return "", false
	}
	var texts []string
	for _, content := range message.Content {
		if _, ok := toolCallOf(content); ok {
			return "", false
		}
		if text, ok := content.(ai.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	return text, text != ""
}

func summaryFailure(message ai.AssistantMessage) string {
	if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
		reason := message.ErrorMessage
		if reason == "" {
			reason = string(message.StopReason)
		}
		return "Summarization failed: " + reason
	}
	if message.StopReason == ai.StopLength {
		return "Summarization hit the token limit; the summary is incomplete"
	}
	for _, content := range message.Content {
		if _, ok := toolCallOf(content); ok {
			return "Summarization attempted to call a tool"
		}
	}
	return "Summarization produced no text"
}

// summaryPrompt is the summarizer's user message: the serialized
// conversation, the prompt, and any instructions.
func summaryPrompt(messages []ai.Message, instructions *string) string {
	focus := ""
	if instructions != nil {
		focus = "\n\nAdditional focus: " + *instructions
	}
	return "<conversation>\n" + serializeConversation(messages) + "\n</conversation>\n\n" + summarizationPrompt + focus
}

// serializeConversation writes messages as plain text, so the summarizer
// reads a transcript instead of continuing it. System messages are omitted.
// Thinking is omitted too: providers return summaries of the real reasoning,
// and replayed thinking is often not part of the live context (a block that
// does not match its request is dropped), so including it can make the
// request larger than the context it summarizes. Visible text, tool calls,
// and tool results record what happened.
func serializeConversation(messages []ai.Message) string {
	var parts []string
	for _, message := range messages {
		switch m := message.(type) {
		case ai.UserMessage:
			if text := summaryContentText(m.Content); text != "" {
				parts = append(parts, "[User]: "+text)
			}
		case ai.AssistantMessage:
			var texts, calls []string
			for _, content := range m.Content {
				if text, ok := content.(ai.TextContent); ok {
					texts = append(texts, text.Text)
				}
				if call, ok := toolCallOf(content); ok {
					calls = append(calls, call.Name+"("+callArguments(call)+")")
				}
			}
			if len(texts) > 0 {
				parts = append(parts, "[Assistant]: "+strings.Join(texts, "\n"))
			}
			if len(calls) > 0 {
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case ai.ToolResultMessage:
			if text := summaryContentText(m.Content); text != "" {
				parts = append(parts, "[Tool result]: "+truncateUTF16(text, toolResultMaxChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// callArguments writes a call's arguments as key=JSON pairs, in the order
// pi's object lists its keys when the call recorded it, otherwise sorted.
func callArguments(call ai.ToolCall) string {
	var pairs []string
	pair := func(key string, value any) {
		text, err := jstext.Stringify(value)
		if err != nil {
			text = "undefined"
		}
		pairs = append(pairs, key+"="+text)
	}
	switch arguments := call.OrderedArguments().(type) {
	case ai.OrderedObject:
		for _, field := range arguments {
			pair(field.Key, field.Value)
		}
	case map[string]any:
		keys := make([]string, 0, len(arguments))
		for key := range arguments {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			pair(key, arguments[key])
		}
	}
	return strings.Join(pairs, ", ")
}

func summaryContentText(content ai.ContentList) string {
	var texts []string
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// truncateUTF16 keeps the first maxChars UTF-16 code units of text, as pi's
// String.prototype.slice, and says how many it dropped.
func truncateUTF16(text string, maxChars int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= maxChars {
		return text
	}
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", string(utf16.Decode(units[:maxChars])), len(units)-maxChars)
}

// placeSummary places the summary entry and completes (spec §8.7). A
// blocking compaction, owned by its generation, appends it: the generation
// holds the run and waits. A conversation-owned one admits it as a write
// submission, placed at once when idle, otherwise at the next boundary, or
// settled stale.
// REMINDER: nothing else may append to a busy conversation, so every
// non-blocking summary goes through admission.
func placeSummary(tx *durable.Tx, rt *TaskRuntime, current durable.TaskRecord, live *delta.Draft, firstKept durable.EntryID, summary string) (*durable.TaskState, error) {
	if err := removeCompactionStatus(live, rt.TaskID); err != nil {
		return nil, err
	}
	if err := live.Delete("compactionFailure"); err != nil {
		return nil, err
	}
	var input CompactionInput
	if err := json.Unmarshal(current.Input, &input); err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]CompactionReason{"reason": input.Reason})
	if err != nil {
		return nil, err
	}
	now := rt.s.now().UnixMilli()
	head := firstKept
	entry := durable.EntryDraft{
		Kind:  durable.CompactionEntry.Kind,
		Head:  &head,
		Model: []ai.Message{ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: summaryPrefix + summary + summarySuffix}}, Timestamp: now}},
		Data:  data,
	}
	var result CompactionResult
	if current.Owner == nil {
		admitted, err := admitSubmission(tx, rt.ConversationID, SubmissionDraft{
			Type:      durable.SubmissionTypeWrite,
			RequestID: fmt.Sprintf("compaction:%d", rt.TaskID),
			Write:     &entry,
		}, now, rt.Settings(), nil)
		if err != nil {
			return nil, err
		}
		result.SubmissionID = &admitted.id
	} else {
		appended, err := tx.AppendEntry(rt.ConversationID, entry)
		if err != nil {
			return nil, err
		}
		result.EntryID = &appended.ID
	}
	return Complete(result)
}

// completeCompaction removes the status and completes without a summary.
func completeCompaction(ctx context.Context, rt *TaskRuntime) error {
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := removeCompactionStatus(live, rt.TaskID); err != nil {
			return nil, err
		}
		return Complete(CompactionResult{})
	})
}

func failCompactionNoModel(ctx context.Context, rt *TaskRuntime, ref *ModelRef) error {
	message := "No model is configured"
	if ref != nil {
		message = fmt.Sprintf("Model %s/%s is not available", ref.Provider, ref.ModelID)
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := failCompactionStatus(live, rt.TaskID, message); err != nil {
			return nil, err
		}
		return modelFailure(message, "no_model"), nil
	})
}
