package telemetry

import "fmt"

// InstrumentOptions describe an instrument (pi InstrumentOptions); empty
// fields are absent.
type InstrumentOptions struct {
	Description string
	Unit        string
}

// HistogramOptions describe a histogram (pi HistogramOptions). Boundaries
// must be finite and strictly increasing; nil leaves them to the adapter.
type HistogramOptions struct {
	InstrumentOptions
	Boundaries []float64
}

// Counter adds non-negative measurements (pi TelemetryCounter).
type Counter interface {
	Add(value float64, attributes SpanAttributes)
}

// UpDownCounter adds measurements of either sign (pi TelemetryUpDownCounter).
type UpDownCounter interface {
	Add(value float64, attributes SpanAttributes)
}

// Histogram records measurements; context, when not nil, is the exemplar's
// span (pi TelemetryHistogram).
type Histogram interface {
	Record(value float64, attributes SpanAttributes, context Context)
}

// Metrics is the optional metrics capability of an adapter (pi
// TelemetryMetrics). Factories and recording methods are synchronous and must
// not panic; a measurement an adapter cannot take is dropped.
type Metrics interface {
	Counter(name string, options InstrumentOptions) Counter
	UpDownCounter(name string, options InstrumentOptions) UpDownCounter
	Histogram(name string, options HistogramOptions) Histogram
}

type noopInstrument struct{}

func (noopInstrument) Add(float64, SpanAttributes)             {}
func (noopInstrument) Record(float64, SpanAttributes, Context) {}

type noopMetrics struct{}

func (noopMetrics) Counter(string, InstrumentOptions) Counter             { return noopInstrument{} }
func (noopMetrics) UpDownCounter(string, InstrumentOptions) UpDownCounter { return noopInstrument{} }
func (noopMetrics) Histogram(string, HistogramOptions) Histogram          { return noopInstrument{} }

// NoopMetrics hands out shared instruments that never inspect or retain
// metadata, measurements or contexts (pi NOOP_TELEMETRY_METRICS).
var NoopMetrics Metrics = noopMetrics{}

// TypedMetrics binds instrument names to the metadata the schemas declare
// for them (pi createTypedMetrics). pi checks names, kinds and measurement
// attributes at compile time; Go has no such typing, so what applies is the
// binding of description, unit and boundaries, and duplicate names are
// rejected by NewTypedMetrics.
type TypedMetrics struct {
	metrics     Metrics
	definitions map[string]MetricDefinition
}

// NewTypedMetrics binds metrics to the metric vocabulary of schemas. A name
// declared by two schemas is an error.
func NewTypedMetrics(metrics Metrics, schemas ...SchemaDefinition) (*TypedMetrics, error) {
	definitions := map[string]MetricDefinition{}
	for _, schema := range schemas {
		for name, definition := range schema.Metrics {
			if _, duplicate := definitions[name]; duplicate {
				return nil, fmt.Errorf("telemetry: metric %q is declared by more than one schema", name)
			}
			definitions[name] = definition
		}
	}
	if metrics == nil {
		metrics = NoopMetrics
	}
	return &TypedMetrics{metrics: metrics, definitions: definitions}, nil
}

func (m *TypedMetrics) instrumentOptions(name string) InstrumentOptions {
	definition := m.definitions[name]
	return InstrumentOptions{Description: definition.Description, Unit: definition.Unit}
}

// Counter returns the counter named name with its declared metadata.
func (m *TypedMetrics) Counter(name string) Counter {
	return m.metrics.Counter(name, m.instrumentOptions(name))
}

// UpDownCounter returns the up-down counter named name with its declared
// metadata.
func (m *TypedMetrics) UpDownCounter(name string) UpDownCounter {
	return m.metrics.UpDownCounter(name, m.instrumentOptions(name))
}

// Histogram returns the histogram named name with its declared metadata and
// boundaries.
func (m *TypedMetrics) Histogram(name string) Histogram {
	options := HistogramOptions{InstrumentOptions: m.instrumentOptions(name)}
	if boundaries := m.definitions[name].Boundaries; boundaries != nil {
		options.Boundaries = append([]float64(nil), boundaries...)
	}
	return m.metrics.Histogram(name, options)
}
