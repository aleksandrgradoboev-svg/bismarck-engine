package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// GenerationInput is the input of a generation: none; the run's inputs live
// in pi.live run.
type GenerationInput struct{}

// GenerationResult is the answer entry of a completed generation.
type GenerationResult struct {
	EntryID durable.EntryID `json:"entryId"`
}

// Generation checkpoint phases.
const (
	phasePrepare = "prepare"
	phaseRequest = "request"
	phaseRetry   = "retry"
	phasePoll    = "poll"
	phaseTools   = "tools"
)

// GenerationCheckpoint is pi's GenerationCheckpoint, one struct for every
// phase: prepare {attempt, compacted?, overflow?, threshold?}; request
// {attempt, compacted?, model, thinkingLevel, streamOptions, cutoff}; retry
// {attempt, compacted?, until}; poll {attempt, compacted?, model, cutoff,
// handle, pollAt}; tools {assistant, tools, pending}.
type GenerationCheckpoint struct {
	Phase   string `json:"phase"`
	Attempt int    `json:"attempt,omitempty"`
	// Compacted is the blocking compaction this generation waited for; it
	// starts no other compaction (spec §8.3).
	Compacted *durable.TaskID `json:"compacted,omitempty"`
	// Overflow is the error text of the overflow that started Compacted;
	// checked once when prepare resumes.
	Overflow string `json:"overflow,omitempty"`
	// Threshold is set when the blocking threshold started Compacted; its
	// outcome is checked once when prepare resumes.
	Threshold bool      `json:"threshold,omitempty"`
	Model     *ModelRef `json:"model,omitempty"`
	// ThinkingLevel is the request's; "off" sends no reasoning option.
	ThinkingLevel ai.ModelThinkingLevel `json:"thinkingLevel,omitempty"`
	// StreamOptions are the settings' request options when preparation
	// committed; a resend after recovery uses them unchanged.
	StreamOptions *ConversationStreamOptions `json:"streamOptions,omitempty"`
	// Cutoff is the newest entry included in the request.
	Cutoff *durable.EntryID `json:"cutoff,omitempty"`
	// Until and PollAt are Unix milliseconds on the Harness clock.
	Until     *int64             `json:"until,omitempty"`
	Handle    *ai.DeferredHandle `json:"handle,omitempty"`
	PollAt    *int64             `json:"pollAt,omitempty"`
	Assistant *durable.EntryID   `json:"assistant,omitempty"`
	Tools     []durable.TaskID   `json:"tools,omitempty"`
	Pending   []string           `json:"pending,omitempty"`
}

// GenerationHooks are the hooks of the built-in generation task; register a
// *GenerationHooks with Hook(GenerationTask, …). A nil field is no hook.
type GenerationHooks struct {
	// BeforeRequest runs before every request attempt, including recovery;
	// non-nil messages replace the request's for that request only.
	BeforeRequest func(ctx context.Context, messages []ai.Message, rt *TaskRuntime) ([]ai.Message, error)
	// AfterResponse sees every terminal provider message, before
	// classification.
	AfterResponse func(ctx context.Context, message ai.AssistantMessage, rt *TaskRuntime) error
	// OnYield sees a final answer; the first non-nil continuation appends a
	// user message and continues the run.
	OnYield func(ctx context.Context, answer ai.AssistantMessage, rt *TaskRuntime) (*UserInput, error)
	// AfterTools runs after every tool of a round is terminal, with the
	// round's result entries in call order.
	AfterTools func(ctx context.Context, assistant durable.EntryID, results []durable.EntryID, rt *TaskRuntime) error
}

type generationTask = Running[GenerationInput, GenerationCheckpoint]

// GenerationTask is the built-in generation task: it prepares the positional
// system prompt and tool loadout, requests the model, and classifies the
// response. The run's inputs live in pi.live run.
var GenerationTask Task

// The definition refers to itself (an answer that continues the run creates
// the next generation), so it is built in init rather than as a package
// variable's initializer.
func init() {
	GenerationTask = defineGeneration()
	builtinTasks = append(builtinTasks, GenerationTask)
}

