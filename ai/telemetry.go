package ai

import (
	"reflect"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// pi-ai request telemetry (pi telemetry.ts, upstream 2ec55d80): a
// pi.ai.request or pi.ai.poll span around each Models request, opened under
// the caller's ProviderRequestOptions.Telemetry. It is content-free: block
// sizes and tool names, never text, thinking, arguments or the deferred
// handle. Providers are unchanged; the span observes the stream Models hands
// back (pi's lazyStream observer).

// Span, event and metric names of TelemetrySchema.
const (
	TelemetrySpanRequest = "pi.ai.request"
	TelemetrySpanPoll    = "pi.ai.poll"

	TelemetryEventFirstChunk = "pi.ai.first_chunk"
	TelemetryEventBlock      = "pi.ai.block"

	TelemetryMetricRequestDuration    = "pi.ai.request.duration"
	TelemetryMetricFirstChunkDuration = "pi.ai.first_chunk.duration"
	TelemetryMetricTokens             = "pi.ai.tokens"
	TelemetryMetricCost               = "pi.ai.cost"
)

// maxTelemetryErrorMessage caps a failed request's status message (pi
// slice(0, 500)), in UTF-16 units as pi counts them.
const maxTelemetryErrorMessage = 500

func telemetryAttr(t telemetry.AttributeType, required bool, description string) telemetry.AttributeDefinition {
	return telemetry.AttributeDefinition{Type: t, Required: required, Description: description}
}

func telemetrySpanDefinition(description string) telemetry.SpanDefinition {
	start := telemetryModelAttributes()
	start["api"] = telemetryAttr(telemetry.AttributeString, true, "API identifier.")
	start["reasoning"] = telemetryAttr(telemetry.AttributeString, false, "Scalar requested reasoning level.")
	return telemetry.SpanDefinition{
		Description:     description,
		Parents:         telemetry.ParentDefinition{Kind: telemetry.ParentsAny},
		StartAttributes: start,
		EndAttributes: map[string]telemetry.AttributeDefinition{
			"stop":              telemetryStopAttribute(false),
			"usage.input":       telemetryAttr(telemetry.AttributeNumber, false, "Uncached input tokens."),
			"usage.output":      telemetryAttr(telemetry.AttributeNumber, false, "Output tokens."),
			"usage.cache_read":  telemetryAttr(telemetry.AttributeNumber, false, "Cache-read tokens."),
			"usage.cache_write": telemetryAttr(telemetry.AttributeNumber, false, "Cache-write tokens."),
			"cost.total":        telemetryAttr(telemetry.AttributeNumber, false, "Total request cost in USD."),
			"response.id": func() telemetry.AttributeDefinition {
				d := telemetryAttr(telemetry.AttributeString, false, "Provider response identifier.")
				d.Cardinality = telemetry.CardinalityHigh
				return d
			}(),
		},
		Events: map[string]telemetry.EventDefinition{
			TelemetryEventFirstChunk: {Description: "First normalized stream event delivered.", Attributes: map[string]telemetry.AttributeDefinition{}},
			TelemetryEventBlock: {
				Description: "A completed content block; content is never recorded.",
				Attributes: map[string]telemetry.AttributeDefinition{
					"type": func() telemetry.AttributeDefinition {
						d := telemetryAttr(telemetry.AttributeString, true, "Block kind.")
						d.Values = []any{"text", "thinking", "tool_call"}
						return d
					}(),
					"index": telemetryAttr(telemetry.AttributeNumber, true, "Content block index."),
					"chars": telemetryAttr(telemetry.AttributeNumber, true, "UTF-16 character count."),
					"tool":  telemetryAttr(telemetry.AttributeString, false, "Tool name, for tool-call blocks only."),
				},
			},
		},
		Status: telemetry.StatusDefinition{
			Default:   "ok",
			ErrorWhen: "Stop reason is error: provider_error with at most 500 error-message characters. Aborted stays ok.",
		},
	}
}

func telemetryStopAttribute(required bool) telemetry.AttributeDefinition {
	d := telemetryAttr(telemetry.AttributeString, required, "Normalized stop reason.")
	d.Values = []any{"pending", "stop", "length", "toolUse", "error", "aborted", "deferred"}
	return d
}

func telemetryModelAttributes() map[string]telemetry.AttributeDefinition {
	return map[string]telemetry.AttributeDefinition{
		"provider": telemetryAttr(telemetry.AttributeString, true, "Provider identifier."),
		"model":    telemetryAttr(telemetry.AttributeString, true, "Requested model identifier."),
	}
}

func telemetryMetricAttributes(name string, definition telemetry.AttributeDefinition) map[string]telemetry.AttributeDefinition {
	attributes := telemetryModelAttributes()
	attributes[name] = definition
	return attributes
}

// TelemetrySchema is pi-ai's telemetry vocabulary (pi AI_TELEMETRY_SCHEMA):
// request metadata, a bounded error status, events and low-cardinality
// metrics.
var TelemetrySchema = telemetry.SchemaDefinition{
	Version: 1,
	Spans: map[string]telemetry.SpanDefinition{
		TelemetrySpanRequest: telemetrySpanDefinition("One logical model request, including authentication and provider setup."),
		TelemetrySpanPoll:    telemetrySpanDefinition("One deferred-response poll, not the deferred request's lifetime."),
	},
	Metrics: map[string]telemetry.MetricDefinition{
		TelemetryMetricRequestDuration: {
			Kind:        telemetry.MetricHistogram,
			Description: "Logical request or poll duration.",
			Unit:        "ms",
			Attributes:  telemetryMetricAttributes("stop", telemetryStopAttribute(true)),
		},
		TelemetryMetricFirstChunkDuration: {
			Kind:        telemetry.MetricHistogram,
			Description: "Time to the first normalized stream event.",
			Unit:        "ms",
			Attributes:  telemetryModelAttributes(),
		},
		TelemetryMetricTokens: {
			Kind:        telemetry.MetricCounter,
			Description: "Reported model token usage; pending and deferred polls are excluded.",
			Unit:        "token",
			Attributes: telemetryMetricAttributes("kind", func() telemetry.AttributeDefinition {
				d := telemetryAttr(telemetry.AttributeString, true, "Token usage category.")
				d.Values = []any{"input", "output", "cache_read", "cache_write"}
				return d
			}()),
		},
		TelemetryMetricCost: {
			Kind:        telemetry.MetricCounter,
			Description: "Reported model cost; pending and deferred polls are excluded.",
			Unit:        "USD",
			Attributes:  telemetryModelAttributes(),
		},
	},
}

type aiInstruments struct {
	requestDuration    telemetry.Histogram
	firstChunkDuration telemetry.Histogram
	tokens             telemetry.Counter
	cost               telemetry.Counter
}

func newAIInstruments(adapter telemetry.Metrics) *aiInstruments {
	metrics, err := telemetry.NewTypedMetrics(adapter, TelemetrySchema)
	if err != nil {
		panic(err) // one schema cannot declare a name twice
	}
	return &aiInstruments{
		requestDuration:    metrics.Histogram(TelemetryMetricRequestDuration),
		firstChunkDuration: metrics.Histogram(TelemetryMetricFirstChunkDuration),
		tokens:             metrics.Counter(TelemetryMetricTokens),
		cost:               metrics.Counter(TelemetryMetricCost),
	}
}

// aiInstrumentCache keeps the instruments of the last metrics object they
// were created for (pi's WeakMap instrumentCache). Go has no weak map over
// interface values, and a map would keep every adapter alive, so it holds
// one: a host wiring in one adapter, as pi's hosts do, creates its
// instruments once, and one adapter per session costs a re-creation when
// they alternate. An adapter whose type is not comparable cannot be matched
// and gets fresh instruments per request.
var aiInstrumentCache struct {
	sync.Mutex
	adapter     telemetry.Metrics
	instruments *aiInstruments
}

func aiInstrumentsFor(adapter telemetry.Metrics) *aiInstruments {
	if !reflect.TypeOf(adapter).Comparable() {
		return newAIInstruments(adapter)
	}
	aiInstrumentCache.Lock()
	defer aiInstrumentCache.Unlock()
	if aiInstrumentCache.instruments == nil || aiInstrumentCache.adapter != adapter {
		aiInstrumentCache.adapter = adapter
		aiInstrumentCache.instruments = newAIInstruments(adapter)
	}
	return aiInstrumentCache.instruments
}

// telemetryNow is the monotonic clock request durations are read from (pi
// performance.now); tests replace it.
var telemetryNow = time.Now

func telemetryMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// truncateTelemetryMessage keeps the first max UTF-16 units of s. Where the
// cut falls inside a surrogate pair pi keeps the lone high surrogate, which a
// Go string cannot hold; the pair is dropped whole instead.
func truncateTelemetryMessage(s string, max int) string {
	units := 0
	for i, r := range s {
		units += utf16.RuneLen(r)
		if units > max {
			return s[:i]
		}
	}
	return s
}

// requestObserver records one Models request on its span (pi
// observeModelRequest). Its methods run on the one goroutine forwarding the
// request's stream.
type requestObserver struct {
	span        telemetry.Span
	finish      func()
	instruments *aiInstruments
	provider    string
	model       string
	started     time.Time
	poll        bool
	first       bool
	toolChars   map[int]int
}

// observeModelRequest opens the request's span under parent and returns the
// observer that closes it. The span stays open on its own goroutine, inside
// parent.StartSpan's callback, until onResult (pi's run promise).
func observeModelRequest(model *Model, parent telemetry.Span, reasoning ThinkingLevel, poll bool) *requestObserver {
	o := &requestObserver{
		instruments: aiInstrumentsFor(telemetry.MetricsOf(parent)),
		provider:    model.Provider,
		model:       model.ID,
		started:     telemetryNow(),
		poll:        poll,
		first:       true,
		toolChars:   map[int]int{},
	}
	start := telemetry.SpanAttributes{"provider": model.Provider, "api": model.Api, "model": model.ID}
	if reasoning != "" {
		start["reasoning"] = string(reasoning)
	}
	name := TelemetrySpanRequest
	if poll {
		name = TelemetrySpanPoll
	}
	opened := make(chan telemetry.Span, 1)
	finished := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		// A panicking adapter must not take the process down from a
		// goroutine no caller can recover on: the request goes on untraced,
		// or with its span left to the adapter, if the panic came later.
		defer func() { _ = recover() }()
		_ = parent.StartSpan(telemetry.SpanOptions{Name: name, Attributes: start}, func(span telemetry.Span) error {
			opened <- span
			<-finished
			return nil
		})
	}()
	select {
	case o.span = <-opened:
	case <-ended:
		// An adapter that never ran the callback; the request goes untraced.
		o.span = telemetry.NoopSpan
	}
	var once sync.Once
	o.finish = func() { once.Do(func() { close(finished) }) }
	return o
}

