package harness

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Durable telemetry (pi harness/telemetry.ts, 2ec55d80): content-free spans,
// events and metrics. IDs address durable records, never content.

// Span, event and metric names of TelemetrySchema.
const (
	SpanSubmit = "pi.durable.submit"
	SpanPhase  = "pi.durable.phase"

	EventEntry      = "pi.durable.entry"
	EventSubmission = "pi.durable.submission"
	EventReport     = "pi.durable.report"
	EventCheckpoint = "pi.durable.checkpoint"
	EventReplay     = "pi.durable.replay"

	MetricReports       = "pi.durable.reports"
	MetricPhaseDuration = "pi.durable.phase.duration"
	MetricTasksActive   = "pi.durable.tasks.active"
)

func attr(t telemetry.AttributeType, required bool, description string) telemetry.AttributeDefinition {
	return telemetry.AttributeDefinition{Type: t, Required: required, Description: description}
}

func highCardinality(d telemetry.AttributeDefinition) telemetry.AttributeDefinition {
	d.Cardinality = telemetry.CardinalityHigh
	return d
}

func withValues(d telemetry.AttributeDefinition, values ...any) telemetry.AttributeDefinition {
	d.Values = values
	return d
}

var (
	entryEvent = telemetry.EventDefinition{
		Description: "A transcript entry was committed; content remains in the transcript.",
		Attributes: map[string]telemetry.AttributeDefinition{
			"entry.id":   highCardinality(attr(telemetry.AttributeNumber, true, "Transcript entry ID.")),
			"entry.kind": attr(telemetry.AttributeString, true, "Entry kind."),
		},
	}
	reportTypeAttribute = withValues(attr(telemetry.AttributeString, true, "ReportInfo type."),
		"session_failed", "task_faulted", "task_parked", "migration_failed", "task_outdated",
		"task_error", "extension_error", "host_error", "listener_error")
	reportEvent = telemetry.EventDefinition{
		Description: "A report was raised; no error payload is recorded.",
		Attributes: map[string]telemetry.AttributeDefinition{
			"type":      reportTypeAttribute,
			"reason":    withValues(attr(telemetry.AttributeString, false, "Fixed report reason, when present."), "threw", "restart_limit", "missing_task", "incompatible_task"),
			"extension": attr(telemetry.AttributeString, false, "Registered extension name, when present."),
			"task.id":   highCardinality(attr(telemetry.AttributeNumber, false, "Task ID, when present.")),
		},
	}
	submissionEvent = telemetry.EventDefinition{
		Description: "A submission was settled.",
		Attributes: map[string]telemetry.AttributeDefinition{
			"submission.id":   highCardinality(attr(telemetry.AttributeNumber, true, "Submission ID.")),
			"status":          withValues(attr(telemetry.AttributeString, true, "Settlement."), "done", "unanswered"),
			"answer.entry_id": highCardinality(attr(telemetry.AttributeNumber, false, "Answer entry ID.")),
		},
	}
)