func defineGeneration() Task {
	return MustDefineTask(TaskSpec[GenerationInput, GenerationCheckpoint, GenerationResult]{
		Name:    "pi.generation",
		Version: 1,
		Initial: func(GenerationInput) GenerationCheckpoint {
			return GenerationCheckpoint{Phase: phasePrepare, Attempt: 1}
		},
		Phases: map[string]func(ctx context.Context, task *generationTask, rt *TaskRuntime) error{
			phasePrepare: prepareGeneration,
			phaseRequest: requestGeneration,
			phaseRetry:   retryGeneration,
			phasePoll:    pollGeneration,
			phaseTools:   toolsGeneration,
		},
		Abort: abortGeneration,
	})
}

// prepareGeneration renders the system prompt and tool loadout and appends
// the positional pi.system entries they need, then moves to request. The
// agent and settings resolved here are fixed for this request. Only the
// Harness writes model context to a busy conversation, so the context read
// here is still current at the commit; display-only entries may land
// meanwhile.
func prepareGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	agent, err := rt.Agent(ctx)
	if err != nil {
		return err
	}
	settings := rt.Settings()
	if agent.Model == nil {
		return failNoModel(ctx, rt, nil)
	}
	resolved := resolveModel(rt, *agent.Model)
	if resolved == nil {
		return failNoModel(ctx, rt, agent.Model)
	}
	checkpoint := task.Checkpoint
	if checkpoint.Compacted != nil && checkpoint.Overflow != "" {
		outcomes, err := rt.Outcomes(ctx, []durable.TaskID{*checkpoint.Compacted})
		if err != nil {
			return err
		}
		var result CompactionResult
		if outcomes[0].Status == durable.OutcomeCompleted {
			if err := json.Unmarshal(outcomes[0].Result, &result); err != nil {
				return err
			}
		}
		if result.EntryID == nil {
			return failModelError(ctx, rt, checkpoint.Overflow)
		}
	}
	// A threshold compaction that found nothing to compact or was declined
	// leaves a request that may still fit. One that failed left a context it
	// judged too large: sending it would at best leave no room for the answer.
	if checkpoint.Compacted != nil && checkpoint.Threshold {
		outcomes, err := rt.Outcomes(ctx, []durable.TaskID{*checkpoint.Compacted})
		if err != nil {
			return err
		}
		if outcomes[0].Status != durable.OutcomeCompleted {
			return failModelError(ctx, rt, compactionFailureText(outcomes[0]))
		}
	}
	view, err := rt.Context(ctx, rt.ConversationID, nil)
	if err != nil {
		return err
	}
	shown := replaySections(view.Messages)
	environment, err := rt.Env(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		if reportErr := rt.Report(err, ReportFrom{Callback: durable.CallbackEnv}); reportErr != nil {
			return reportErr
		}
		environment = nil
	}
	shownMap := map[string]string{}
	for _, key := range shown.keys {
		shownMap[key], _ = shown.get(key)
	}
	input := PromptInput{ConversationID: rt.ConversationID, Agent: agent, Env: environment, Shown: shownMap, Read: rt}
	desired, err := renderSections(ctx, agent.Sections, input, shown, func(err error, section *PromptSection) {
		_ = rt.Report(err, ReportFrom{Extension: sectionExtension(agent, section.Key)})
	})
	if err != nil {
		return err
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	entries := planSystemEntries(view, desired, offeredAITools(agent.Tools), now.UnixMilli())
	threshold := ""
	if checkpoint.Compacted == nil {
		threshold = thresholdCompaction(view, entries, resolved.ContextWindow, compactionPolicy(settings, *agent.Model))
	}
	if threshold == "blocking" {
		// Compact first and prepare again; the transcript is unchanged until
		// the compaction appends.
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			owner := rt.TaskID
			child, err := createCompaction(tx, rt.ConversationID, CompactionInput{Reason: CompactionThreshold}, &owner)
			if err != nil {
				return nil, err
			}
			return Wait([]durable.TaskID{child}, durable.JoinAllSettled, GenerationCheckpoint{Phase: phasePrepare, Attempt: checkpoint.Attempt, Compacted: &child, Threshold: true})
		})
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		page, err := tx.ScanEntries(durable.EntryQuery{ConversationID: rt.ConversationID}, 1, nil)
		if err != nil {
			return nil, err
		}
		var cutoff *durable.EntryID
		if len(page.Items) > 0 {
			id := page.Items[0].ID
			cutoff = &id
		}
		for _, entry := range entries {
			appended, err := tx.AppendEntry(rt.ConversationID, entry)
			if err != nil {
				return nil, err
			}
			id := appended.ID
			cutoff = &id
		}
		if cutoff == nil {
			return nil, fmt.Errorf("Conversation %d has no entries to send", rt.ConversationID)
		}
		// Checked in this commit, so a compaction admitted during preparation
		// counts.
		if threshold == "background" {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			if !live.Has("compactions") {
				if _, err := createCompaction(tx, rt.ConversationID, CompactionInput{Reason: CompactionThreshold}, nil); err != nil {
					return nil, err
				}
			}
		}
		stream := settings.Stream
		model := *agent.Model
		return Next(GenerationCheckpoint{
			Phase:         phaseRequest,
			Attempt:       checkpoint.Attempt,
			Compacted:     checkpoint.Compacted,
			Model:         &model,
			ThinkingLevel: agent.ThinkingLevel,
			StreamOptions: &stream,
			Cutoff:        cutoff,
		})
	})
}