// metricAttributes are the request's low-cardinality metric attributes, plus
// one more when key is set.
func (o *requestObserver) metricAttributes(key, value string) telemetry.SpanAttributes {
	attributes := telemetry.SpanAttributes{"provider": o.provider, "model": o.model}
	if key != "" {
		attributes[key] = value
	}
	return attributes
}

func (o *requestObserver) onEvent(event AssistantMessageEvent) {
	if o.first {
		o.first = false
		o.span.AddEvent(TelemetryEventFirstChunk, telemetry.SpanAttributes{})
		o.instruments.firstChunkDuration.Record(telemetryMillis(telemetryNow().Sub(o.started)), o.metricAttributes("", ""), o.span)
	}
	switch event.Type {
	case EventToolCallDelta:
		o.toolChars[event.ContentIndex] += utf16Units(event.Delta)
	case EventTextEnd, EventThinkingEnd:
		kind := "text"
		if event.Type == EventThinkingEnd {
			kind = "thinking"
		}
		o.span.AddEvent(TelemetryEventBlock, telemetry.SpanAttributes{
			"type":  kind,
			"index": event.ContentIndex,
			"chars": utf16Units(event.Content),
		})
	case EventToolCallEnd:
		chars := o.toolChars[event.ContentIndex]
		delete(o.toolChars, event.ContentIndex)
		attributes := telemetry.SpanAttributes{"type": "tool_call", "index": event.ContentIndex, "chars": chars}
		if event.ToolCall != nil {
			attributes["tool"] = event.ToolCall.Name
		}
		o.span.AddEvent(TelemetryEventBlock, attributes)
	}
}

