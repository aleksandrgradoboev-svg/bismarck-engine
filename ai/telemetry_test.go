package ai

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Ported from pi packages/ai/test/telemetry.test.ts (upstream 2ec55d80).
// Adapted: Go's StreamOptions has no reasoning field, so only StreamSimple
// records one, and ThinkingLevel is a string, so pi's non-scalar native
// reasoning cannot be passed; a ToolCall's arguments are a map that cannot
// throw on read, so the "without reading arguments" half of the interleaved
// case has no Go form; and every Go stream has Result, so pi's iterable
// without a result method becomes a stream ended by its terminal event alone.
// Added: the UTF-16 cap of the error message at a surrogate pair and over
// multi-byte text.

var telemetryTestModel = &Model{Provider: "test", ID: "model", Api: "telemetry-test"}

var telemetryTestHandle = DeferredHandle{Provider: "test", ModelID: "model", Api: "telemetry-test", ID: "deferred-secret"}

func telemetryTestMessage(stop StopReason) *AssistantMessage {
	return &AssistantMessage{
		Api:      telemetryTestModel.Api,
		Provider: telemetryTestModel.Provider,
		Model:    telemetryTestModel.ID,
		Content: ContentList{
			TextContent{Text: "text-secret"},
			ThinkingContent{Thinking: "thinking-secret"},
			ToolCall{ID: "call-secret", Name: "lookup", Arguments: map[string]any{"value": "argument-secret"}},
		},
		Usage: Usage{
			Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, TotalTokens: 110,
			Cost: CostBreakdown{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1},
		},
		ResponseID: "response",
		StopReason: stop,
		Timestamp:  time.Now().UnixMilli(),
	}
}

func telemetryTestEvents(result *AssistantMessage) []AssistantMessageEvent {
	toolCall := result.Content[2].(ToolCall)
	events := []AssistantMessageEvent{
		{Type: EventStart, Partial: result},
		{Type: EventTextDelta, ContentIndex: 0, Delta: "text-secret", Partial: result},
		{Type: EventTextEnd, ContentIndex: 0, Content: "text-secret", Partial: result},
		{Type: EventThinkingEnd, ContentIndex: 1, Content: "thinking-secret", Partial: result},
		{Type: EventToolCallDelta, ContentIndex: 2, Delta: `{"value":`, Partial: result},
		{Type: EventToolCallDelta, ContentIndex: 2, Delta: `"argument-secret"}`, Partial: result},
		{Type: EventToolCallEnd, ContentIndex: 2, ToolCall: &toolCall, Partial: result},
	}
	switch result.StopReason {
	case StopError, StopAborted:
		return append(events, AssistantMessageEvent{Type: EventError, Reason: result.StopReason, Error: result})
	case StopPending:
		return append(events, AssistantMessageEvent{Type: EventDone, Reason: StopDeferred, Message: result})
	}
	return append(events, AssistantMessageEvent{Type: EventDone, Reason: result.StopReason, Message: result})
}

func telemetryTestModels(result *AssistantMessage) (MutableModels, []AssistantMessageEvent) {
	events := telemetryTestEvents(result)
	respond := func() *AssistantMessageEventStream {
		s := NewAssistantMessageEventStream()
		for _, event := range events {
			s.Push(event)
		}
		s.End(result)
		return s
	}
	m := modelsWithEnv(map[string]string{"K": "key"}, nil)
	m.SetProvider(CreateProvider(CreateProviderOptions{
		ID:     telemetryTestModel.Provider,
		Auth:   ProviderAuth{APIKey: EnvAPIKeyAuth(string(telemetryTestModel.Provider), "K")},
		Models: []*Model{telemetryTestModel},
		API: ptrStreams(ProviderStreams{
			Stream: func(context.Context, *Model, TranscriptContext, *StreamOptions) *AssistantMessageEventStream {
				return respond()
			},
			StreamSimple: func(context.Context, *Model, TranscriptContext, *SimpleStreamOptions) *AssistantMessageEventStream {
				return respond()
			},
			FetchDeferred: func(context.Context, *Model, DeferredHandle, *DeferredFetchOptions) *AssistantMessageEventStream {
				return respond()
			},
		}),
	}))
	return m, events
}