// offeredAITools are the tool declarations of the offered registrations.
func offeredAITools(tools []*ToolRegistration) []ai.Tool {
	out := make([]ai.Tool, len(tools))
	for i, tool := range tools {
		out[i] = tool.Tool
	}
	return out
}

// resolveModel resolves a model reference through the Harness model access;
// nil when unavailable.
func resolveModel(rt *TaskRuntime, ref ModelRef) *ai.Model {
	models := rt.Models()
	if models == nil {
		return nil
	}
	return models.GetModel(ref.Provider, ref.ModelID)
}

// requestGeneration sends the prepared request and classifies the answer.
func requestGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	if checkpoint.Model == nil || checkpoint.Cutoff == nil {
		return errors.New("Generation request checkpoint lacks its model or cutoff")
	}
	if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := convertPartial(tx, live, rt.ConversationID); err != nil {
			return nil, err
		}
		return nil, setDraftField(live, "generation", LiveGeneration{Attempt: checkpoint.Attempt})
	}); err != nil {
		return err
	}
	model := resolveModel(rt, *checkpoint.Model)
	if model == nil {
		return failNoModel(ctx, rt, checkpoint.Model)
	}
	view, err := rt.Context(ctx, rt.ConversationID, checkpoint.Cutoff)
	if err != nil {
		return err
	}
	messages := view.Messages
	if err := EachHook(ctx, rt, func(hooks *GenerationHooks) error {
		if hooks.BeforeRequest == nil {
			return nil
		}
		replaced, err := hooks.BeforeRequest(ctx, messages, rt)
		if err == nil && replaced != nil {
			messages = replaced
		}
		return err
	}); err != nil {
		return err
	}
	sessionID, err := ensureProviderSessionID(ctx, rt)
	if err != nil {
		return err
	}
	options := streamOptions(checkpoint.StreamOptions, sessionID, checkpoint.ThinkingLevel)
	// The provider request is traced under the phase span.
	options.SimpleStreamOptions.StreamOptions.Telemetry = telemetry.SpanFrom(ctx)
	message, err := streamResponse(ctx, rt, model, messages, options, checkpoint.Attempt)
	if err != nil {
		return err
	}
	return classify(ctx, rt, checkpoint, message)
}

// defaultPollAfter is the wait before polling a deferred response whose
// provider did not say.
const defaultPollAfter = 5000 // milliseconds

// retryGeneration runs after the backoff's sleep; a run before until (an
// early wake, or a record of an earlier version) sleeps again.
func retryGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	if checkpoint.Until == nil {
		return errors.New("Generation retry checkpoint lacks its deadline")
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	if *checkpoint.Until > now.UnixMilli() {
		return rt.SleepUntil(ctx, time.UnixMilli(*checkpoint.Until), checkpoint)
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := setDraftField(live, "generation", LiveGeneration{Attempt: checkpoint.Attempt + 1}); err != nil {
			return nil, err
		}
		return Next(GenerationCheckpoint{Phase: phasePrepare, Attempt: checkpoint.Attempt + 1, Compacted: checkpoint.Compacted})
	})
}