func (o *requestObserver) onResult(message *AssistantMessage) {
	defer o.finish()
	if message == nil {
		return
	}
	stop := string(message.StopReason)
	end := telemetry.SpanAttributes{
		"stop":              stop,
		"usage.input":       message.Usage.Input,
		"usage.output":      message.Usage.Output,
		"usage.cache_read":  message.Usage.CacheRead,
		"usage.cache_write": message.Usage.CacheWrite,
		"cost.total":        message.Usage.Cost.Total,
	}
	// pi writes responseId even when undefined; an absent ID is left out.
	if message.ResponseID != "" {
		end["response.id"] = message.ResponseID
	}
	o.span.SetAttributes(end)
	if message.StopReason == StopError {
		o.span.SetStatus(telemetry.SpanStatus{
			Code:  telemetry.StatusError,
			Error: &telemetry.SpanError{Name: "provider_error", Message: truncateTelemetryMessage(message.ErrorMessage, maxTelemetryErrorMessage)},
		})
	}
	o.instruments.requestDuration.Record(telemetryMillis(telemetryNow().Sub(o.started)), o.metricAttributes("stop", stop), o.span)
	if !o.poll || (message.StopReason != StopPending && message.StopReason != StopDeferred) {
		usage := message.Usage
		for _, token := range []struct {
			kind  string
			value int
		}{{"input", usage.Input}, {"output", usage.Output}, {"cache_read", usage.CacheRead}, {"cache_write", usage.CacheWrite}} {
			o.instruments.tokens.Add(float64(token.value), o.metricAttributes("kind", token.kind))
		}
		o.instruments.cost.Add(usage.Cost.Total, o.metricAttributes("", ""))
	}
}