// underCaller runs fn inside a "caller" span of recorder and waits until every
// span has settled (pi's settled helper).
func underCaller(t *testing.T, recorder *telemetry.InMemoryContext, fn func(parent telemetry.Span)) {
	t.Helper()
	if err := recorder.StartSpan(telemetry.SpanOptions{Name: "caller"}, func(span telemetry.Span) error {
		fn(span)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, recorder)
}

func waitSettled(t *testing.T, recorder *telemetry.InMemoryContext) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		settled := true
		for _, span := range recorder.Spans() {
			settled = settled && span.Settled
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spans never settled: %+v", recorder.Spans())
		}
		time.Sleep(time.Millisecond)
	}
}

func mustNotMention(t *testing.T, what string, value any, secrets ...string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(data), secret) {
			t.Errorf("%s mentions %q: %s", what, secret, data)
		}
	}
}

func TestTelemetryObservesModelsRequestsWithoutChangingEventsOrResults(t *testing.T) {
	for _, method := range []string{"Stream", "StreamSimple", "StreamDeferred"} {
		t.Run(method, func(t *testing.T) {
			recorder := telemetry.NewInMemoryContext()
			result := telemetryTestMessage(StopStop)
			m, want := telemetryTestModels(result)
			underCaller(t, recorder, func(parent telemetry.Span) {
				request := ProviderRequestOptions{Telemetry: parent}
				var stream *AssistantMessageEventStream
				switch method {
				case "Stream":
					stream = m.Stream(context.Background(), telemetryTestModel, Context{}, &ModelsStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}})
				case "StreamSimple":
					stream = m.StreamSimple(context.Background(), telemetryTestModel, Context{}, &ModelsSimpleStreamOptions{SimpleStreamOptions: SimpleStreamOptions{StreamOptions: StreamOptions{ProviderRequestOptions: request}, Reasoning: ThinkingHigh}})
				default:
					stream = m.StreamDeferred(context.Background(), telemetryTestModel, telemetryTestHandle, &ModelsDeferredFetchOptions{DeferredFetchOptions: DeferredFetchOptions{ProviderRequestOptions: request}})
				}
				var received []AssistantMessageEvent
				for event := range stream.Events() {
					received = append(received, event)
				}
				if !reflect.DeepEqual(received, want) {
					t.Fatalf("received %+v, want %+v", received, want)
				}
				for i := range received {
					if received[i].Partial != want[i].Partial || received[i].ToolCall != want[i].ToolCall {
						t.Fatalf("event %d is not the provider's own", i)
					}
				}
				if got := stream.Result(); got != result {
					t.Fatalf("result = %p, want the provider's own %p", got, result)
				}
			})

			spans := recorder.Spans()
			if len(spans) != 2 {
				t.Fatalf("spans = %+v, want caller and request", spans)
			}
			parent, span := spans[0], spans[1]
			wantName := TelemetrySpanRequest
			if method == "StreamDeferred" {
				wantName = TelemetrySpanPoll
			}
			if span.Name != wantName || span.ParentID != parent.ID {
				t.Fatalf("span %q under %d, want %q under %d", span.Name, span.ParentID, wantName, parent.ID)
			}
			wantAttributes := telemetry.SpanAttributes{
				"provider": "test", "api": "telemetry-test", "model": "model",
				"stop": "stop", "usage.input": 11, "usage.output": 22, "usage.cache_read": 33,
				"usage.cache_write": 44, "cost.total": 1.0, "response.id": "response",
			}
			if method == "StreamSimple" {
				wantAttributes["reasoning"] = "high"
			}
			if !reflect.DeepEqual(span.Attributes, wantAttributes) {
				t.Fatalf("attributes = %#v, want %#v", span.Attributes, wantAttributes)
			}
			wantEvents := []telemetry.RecordedEvent{
				{Name: TelemetryEventFirstChunk, Attributes: telemetry.SpanAttributes{}},
				{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "text", "index": 0, "chars": 11}},
				{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "thinking", "index": 1, "chars": 15}},
				{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "tool_call", "index": 2, "chars": 27, "tool": "lookup"}},
			}
			if !reflect.DeepEqual(span.Events, wantEvents) {
				t.Fatalf("events = %#v, want %#v", span.Events, wantEvents)
			}
			mustNotMention(t, "span", span, "text-secret", "thinking-secret", "argument-secret", "call-secret", "deferred-secret")

			measurements := recorder.Measurements()
			if len(measurements) != 7 {
				t.Fatalf("measurements = %+v, want 7", measurements)
			}
			var counters []float64
			for _, m := range measurements {
				if m.Kind == telemetry.MeasurementCounter {
					counters = append(counters, m.Value)
				}
				keys := make([]string, 0, len(m.Attributes))
				for key := range m.Attributes {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				wantKeys := []string{"model", "provider"}
				switch m.Name {
				case TelemetryMetricTokens:
					wantKeys = []string{"kind", "model", "provider"}
				case TelemetryMetricRequestDuration:
					wantKeys = []string{"model", "provider", "stop"}
				}
				if !slices.Equal(keys, wantKeys) {
					t.Errorf("%s attributes %v, want %v", m.Name, keys, wantKeys)
				}
				if m.Kind == telemetry.MeasurementHistogram && !reflect.DeepEqual(m.Context, span.Context) {
					t.Errorf("%s exemplar %v, want the request span %v", m.Name, m.Context, span.Context)
				}
			}
			if !slices.Equal(counters, []float64{11, 22, 33, 44, 1}) {
				t.Fatalf("counters = %v", counters)
			}
		})
	}
}

