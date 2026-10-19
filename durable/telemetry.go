package durable

import (
	"context"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// taskTrace is a new task's stored telemetry parent (pi createTask): the
// explicit carrier, none for TraceRoot, or the carrier of the commit's span,
// which is nil without propagation.
func taskTrace(ctx context.Context, options TaskOptions) telemetry.Carrier {
	if options.TraceRoot {
		return nil
	}
	if options.Trace != nil {
		return options.Trace
	}
	span := telemetry.SpanFrom(ctx)
	return telemetry.PropagationOf(span).Inject(span)
}
