package coding

import (
	"os"
	"strings"

	engine "github.com/aleksandrgradoboev-svg/bismarck-engine"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// ClientIdentity is how the coding agent's requests identify it to providers
// (pi coding-agent getClientIdentity, upstream adc5fa9f, 04a93bad): a
// detailed User-Agent with the engine's version, and attribution to pi while
// install telemetry is on.
//
// DIVERGENCE (bismarck-engine): pi's install telemetry is on unless a setting
// or PI_TELEMETRY turns it off; here only a truthy PI_TELEMETRY turns it on,
// the opt-in the engine's provider attribution has always had. Off, the
// identity says so explicitly (attribution null), so it also clears an
// attribution a Models default carries.
func ClientIdentity() *ai.ClientIdentity {
	detailed := ai.UserAgentDetailed
	identity := &ai.ClientIdentity{UserAgent: &detailed, Version: engine.Version()}
	if installTelemetryEnabled() {
		attribution := ai.PIAttribution
		attribution.Categories = append([]string(nil), ai.PIAttribution.Categories...)
		identity.Attribution = &attribution
	} else {
		identity.NoAttribution = true
	}
	return identity
}

// installTelemetryEnabled reports whether PI_TELEMETRY is truthy (pi
// isTruthyEnvFlag: "1", "true" or "yes", the last two in any case).
func installTelemetryEnabled() bool {
	value := os.Getenv("PI_TELEMETRY")
	lower := strings.ToLower(value)
	return value == "1" || lower == "true" || lower == "yes"
}