// pollGeneration runs after the sleep until pollAt; a run before it (an early
// wake, or a record of an earlier version) sleeps again.
func pollGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	if checkpoint.PollAt == nil || checkpoint.Model == nil || checkpoint.Cutoff == nil || checkpoint.Handle == nil {
		return errors.New("Generation poll checkpoint lacks its deadline, model, cutoff, or handle")
	}
	now, err := rt.Now()
	if err != nil {
		return err
	}
	if *checkpoint.PollAt > now.UnixMilli() {
		return rt.SleepUntil(ctx, time.UnixMilli(*checkpoint.PollAt), checkpoint)
	}
	model := resolveModel(rt, *checkpoint.Model)
	if model == nil {
		return failNoModel(ctx, rt, checkpoint.Model)
	}
	options := &ai.ModelsDeferredFetchOptions{}
	options.Telemetry = telemetry.SpanFrom(ctx)
	message := rt.Models().FetchDeferred(ctx, model, *checkpoint.Handle, options)
	if message == nil {
		return errors.New("Deferred fetch ended without a message")
	}
	return classify(ctx, rt, checkpoint, message)
}

// retryDelay is the backoff before the attempt after attempt.
func retryDelay(policy RetryPolicy, attempt int) int64 {
	maxDelay := int(policy.MaxAgentDelay.Milliseconds())
	return int64(ai.RetryDelayMs(ai.RetryPolicy{
		Enabled:         policy.Enabled,
		MaxRetries:      policy.MaxRetries,
		BaseDelayMs:     int(policy.BaseDelay.Milliseconds()),
		MaxAgentDelayMs: &maxDelay,
	}, attempt))
}

// streamOptions builds the request options of a generation.
func streamOptions(stored *ConversationStreamOptions, sessionID string, thinking ai.ModelThinkingLevel) *ai.ModelsSimpleStreamOptions {
	options := &ai.ModelsSimpleStreamOptions{}
	if stored != nil {
		s := &options.SimpleStreamOptions.StreamOptions
		s.Transport = stored.Transport
		if stored.TimeoutMs != nil {
			s.TimeoutMs = *stored.TimeoutMs
		}
		if stored.MaxRetries != nil {
			s.MaxRetries = *stored.MaxRetries
		}
		if stored.MaxRetryDelayMs != nil {
			delay := *stored.MaxRetryDelayMs
			s.MaxRetryDelayMs = &delay
		}
		if stored.Headers != nil {
			s.Headers = ai.ProviderHeaders{}
			for name, value := range stored.Headers {
				value := value
				s.Headers[name] = &value
			}
		}
		s.Metadata = stored.Metadata
		s.CacheRetention = stored.CacheRetention
		if stored.Deferred != nil {
			options.Deferred = &ai.DeferredRequest{Window: stored.Deferred.Window}
		}
	}
	options.SessionID = sessionID
	if thinking != "" && thinking != "off" {
		options.Reasoning = ai.ThinkingLevel(thinking)
	}
	return options
}

