package telemetry

// Schema data (pi TelemetrySchemaDefinition and its parts): the serializable
// vocabulary of a package's spans, events and metrics. JSON names follow pi,
// so a definition marshals as pi's object does.

// AttributeType is the declared type of an attribute (pi
// TelemetryAttributeType).
type AttributeType string

const (
	AttributeString       AttributeType = "string"
	AttributeNumber       AttributeType = "number"
	AttributeBoolean      AttributeType = "boolean"
	AttributeStringArray  AttributeType = "string[]"
	AttributeNumberArray  AttributeType = "number[]"
	AttributeBooleanArray AttributeType = "boolean[]"
)

// Attribute cardinalities (pi TelemetryAttributeMetadata.cardinality).
const (
	CardinalityLow  = "low"
	CardinalityHigh = "high"
)

// AttributeDefinition declares one attribute (pi TelemetryAttributeDefinition;
// with Required, pi's start, event and metric attribute definitions). pi
// types Values and Examples per attribute type; here they are values of the
// declared type. Values bounds a scalar's values, ElementValues an array's
// elements.
type AttributeDefinition struct {
	Type        AttributeType `json:"type"`
	Required    bool          `json:"required,omitempty"`
	Description string        `json:"description"`
	Sensitive   bool          `json:"sensitive,omitempty"`
	Cardinality string        `json:"cardinality,omitempty"`
	Values      []any         `json:"values,omitempty"`
	// ElementValues bounds the elements of an array attribute.
	ElementValues []any `json:"elementValues,omitempty"`
	Examples      []any `json:"examples,omitempty"`
}

// EventDefinition declares a span event (pi TelemetryEventDefinition).
type EventDefinition struct {
	Description string                         `json:"description"`
	Attributes  map[string]AttributeDefinition `json:"attributes"`
}

// Parent kinds (pi TelemetryParentDefinition.kind).
const (
	ParentsAny            = "any"
	ParentsRootOrExternal = "root_or_external"
	ParentsSpans          = "spans"
)

// ParentDefinition declares which spans may parent a span (pi
// TelemetryParentDefinition); Spans names them for kind "spans".
type ParentDefinition struct {
	Kind  string   `json:"kind"`
	Spans []string `json:"spans,omitempty"`
}

// StatusDefinition declares a span's status (pi's `status: { default: "ok";
// errorWhen }`).
type StatusDefinition struct {
	Default   string `json:"default"`
	ErrorWhen string `json:"errorWhen"`
}

// SpanDefinition declares a span (pi TelemetrySpanDefinition).
type SpanDefinition struct {
	Description     string                         `json:"description"`
	Parents         ParentDefinition               `json:"parents"`
	StartAttributes map[string]AttributeDefinition `json:"startAttributes"`
	EndAttributes   map[string]AttributeDefinition `json:"endAttributes"`
	Events          map[string]EventDefinition     `json:"events,omitempty"`
	Status          StatusDefinition               `json:"status"`
}

// Metric kinds (pi TelemetryMetricDefinition.kind).
const (
	MetricCounter       = "counter"
	MetricUpDownCounter = "upDownCounter"
	MetricHistogram     = "histogram"
)

// MetricDefinition declares a metric (pi TelemetryMetricDefinition);
// Boundaries apply to histograms only. Metric attributes are low-cardinality.
type MetricDefinition struct {
	Kind        string                         `json:"kind"`
	Description string                         `json:"description"`
	Unit        string                         `json:"unit,omitempty"`
	Attributes  map[string]AttributeDefinition `json:"attributes"`
	Boundaries  []float64                      `json:"boundaries,omitempty"`
}

// SchemaDefinition is a package's telemetry vocabulary (pi
// TelemetrySchemaDefinition, as defineTelemetrySchema returns it).
type SchemaDefinition struct {
	Version int                         `json:"version"`
	Spans   map[string]SpanDefinition   `json:"spans"`
	Metrics map[string]MetricDefinition `json:"metrics,omitempty"`
}