func TestTelemetryReportsErrorAndAbortedStatusWithoutErrorAttributesOrEvents(t *testing.T) {
	for _, reason := range []StopReason{StopError, StopAborted} {
		t.Run(string(reason), func(t *testing.T) {
			recorder := telemetry.NewInMemoryContext()
			result := telemetryTestMessage(reason)
			result.ErrorMessage = "error-secret"
			m, _ := telemetryTestModels(result)
			underCaller(t, recorder, func(parent telemetry.Span) {
				options := &ModelsSimpleStreamOptions{}
				options.Telemetry = parent
				if got := m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, options); got != result {
					t.Fatalf("result = %+v, want the provider's own", got)
				}
			})
			span := recorder.Spans()[1]
			want := telemetry.SpanStatus{Code: telemetry.StatusOK}
			if reason == StopError {
				want = telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "provider_error", Message: "error-secret"}}
			}
			if !reflect.DeepEqual(span.Status, want) {
				t.Fatalf("status = %+v, want %+v", span.Status, want)
			}
			if span.Attributes["stop"] != string(reason) {
				t.Fatalf("stop = %v", span.Attributes["stop"])
			}
			mustNotMention(t, "attributes", span.Attributes, "error-secret")
			mustNotMention(t, "events", span.Events, "error-secret")
			if reason == StopAborted {
				mustNotMention(t, "span", span, "error-secret")
			}
		})
	}
}

func TestTelemetryBoundsPollErrorMessages(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
	}{
		{"missing", "", ""},
		{"long", strings.Repeat("x", 500) + "discarded-tail", strings.Repeat("x", 500)},
		{"surrogate pair at the cap", strings.Repeat("x", 499) + "😀" + "discarded-tail", strings.Repeat("x", 499)},
		{"multi-byte", strings.Repeat("é", 600), strings.Repeat("é", 500)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.NewInMemoryContext()
			result := telemetryTestMessage(StopError)
			result.ErrorMessage = tc.message
			m, _ := telemetryTestModels(result)
			underCaller(t, recorder, func(parent telemetry.Span) {
				options := &ModelsDeferredFetchOptions{}
				options.Telemetry = parent
				if got := m.FetchDeferred(context.Background(), telemetryTestModel, telemetryTestHandle, options); got != result {
					t.Fatalf("result = %+v, want the provider's own", got)
				}
			})
			span := recorder.Spans()[1]
			want := telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "provider_error", Message: tc.want}}
			if !reflect.DeepEqual(span.Status, want) {
				t.Fatalf("status = %+v, want %+v", span.Status, want)
			}
			mustNotMention(t, "span", span, "discarded-tail")
			mustNotMention(t, "attributes", span.Attributes, "xxx")
			mustNotMention(t, "events", span.Events, "xxx")
		})
	}
}