// streamResponse streams one request and returns the terminal message.
// Partials commit at most every Progress.PartialInterval with one commit in
// flight; the end stops the throttle and awaits that commit, so no stale
// partial lands after the outcome.
func streamResponse(ctx context.Context, rt *TaskRuntime, model *ai.Model, messages []ai.Message, options *ai.ModelsSimpleStreamOptions, attempt int) (*ai.AssistantMessage, error) {
	interval := rt.Settings().Progress.PartialInterval
	var (
		mu       sync.Mutex
		pending  *ai.AssistantMessage
		disarm   func()
		inFlight chan struct{}
		stopped  bool
	)
	var flush func()
	flush = func() {
		mu.Lock()
		disarm = nil
		partial := pending
		pending = nil
		if partial == nil || stopped {
			mu.Unlock()
			return
		}
		done := make(chan struct{})
		inFlight = done
		mu.Unlock()
		go func() {
			defer func() {
				mu.Lock()
				inFlight = nil
				if pending != nil && !stopped {
					disarm = rt.SetTimer(flush, interval)
				}
				mu.Unlock()
				close(done)
			}()
			value, err := toValue(partial)
			if err == nil {
				err = rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
					live, err := liveDraft(tx, rt.ConversationID)
					if err != nil {
						return nil, err
					}
					generation := live.At("generation")
					if generation == nil {
						if err := setDraftField(live, "generation", LiveGeneration{Attempt: attempt}); err != nil {
							return nil, err
						}
						generation = live.At("generation")
					}
					return nil, assignJSON(generation, "message", value)
				})
			}
			// Failures after an abort mark or close are expected; the
			// committed state stays consistent.
			if err != nil && ctx.Err() == nil {
				_ = rt.Report(err)
			}
		}()
	}
	stop := func() {
		mu.Lock()
		stopped = true
		if disarm != nil {
			disarm()
			disarm = nil
		}
		done := inFlight
		mu.Unlock()
		if done != nil {
			<-done
		}
	}
	defer stop()
	events := rt.Models().StreamSimple(ctx, model, ai.Context{Messages: append([]ai.Message(nil), messages...)}, options)
	for event := range events.Events() {
		// A partial without content, such as the opening start event, shows
		// nothing; a deferred response never gets past it, so it never leaves
		// a partial.
		if event.Type == ai.EventDone || event.Type == ai.EventError || event.Partial == nil || len(event.Partial.Content) == 0 {
			continue
		}
		mu.Lock()
		pending = event.Partial
		if disarm == nil && inFlight == nil {
			disarm = rt.SetTimer(flush, interval)
		}
		mu.Unlock()
	}
	message := events.Result()
	if message == nil {
		return nil, errors.New("Model stream ended without a message")
	}
	return message, nil
}