// observeRequest runs setup, a Models request, under a span opened beneath
// parent (pi lazyStream with observeModelRequest). Each event reaches the
// returned stream before the observer sees it, and the result is observed
// after the stream ends (pi forwardStream). setup's own failures arrive as
// its error stream and are observed like any other.
//
// Without a parent, or under the no-op span, there is nothing to record, so
// setup's stream is returned as it is, without a forwarding goroutine.
func observeRequest(model *Model, parent telemetry.Span, reasoning ThinkingLevel, poll bool, setup func() *AssistantMessageEventStream) *AssistantMessageEventStream {
	if parent == nil || parent == telemetry.NoopSpan {
		return setup()
	}
	return forwardObserved(setup, observeModelRequest(model, parent, reasoning, poll))
}

// streamObserver is a synchronous, read-only observer of a forwarded stream
// (pi LazyStreamObserver).
type streamObserver interface {
	onEvent(event AssistantMessageEvent)
	onResult(result *AssistantMessage)
}

// forwardObserved returns a stream that forwards setup's events and result,
// showing each to observer after it is delivered, and the result once.
func forwardObserved(setup func() *AssistantMessageEventStream, observer streamObserver) *AssistantMessageEventStream {
	inner := setup()
	// Untimed: the provider's stream already timed its response, and timing
	// it again here would make a traced result differ from an untraced one.
	outer := newAssistantMessageEventStream()
	go func() {
		for event := range inner.Events() {
			outer.Push(event)
			observer.onEvent(event)
		}
		result := inner.Result()
		if result != nil {
			outer.End(result)
		} else {
			outer.End()
		}
		observer.onResult(result)
	}()
	return outer
}