func TestTelemetryIncludesSetupFailuresAndOmitsAnUnsetReasoning(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	m := modelsWithEnv(nil, nil)
	underCaller(t, recorder, func(parent telemetry.Span) {
		options := &ModelsSimpleStreamOptions{}
		options.Telemetry = parent
		if got := m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, options); got.StopReason != StopError {
			t.Fatalf("stop = %q, want error", got.StopReason)
		}
	})
	span := recorder.Spans()[1]
	if _, ok := span.Attributes["reasoning"]; ok {
		t.Fatalf("attributes = %v, want no reasoning", span.Attributes)
	}
	if want := []telemetry.RecordedEvent{{Name: TelemetryEventFirstChunk, Attributes: telemetry.SpanAttributes{}}}; !reflect.DeepEqual(span.Events, want) {
		t.Fatalf("events = %#v, want %#v", span.Events, want)
	}
	want := telemetry.SpanStatus{Code: telemetry.StatusError, Error: &telemetry.SpanError{Name: "provider_error", Message: "Unknown provider: test"}}
	if !reflect.DeepEqual(span.Status, want) {
		t.Fatalf("status = %+v, want %+v", span.Status, want)
	}
	mustNotMention(t, "attributes", span.Attributes, "Unknown provider")
}

func TestTelemetryCountsInterleavedToolDeltasIndependently(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	result := telemetryTestMessage(StopStop)
	underCaller(t, recorder, func(parent telemetry.Span) {
		observer := observeModelRequest(telemetryTestModel, parent, "", false)
		for _, delta := range []struct {
			index int
			delta string
		}{{2, "ab"}, {3, "123"}, {2, "c"}} {
			observer.onEvent(AssistantMessageEvent{Type: EventToolCallDelta, ContentIndex: delta.index, Delta: delta.delta, Partial: result})
		}
		toolCall := &ToolCall{ID: "call", Name: "lookup"}
		for _, index := range []int{3, 2, 4} {
			observer.onEvent(AssistantMessageEvent{Type: EventToolCallEnd, ContentIndex: index, ToolCall: toolCall, Partial: result})
		}
		observer.onResult(result)
	})
	want := []telemetry.RecordedEvent{
		{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "tool_call", "index": 3, "chars": 3, "tool": "lookup"}},
		{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "tool_call", "index": 2, "chars": 3, "tool": "lookup"}},
		{Name: TelemetryEventBlock, Attributes: telemetry.SpanAttributes{"type": "tool_call", "index": 4, "chars": 0, "tool": "lookup"}},
	}
	if got := recorder.Spans()[1].Events[1:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

// tracingOnlySpan hides the adapter's metrics: its method set is Span's only.
type tracingOnlySpan struct{ telemetry.Span }

func TestTelemetrySupportsATracingOnlySpanWithoutMetrics(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	m, _ := telemetryTestModels(telemetryTestMessage(StopStop))
	underCaller(t, recorder, func(parent telemetry.Span) {
		options := &ModelsSimpleStreamOptions{}
		options.Telemetry = tracingOnlySpan{parent}
		m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, options)
	})
	if spans := recorder.Spans(); len(spans) != 2 {
		t.Fatalf("spans = %+v, want 2", spans)
	}
	if measurements := recorder.Measurements(); len(measurements) != 0 {
		t.Fatalf("measurements = %+v, want none", measurements)
	}
}

func TestTelemetryDefaultsToNoop(t *testing.T) {
	m, _ := telemetryTestModels(telemetryTestMessage(StopStop))
	if got := m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, nil); got.StopReason != StopStop {
		t.Fatalf("stop = %q", got.StopReason)
	}
	if !TelemetrySchema.Spans[TelemetrySpanRequest].StartAttributes["provider"].Required {
		t.Fatal("provider is not a required start attribute")
	}
}

func TestTelemetryRecordsLowCardinalityMetricsWithMonotonicDurations(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	result := telemetryTestMessage(StopStop)
	base := time.Now()
	clock := base.Add(100 * time.Millisecond)
	telemetryNow = func() time.Time { return clock }
	t.Cleanup(func() { telemetryNow = time.Now })
	underCaller(t, recorder, func(parent telemetry.Span) {
		observer := observeModelRequest(telemetryTestModel, parent, "", false)
		clock = base.Add(125 * time.Millisecond)
		observer.onEvent(AssistantMessageEvent{Type: EventStart, Partial: result})
		clock = base.Add(175 * time.Millisecond)
		observer.onResult(result)
	})
	type row struct {
		Name       string
		Value      float64
		Attributes telemetry.SpanAttributes
		Unit       string
	}
	model := func(extra ...string) telemetry.SpanAttributes {
		attributes := telemetry.SpanAttributes{"provider": "test", "model": "model"}
		for i := 0; i < len(extra); i += 2 {
			attributes[extra[i]] = extra[i+1]
		}
		return attributes
	}
	want := []row{
		{TelemetryMetricFirstChunkDuration, 25, model(), "ms"},
		{TelemetryMetricRequestDuration, 75, model("stop", "stop"), "ms"},
		{TelemetryMetricTokens, 11, model("kind", "input"), "token"},
		{TelemetryMetricTokens, 22, model("kind", "output"), "token"},
		{TelemetryMetricTokens, 33, model("kind", "cache_read"), "token"},
		{TelemetryMetricTokens, 44, model("kind", "cache_write"), "token"},
		{TelemetryMetricCost, 1, model(), "USD"},
	}
	measurements := recorder.Measurements()
	var got []row
	for _, m := range measurements {
		got = append(got, row{m.Name, m.Value, m.Attributes, m.Unit})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measurements = %+v, want %+v", got, want)
	}
	if measurements[0].Context == nil || measurements[1].Context == nil {
		t.Fatal("a duration has no exemplar context")
	}
}