// classify classifies a terminal provider message in one commit that also
// clears the partial.
func classify(ctx context.Context, rt *TaskRuntime, request GenerationCheckpoint, message *ai.AssistantMessage) error {
	// An abort mark or close: the abort invocation or the reopened run handles
	// the committed state.
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if message.StopReason == ai.StopDeferred && message.Deferred != nil {
		handle := *message.Deferred
		now, err := rt.Now()
		if err != nil {
			return err
		}
		after := handle.PollAfterMs
		if after == 0 {
			after = defaultPollAfter
		}
		pollAt := now.UnixMilli() + after
		// A still deferred result of a poll polls strictly later.
		if request.PollAt != nil && *request.PollAt+1 > pollAt {
			pollAt = *request.PollAt + 1
		}
		return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
			live, err := liveDraft(tx, rt.ConversationID)
			if err != nil {
				return nil, err
			}
			if err := setDraftField(live, "generation", LiveGeneration{Attempt: request.Attempt, Deferred: &LiveDeferred{PollAt: pollAt}}); err != nil {
				return nil, err
			}
			return Sleep(time.UnixMilli(pollAt), GenerationCheckpoint{
				Phase:     phasePoll,
				Attempt:   request.Attempt,
				Compacted: request.Compacted,
				Model:     request.Model,
				Cutoff:    request.Cutoff,
				Handle:    &handle,
				PollAt:    &pollAt,
			})
		})
	}
	if err := EachHook(ctx, rt, func(hooks *GenerationHooks) error {
		if hooks.AfterResponse == nil {
			return nil
		}
		return hooks.AfterResponse(ctx, *message, rt)
	}); err != nil {
		return err
	}
	var calls []ai.ToolCall
	for _, content := range message.Content {
		if call, ok := toolCallOf(content); ok {
			calls = append(calls, call)
		}
	}
	if message.StopReason == ai.StopToolUse && len(calls) > 0 {
		return startToolRound(ctx, rt, request, *message, calls)
	}
	if message.StopReason == ai.StopStop || message.StopReason == ai.StopLength || message.StopReason == ai.StopToolUse {
		return answer(ctx, rt, *message)
	}
	// The retry and compaction policies govern the next attempt, so they are
	// read now rather than pinned at preparation.
	settings := rt.Settings()
	overflow := message.StopReason == ai.StopError && ai.IsContextOverflow(*message, 0)
	if request.Model == nil || request.Cutoff == nil {
		return errors.New("Generation request checkpoint lacks its model or cutoff")
	}
	compaction := compactionPolicy(settings, *request.Model)
	if overflow && request.Compacted == nil && compaction.Enabled {
		view, err := rt.Context(ctx, rt.ConversationID, request.Cutoff)
		if err != nil {
			return err
		}
		if _, ok := selectCut(view, compaction.KeepRecentTokens); ok {
			text := message.ErrorMessage
			if text == "" {
				text = "Context overflow"
			}
			return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
				live, err := liveDraft(tx, rt.ConversationID)
				if err != nil {
					return nil, err
				}
				if _, err := appendAssistant(tx, rt.ConversationID, *message); err != nil {
					return nil, err
				}
				if err := live.Delete("generation"); err != nil {
					return nil, err
				}
				owner := rt.TaskID
				child, err := createCompaction(tx, rt.ConversationID, CompactionInput{Reason: CompactionOverflow}, &owner)
				if err != nil {
					return nil, err
				}
				return Wait([]durable.TaskID{child}, durable.JoinAllSettled, GenerationCheckpoint{Phase: phasePrepare, Attempt: request.Attempt, Compacted: &child, Overflow: text})
			})
		}
	}
	policy := settings.Retry
	// An overflow is never retried: only a compaction can make the next
	// request fit.
	retry := message.StopReason == ai.StopError && !overflow && ai.IsRetryableAssistantError(*message) && policy.Enabled && request.Attempt <= policy.MaxRetries
	var until int64
	if retry {
		now, err := rt.Now()
		if err != nil {
			return err
		}
		until = now.UnixMilli() + retryDelay(policy, request.Attempt)
	}
	text := message.ErrorMessage
	if text == "" {
		text = fmt.Sprintf("Model response ended with stop reason %s", message.StopReason)
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		// Read before the first table write; only a failure uses it.
		var failure failure
		if !retry {
			var err error
			if failure, err = prepareFailure(tx, rt); err != nil {
				return nil, err
			}
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if _, err := appendAssistant(tx, rt.ConversationID, *message); err != nil {
			return nil, err
		}
		if retry {
			generation := LiveGeneration{Attempt: request.Attempt, Retry: &LiveRetry{At: until, Error: message.ErrorMessage}}
			if err := setDraftField(live, "generation", generation); err != nil {
				return nil, err
			}
			return Sleep(time.UnixMilli(until), GenerationCheckpoint{Phase: phaseRetry, Attempt: request.Attempt, Compacted: request.Compacted, Until: &until})
		}
		detail, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		if err := endFailedRun(tx, rt, live, failure, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "model_error", Detail: detail}); err != nil {
			return nil, err
		}
		return modelFailure(text, "model_error"), nil
	})
}

// answer handles a final answer; the final boundary places queued items (spec
// §6). The first OnYield continuation appends a user message and hands the
// run to a successor generation, but only when the boundary selected no user
// item and no reset. Otherwise the run's inputs settle done, and selected
// user items start the next run.
func answer(ctx context.Context, rt *TaskRuntime, message ai.AssistantMessage) error {
	var continuation *UserInput
	if err := EachHook(ctx, rt, func(hooks *GenerationHooks) error {
		if continuation != nil || hooks.OnYield == nil {
			return nil
		}
		next, err := hooks.OnYield(ctx, message, rt)
		if err == nil {
			continuation = next
		}
		return err
	}); err != nil {
		return err
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		// Queue modes are read on the Session line, when the boundary is
		// decided: a change while this commit waits behind another applies.
		b, err := prepareBoundary(tx, rt.ConversationID, rt.Settings())
		if err != nil {
			return nil, err
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		entry, err := appendAssistant(tx, rt.ConversationID, message)
		if err != nil {
			return nil, err
		}
		result, err := Complete(GenerationResult{EntryID: entry.ID})
		if err != nil {
			return nil, err
		}
		now := rt.s.now().UnixMilli()
		placed, err := applyBoundary(tx, b, true, now)
		if err != nil {
			return nil, err
		}
		if continuation != nil && len(placed.users) == 0 && !placed.reset {
			user := continuation.userMessage(now)
			if _, err := tx.AppendEntry(rt.ConversationID, durable.EntryDraft{Kind: durable.UserEntry.Kind, Model: []ai.Message{user}}); err != nil {
				return nil, err
			}
			next, err := createGeneration(tx, rt.ConversationID, nil)
			if err != nil {
				return nil, err
			}
			if err := handOver(live, rt.TaskID, next); err != nil {
				return nil, err
			}
			return result, live.Delete("generation")
		}
		if err := endRun(tx, live, rt.TaskID, durable.SubmissionSettlement{Status: durable.SubmissionDone, Answer: entry.ID}); err != nil {
			return nil, err
		}
		if len(placed.users) > 0 {
			if err := startRun(tx, rt.ConversationID, live, placed.users); err != nil {
				return nil, err
			}
		}
		return result, nil
	})
}

