package providers

import (
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Identity and OpenCode session headers at the bottom of a provider request's
// header object.
//
// pi (upstream adc5fa9f, 04a93bad) opens every adapter's header merge with
// getIdentityHeaders(model, options.identity) — the User-Agent, then the
// provider attribution of the request's identity — so model.headers, the
// adapter's own headers and the consumer's options.headers all outrank them,
// and a deletion marker in any of them removes one. The identity, and so the
// attribution, is the request's: pi-ai's default sends none, and this port
// has no environment switch of its own any more (the PI_TELEMETRY gate it
// used to read lives in the coding agent's identity, coding.ClientIdentity).
//
// x-opencode-session is functional rather than attribution: OpenCode routes a
// conversation by it. pi adds it in the opencode/opencode-go provider tables
// (561a2e066, into options.headers unless a source already names it) and in
// Models for any OpenCode target (04a93bad). The port adds it here for every
// request to an OpenCode target, below model.Headers — the bare-path cells
// tracked as Scope queue entry 15 — and Models adds it above them, as pi's.

// mergeIdentityHeaders folds the request identity's headers into o: the
// User-Agent first, then the attribution headers for model's provider and
// base URL (pi getIdentityHeaders).
func mergeIdentityHeaders(o *headerObject, model *ai.Model, identity *ai.ClientIdentity) {
	o.set("User-Agent", ai.FormatUserAgent(identity))
	o.mergeStrings(ai.AttributionHeaders(model, identity))
}

// mergeOpenCodeSession folds OpenCode's session header into o for a request
// to an OpenCode target with a session id.
func mergeOpenCodeSession(o *headerObject, model *ai.Model, sessionID string) {
	o.merge(ai.OpenCodeSessionHeaders(model, sessionID))
}