func TestTelemetryRecordsPollUsageOnlyWhenFinal(t *testing.T) {
	for _, reason := range []StopReason{StopPending, StopDeferred, StopStop, StopError} {
		t.Run(string(reason), func(t *testing.T) {
			recorder := telemetry.NewInMemoryContext()
			m, _ := telemetryTestModels(telemetryTestMessage(reason))
			underCaller(t, recorder, func(parent telemetry.Span) {
				options := &ModelsDeferredFetchOptions{}
				options.Telemetry = parent
				m.FetchDeferred(context.Background(), telemetryTestModel, telemetryTestHandle, options)
			})
			counters := 0
			for _, m := range recorder.Measurements() {
				if m.Kind == telemetry.MeasurementCounter {
					counters++
				}
			}
			want := 5
			if reason == StopPending || reason == StopDeferred {
				want = 0
			}
			if counters != want {
				t.Fatalf("counters = %d, want %d", counters, want)
			}
		})
	}
}

// countingMetrics records which instruments are created on the adapter.
type countingMetrics struct {
	telemetry.Metrics
	histograms, counters []string
}

func (m *countingMetrics) Histogram(name string, options telemetry.HistogramOptions) telemetry.Histogram {
	m.histograms = append(m.histograms, name)
	return m.Metrics.Histogram(name, options)
}

func (m *countingMetrics) Counter(name string, options telemetry.InstrumentOptions) telemetry.Counter {
	m.counters = append(m.counters, name)
	return m.Metrics.Counter(name, options)
}

// meteredSpan is a span whose adapter metrics are metrics.
type meteredSpan struct {
	telemetry.Span
	metrics telemetry.Metrics
}

func (s meteredSpan) Metrics() telemetry.Metrics { return s.metrics }

func TestTelemetryCreatesInstrumentsOncePerMetricsObject(t *testing.T) {
	recorder := telemetry.NewInMemoryContext()
	metrics := &countingMetrics{Metrics: recorder}
	m, _ := telemetryTestModels(telemetryTestMessage(StopStop))
	underCaller(t, recorder, func(parent telemetry.Span) {
		options := &ModelsSimpleStreamOptions{}
		options.Telemetry = meteredSpan{parent, metrics}
		m.CompleteSimple(context.Background(), telemetryTestModel, Context{}, options)
	})
	underCaller(t, recorder, func(parent telemetry.Span) {
		options := &ModelsDeferredFetchOptions{}
		options.Telemetry = meteredSpan{parent, metrics}
		m.FetchDeferred(context.Background(), telemetryTestModel, telemetryTestHandle, options)
	})
	underCaller(t, recorder, func(parent telemetry.Span) {
		options := &ModelsStreamOptions{}
		options.Telemetry = meteredSpan{parent, metrics}
		m.Complete(context.Background(), telemetryTestModel, Context{}, options)
	})
	if want := []string{TelemetryMetricRequestDuration, TelemetryMetricFirstChunkDuration}; !slices.Equal(metrics.histograms, want) {
		t.Fatalf("histograms = %v, want %v", metrics.histograms, want)
	}
	if want := []string{TelemetryMetricTokens, TelemetryMetricCost}; !slices.Equal(metrics.counters, want) {
		t.Fatalf("counters = %v, want %v", metrics.counters, want)
	}
	if got := len(recorder.Measurements()); got != 21 {
		t.Fatalf("measurements = %d, want 21", got)
	}
}