// abortGeneration ends an abort-marked generation: a committed partial
// becomes an aborted assistant entry, and the run's inputs settle unanswered
// "aborted".
func abortGeneration(ctx context.Context, task *generationTask, rt *TaskRuntime) error {
	checkpoint := task.Checkpoint
	// Runs after the round's tool tasks are terminal; calls never started
	// get aborted results (spec §8.5).
	var unstarted []ai.ToolCall
	if checkpoint.Phase == phaseTools && checkpoint.Assistant != nil {
		var err error
		if unstarted, err = unstartedCalls(ctx, rt, *checkpoint.Assistant, checkpoint.Pending); err != nil {
			return err
		}
	}
	if checkpoint.Phase == phasePoll && checkpoint.Model != nil && checkpoint.Handle != nil {
		if model := resolveModel(rt, *checkpoint.Model); model != nil {
			options := &ai.ModelsDeferredCancelOptions{}
			options.Telemetry = telemetry.SpanFrom(ctx)
			if err := rt.Models().CancelDeferred(ctx, model, *checkpoint.Handle, options); err != nil {
				if reportErr := rt.Report(err, ReportFrom{Callback: durable.CallbackModels}); reportErr != nil {
					return reportErr
				}
			}
		}
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := convertPartial(tx, live, rt.ConversationID); err != nil {
			return nil, err
		}
		now := rt.s.now().UnixMilli()
		for _, call := range unstarted {
			result := harnessError("aborted", fmt.Sprintf("Tool %s was aborted", call.Name))
			if _, err := appendToolResult(tx, rt.ConversationID, call, result, now, nil); err != nil {
				return nil, err
			}
		}
		if err := endRun(tx, live, rt.TaskID, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "aborted"}); err != nil {
			return nil, err
		}
		return Abort(""), nil
	})
}

// modelFailure is a failed outcome with message and detail {reason}.
func modelFailure(message, reason string) *durable.TaskState {
	state := Fail(message)
	state.Outcome.Error.Detail = json.RawMessage(fmt.Sprintf(`{"reason":%q}`, reason))
	return state
}

// failNoModel settles the run's inputs unanswered "no_model" and fails.
func failNoModel(ctx context.Context, rt *TaskRuntime, ref *ModelRef) error {
	message := "No model is configured"
	if ref != nil {
		message = fmt.Sprintf("Model %s/%s is not available", ref.Provider, ref.ModelID)
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		failure, err := prepareFailure(tx, rt)
		if err != nil {
			return nil, err
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := endFailedRun(tx, rt, live, failure, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "no_model"}); err != nil {
			return nil, err
		}
		return modelFailure(message, "no_model"), nil
	})
}

// failModelError settles the run's inputs unanswered "model_error" with text
// and fails with it.
func failModelError(ctx context.Context, rt *TaskRuntime, text string) error {
	detail, err := json.Marshal(text)
	if err != nil {
		return err
	}
	return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		failure, err := prepareFailure(tx, rt)
		if err != nil {
			return nil, err
		}
		live, err := liveDraft(tx, rt.ConversationID)
		if err != nil {
			return nil, err
		}
		if err := endFailedRun(tx, rt, live, failure, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "model_error", Detail: detail}); err != nil {
			return nil, err
		}
		return modelFailure(text, "model_error"), nil
	})
}