// TelemetrySchema is durable's telemetry vocabulary (pi
// DURABLE_TELEMETRY_SCHEMA).
var TelemetrySchema = telemetry.SchemaDefinition{
	Version: 1,
	Spans: map[string]telemetry.SpanDefinition{
		SpanSubmit: {
			Description: "Public submission admission and its commit. Inputs start root traces linked to their callers; writes are children. Internal admissions emit phase events only.",
			Parents:     telemetry.ParentDefinition{Kind: telemetry.ParentsAny},
			StartAttributes: map[string]telemetry.AttributeDefinition{
				"submission.type": withValues(attr(telemetry.AttributeString, true, "Submission type."), "input", "write"),
				"conversation.id": highCardinality(attr(telemetry.AttributeNumber, true, "Conversation ID.")),
			},
			EndAttributes: map[string]telemetry.AttributeDefinition{
				"submission.id": highCardinality(attr(telemetry.AttributeNumber, false, "Admitted submission ID.")),
				"result":        withValues(attr(telemetry.AttributeString, false, "Admission result; stale writes report done."), "queued", "placed", "done"),
			},
			Events: map[string]telemetry.EventDefinition{EventEntry: entryEvent, EventSubmission: submissionEvent, EventReport: reportEvent},
			Status: telemetry.StatusDefinition{Default: "ok", ErrorWhen: "Admission failed."},
		},
		SpanPhase: {
			Description: "One task phase invocation, including its decision commit, never a wait or restart.",
			Parents:     telemetry.ParentDefinition{Kind: telemetry.ParentsAny},
			StartAttributes: map[string]telemetry.AttributeDefinition{
				"task.id":   highCardinality(attr(telemetry.AttributeNumber, true, "Task ID.")),
				"task.name": attr(telemetry.AttributeString, true, "Registered task name."),
				"phase":     attr(telemetry.AttributeString, true, "Invoked checkpoint phase."),
				"kind":      withValues(attr(telemetry.AttributeString, true, "Invocation kind."), "run", "abort"),
				"attempt":   attr(telemetry.AttributeNumber, false, "Checkpoint attempt, when present."),
				"resumed":   attr(telemetry.AttributeBoolean, true, "First invocation of a task loaded in this process."),
			},
			EndAttributes: map[string]telemetry.AttributeDefinition{
				"outcome":      attr(telemetry.AttributeString, false, "next=<phase>, waiting, sleeping, done, faulted, or aborted."),
				"tool.name":    attr(telemetry.AttributeString, false, "Called tool name."),
				"tool.call_id": highCardinality(attr(telemetry.AttributeString, false, "Transcript tool call ID.")),
			},
			Events: map[string]telemetry.EventDefinition{
				EventEntry:      entryEvent,
				EventSubmission: submissionEvent,
				EventReport:     reportEvent,
				EventCheckpoint: {
					Description: "The running checkpoint's phase changed while the invocation continued.",
					Attributes:  map[string]telemetry.AttributeDefinition{"phase": attr(telemetry.AttributeString, true, "Committed checkpoint phase.")},
				},
				EventReplay: {
					Description: "A nested tool call was answered from its stored result.",
					Attributes:  map[string]telemetry.AttributeDefinition{"tool.name": attr(telemetry.AttributeString, true, "Called tool name.")},
				},
			},
			Status: telemetry.StatusDefinition{
				Default:   "ok",
				ErrorWhen: "Faulted, failed, and orphaned outcomes use task_faulted, task_failed, and task_orphaned with a diagnostic capped at 500 characters. Aborted outcomes keep status ok.",
			},
		},
	},
	Metrics: map[string]telemetry.MetricDefinition{
		MetricReports: {
			Kind:        telemetry.MetricCounter,
			Description: "Reports raised, grouped by their fixed ReportInfo type.",
			Attributes:  map[string]telemetry.AttributeDefinition{"type": reportTypeAttribute},
		},
		MetricPhaseDuration: {
			Kind:        telemetry.MetricHistogram,
			Unit:        "ms",
			Description: "Duration of a phase invocation and its decision commit.",
			Attributes: map[string]telemetry.AttributeDefinition{
				"task":    attr(telemetry.AttributeString, true, "Registered task name."),
				"phase":   attr(telemetry.AttributeString, true, "Invoked checkpoint phase."),
				"outcome": attr(telemetry.AttributeString, true, "Phase outcome."),
			},
		},
		MetricTasksActive: {
			Kind:        telemetry.MetricUpDownCounter,
			Description: "Live tasks held by this Harness; decremented at terminal settlement or close.",
			Attributes:  map[string]telemetry.AttributeDefinition{"task": attr(telemetry.AttributeString, true, "Registered task name.")},
		},
	},
}

// durableMetrics binds the adapter's metrics to TelemetrySchema; the schema
// has no duplicate, so binding cannot fail.
func durableMetrics(adapter telemetry.Context) *telemetry.TypedMetrics {
	metrics, err := telemetry.NewTypedMetrics(telemetry.MetricsOf(adapter), TelemetrySchema)
	if err != nil {
		panic(err)
	}
	return metrics
}

// reportFunc is a report with the context of the operation it belongs to.
type reportFunc func(ctx context.Context, err error, info durable.ReportInfo)

// safeReport returns the Harness report (pi safeReport): OnReport, which
// cannot fail the caller, so its own panic is dropped; then the report
// counter by type, and a report event, with fixed fields only, on the span of
// ctx. The adapter is responsible for its own failures.
func safeReport(onReport func(error, durable.ReportInfo), adapter telemetry.Context) reportFunc {
	reports := durableMetrics(adapter).Counter(MetricReports)
	return func(ctx context.Context, err error, info durable.ReportInfo) {
		if onReport != nil {
			func() {
				defer func() { _ = recover() }()
				onReport(err, info)
			}()
		}
		attributes := telemetry.SpanAttributes{"type": string(info.Type)}
		switch info.Type {
		case durable.ReportTaskFaulted, durable.ReportTaskOutdated:
			attributes["reason"] = info.Reason
			attributes["extension"] = info.Extension
			if info.Task != nil {
				attributes["task.id"] = int64(info.Task.ID)
			}
		case durable.ReportTaskParked, durable.ReportMigrationFailed, durable.ReportTaskError, durable.ReportExtensionError:
			attributes["extension"] = info.Extension
			if info.Task != nil {
				attributes["task.id"] = int64(info.Task.ID)
			}
		case durable.ReportHostError:
			if info.Task != nil {
				attributes["task.id"] = int64(info.Task.ID)
			}
		}
		// pi drops undefined fields; Go's empty strings are their absence.
		for key, value := range attributes {
			if value == "" {
				delete(attributes, key)
			}
		}
		reports.Add(1, telemetry.SpanAttributes{"type": string(info.Type)})
		telemetry.SpanFrom(ctx).AddEvent(EventReport, attributes)
	}
}

// background reports without an operation's context: no span gets the event.
func (r reportFunc) background() func(error, durable.ReportInfo) {
	return func(err error, info durable.ReportInfo) { r(context.Background(), err, info) }
}