// recordingObserver records what a forwarded stream showed it, and whether
// the forwarded result was already delivered when the result was observed.
type recordingObserver struct {
	stream   **AssistantMessageEventStream
	observed []string
	results  []*AssistantMessage
}

func (o *recordingObserver) onEvent(event AssistantMessageEvent) {
	o.observed = append(o.observed, string(event.Type))
}

func (o *recordingObserver) onResult(result *AssistantMessage) {
	if (*o.stream).Result() != result {
		o.observed = append(o.observed, "result observed before delivery")
	}
	o.results = append(o.results, result)
	if result == nil {
		o.observed = append(o.observed, "missing")
	} else {
		o.observed = append(o.observed, string(result.StopReason))
	}
}

func forwardForTest(setup func() *AssistantMessageEventStream) (*AssistantMessageEventStream, *recordingObserver, chan struct{}) {
	var stream *AssistantMessageEventStream
	observer := &recordingObserver{stream: &stream}
	observed := make(chan struct{})
	ready := make(chan struct{})
	wrapped := &signallingObserver{observer, ready, observed}
	stream = forwardObserved(setup, wrapped)
	close(ready)
	return stream, observer, observed
}

// signallingObserver holds the result back until the test has the forwarded
// stream, and signals once the result is observed.
type signallingObserver struct {
	*recordingObserver
	ready, done chan struct{}
}

// onEvent checks that a terminal event already reached the forwarded stream:
// its result must be there without waiting for the observer.
func (o *signallingObserver) onEvent(event AssistantMessageEvent) {
	<-o.ready
	if event.Type == EventDone || event.Type == EventError {
		delivered := make(chan struct{})
		go func() {
			(*o.stream).Result()
			close(delivered)
		}()
		select {
		case <-delivered:
		case <-time.After(time.Second):
			o.observed = append(o.observed, "observed before delivery")
		}
	}
	o.recordingObserver.onEvent(event)
}

func (o *signallingObserver) onResult(result *AssistantMessage) {
	<-o.ready
	o.recordingObserver.onResult(result)
	close(o.done)
}

func TestForwardObservedObservesAfterDeliveryOncePerResult(t *testing.T) {
	result := telemetryTestMessage(StopStop)
	stream, observer, observed := forwardForTest(func() *AssistantMessageEventStream {
		s := NewAssistantMessageEventStream()
		s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: result})
		s.End(result)
		return s
	})
	if got := stream.Result(); got != result {
		t.Fatalf("result = %p, want %p", got, result)
	}
	<-observed
	if !slices.Equal(observer.observed, []string{"done", "stop"}) || len(observer.results) != 1 {
		t.Fatalf("observed %v, results %v", observer.observed, observer.results)
	}
}

func TestForwardObservedReportsTheTerminalResultOfAStreamNeverEnded(t *testing.T) {
	result := telemetryTestMessage(StopStop)
	stream, observer, observed := forwardForTest(func() *AssistantMessageEventStream {
		s := NewAssistantMessageEventStream()
		s.Push(AssistantMessageEvent{Type: EventDone, Reason: StopStop, Message: result})
		return s
	})
	if got := stream.Result(); got != result {
		t.Fatalf("result = %p, want %p", got, result)
	}
	<-observed
	if len(observer.results) != 1 || observer.results[0] != result {
		t.Fatalf("results = %v", observer.results)
	}
}

func TestForwardObservedObservesAResultOnlySourceOnce(t *testing.T) {
	result := telemetryTestMessage(StopStop)
	stream, observer, observed := forwardForTest(func() *AssistantMessageEventStream {
		s := NewAssistantMessageEventStream()
		s.End(result)
		return s
	})
	if got := stream.Result(); got != result {
		t.Fatalf("result = %p, want %p", got, result)
	}
	<-observed
	if !slices.Equal(observer.observed, []string{"stop"}) {
		t.Fatalf("observed %v", observer.observed)
	}
}

func TestForwardObservedReportsSetupFailureEventsAndTheResultOnce(t *testing.T) {
	stream, observer, observed := forwardForTest(func() *AssistantMessageEventStream {
		return ErrorStream(telemetryTestModel, errorString("setup-secret"))
	})
	if got := stream.Result(); got.ErrorMessage != "setup-secret" {
		t.Fatalf("error = %q", got.ErrorMessage)
	}
	<-observed
	if !slices.Equal(observer.observed, []string{"error", "error"}) {
		t.Fatalf("observed %v", observer.observed)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