// thresholdCompaction is which threshold compaction preparation starts
// before its request (spec §8.3): "blocking" above contextWindow -
// ReserveTokens, "background" above the background threshold, and only when
// range selection finds a cut; "" for none. The caller starts a background
// one only while no compaction is listed.
func thresholdCompaction(view ContextView, planned []durable.EntryDraft, contextWindow int, policy CompactionPolicy) string {
	if !policy.Enabled || contextWindow <= 0 {
		return ""
	}
	var extra []ai.Message
	for _, entry := range planned {
		extra = append(extra, entry.Model...)
	}
	tokens := estimateContext(view, extra)
	blocking := contextWindow - policy.ReserveTokens
	background := blocking - policy.BackgroundTokens
	over := ""
	switch {
	case tokens > blocking:
		over = "blocking"
	case policy.BackgroundTokens > 0 && tokens > background:
		over = "background"
	}
	if over == "" {
		return ""
	}
	if _, ok := selectCut(view, policy.KeepRecentTokens); !ok {
		return ""
	}
	return over
}

// compactionFailureText is the error text of a generation whose blocking
// compaction ended without completing.
func compactionFailureText(outcome durable.TaskOutcome) string {
	switch outcome.Status {
	case durable.OutcomeFailed, durable.OutcomeFaulted:
		message := ""
		if outcome.Error != nil {
			message = outcome.Error.Message
		}
		return "Compaction failed: " + message
	case durable.OutcomeOrphaned:
		return "Compaction failed: " + outcome.Reason
	default:
		return "Compaction was aborted"
	}
}

// failure is what a failed run's end needs from the table, read before the
// commit's first table write.
type failure struct {
	policy   AfterFailedRun
	boundary *boundary
}

func prepareFailure(tx *durable.Tx, rt *TaskRuntime) (failure, error) {
	settings := rt.Settings()
	if settings.AfterFailedRun != AfterFailedRunContinue {
		return failure{policy: settings.AfterFailedRun}, nil
	}
	b, err := prepareBoundary(tx, rt.ConversationID, settings)
	if err != nil {
		return failure{}, err
	}
	return failure{policy: settings.AfterFailedRun, boundary: b}, nil
}

// endFailedRun ends a failed run, settling its inputs with settlement, and
// does with the inputs queued behind it what Settings.AfterFailedRun says
// (spec §6): keep them, start the next run with them, or withdraw them.
func endFailedRun(tx *durable.Tx, rt *TaskRuntime, live *delta.Draft, f failure, settlement durable.SubmissionSettlement) error {
	if err := endRun(tx, live, rt.TaskID, settlement); err != nil {
		return err
	}
	if f.policy == AfterFailedRunWithdraw {
		if err := withdrawQueuedInputs(tx, rt.ConversationID, "run_failed"); err != nil {
			return err
		}
	}
	if f.boundary == nil {
		return nil
	}
	placed, err := applyBoundary(tx, f.boundary, true, rt.s.now().UnixMilli())
	if err != nil || len(placed.users) == 0 {
		return err
	}
	return startRun(tx, rt.ConversationID, live, placed.users)
}

// ensureProviderSessionID returns the persisted provider identity without
// writing in the normal path. A legacy conversation without pi.provider gets
// one migration commit whose document access runs Initial before the request
// starts.
func ensureProviderSessionID(ctx context.Context, rt *TaskRuntime) (string, error) {
	existing, err := rt.Snapshot(ctx, ProviderDoc.Conversation(rt.ConversationID))
	if err != nil {
		return "", err
	}
	if existing != nil {
		id, _ := existing["sessionId"].(string)
		return id, nil
	}
	var created string
	if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
		doc, err := tx.Doc(ProviderDoc.Conversation(rt.ConversationID))
		if err != nil {
			return nil, err
		}
		value, _ := doc.Get("sessionId")
		created, _ = value.(string)
		return nil, nil
	}); err != nil {
		return "", err
	}
	if created == "" {
		return "", fmt.Errorf("Conversation %d has no provider session ID", rt.ConversationID)
	}
	return created, nil
}
