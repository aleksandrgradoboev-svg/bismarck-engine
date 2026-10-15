// Captures the usage real pi reports when an Anthropic Messages stream carries
// the 1h cache-write breakdown (usage.cache_creation.ephemeral_1h_input_tokens)
// — the oracle behind TestAnthropic1hCacheWriteMatchesPi (upstream 667fc3dd3).
//
//   node --experimental-strip-types capture-cache-write-1h.mts <extraction> <pi npm dir> <out.json> <sha>
//   e.g. ... capture-cache-write-1h.mts <dir> ~/.cache/pi-npm/0.87.1 cache-write-1h-8676a0dcd.json 8676a0dcd
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone), with packages/ai/node_modules
// resolving pi-ai's dependencies. The npm build's node_modules serves once its
// copies match what <sha>'s package-lock.json locks; at 8676a0dcd that is
// @anthropic-ai/sdk 0.124.0 and partial-json 0.1.7, integrity-identical to
// ~/.cache/pi-npm/0.87.1's. The adapter runs from SOURCE because no published
// build carries 667fc3dd3 yet; re-capture against the first build that does.
//
// The models are the npm build's MODELS entries — the catalog the port embeds
// (ai/models_catalog.json) — and each row records the rates pi priced it at;
// the Go test replays the row at those rates, so a later catalog regen cannot
// move the expectation under it.
//
// Every row streams through the upstream suite's fake client
// (anthropic-cache-write-1h-cost.test.ts createSseResponse /
// createFakeAnthropicClient), so neither side needs a key or the network. A row
// records the exact SSE body; the Go test replays it byte for byte.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, npmDir, outFile, sha] = process.argv.slice(2);
if (!extraction || !npmDir || !outFile || !sha) {
	console.error(
		"usage: node --experimental-strip-types capture-cache-write-1h.mts <extraction> <pi npm dir> <out.json> <sha>",
	);
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const { stream: streamAnthropic } = await import(pathToFileURL(path.join(src, "api/anthropic-messages.ts")).href);
const { normalizeContext } = await import(pathToFileURL(path.join(src, "utils/transcript.ts")).href);
const pkg = path.join(npmDir, "node_modules/@earendil-works/pi-ai");
const version = JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version;
const { MODELS } = await import(pathToFileURL(path.join(pkg, "dist/models.generated.js")).href);

type SseEvent = { event: string; data: string };

function sseBody(events: SseEvent[]): string {
	return events.map(({ event, data }) => `event: ${event}\ndata: ${data}\n`).join("\n");
}

function createFakeAnthropicClient(body: string): any {
	const response = new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	return { beta: { messages: { create: () => ({ asResponse: async () => response }) } } };
}

const start = (usage: object): SseEvent => ({
	event: "message_start",
	data: JSON.stringify({ type: "message_start", message: { id: "msg_test", usage } }),
});
const delta = (usage: object): SseEvent => ({
	event: "message_delta",
	data: JSON.stringify({ type: "message_delta", delta: { stop_reason: "end_turn" }, usage }),
});
const stop: SseEvent = { event: "message_stop", data: JSON.stringify({ type: "message_stop" }) };

// The start both opus rows open with: a 1000-token write, 400 of it at 1h.
const opusStart = start({
	input_tokens: 1,
	output_tokens: 0,
	cache_creation_input_tokens: 1000,
	cache_creation: { ephemeral_1h_input_tokens: 400 },
});

// [name, provider, model id, events]
const cases: Array<[string, string, string, SseEvent[]]> = [
	// Upstream's own case, 'prices 1h cache writes reported only in message_delta'
	// (#9210): the whole breakdown arrives on the delta.
	[
		"deltaOnly",
		"vercel-ai-gateway",
		"anthropic/claude-haiku-4.5",
		[
			start({ input_tokens: 0, output_tokens: 0 }),
			delta({
				input_tokens: 3,
				output_tokens: 4,
				cache_creation_input_tokens: 6535,
				cache_creation: { ephemeral_5m_input_tokens: 0, ephemeral_1h_input_tokens: 6535 },
			}),
			stop,
		],
	],
	// The delta's breakdown is read even with no cache_creation_input_tokens
	// beside it: CacheWrite keeps the start's 100 while CacheWrite1h takes 6535,
	// and the negative 5m remainder is priced as calculateCost prices it.
	[
		"deltaBreakdownWithoutTotal",
		"vercel-ai-gateway",
		"anthropic/claude-haiku-4.5",
		[
			start({ input_tokens: 0, output_tokens: 0, cache_creation_input_tokens: 100 }),
			delta({ output_tokens: 4, cache_creation: { ephemeral_1h_input_tokens: 6535 } }),
			stop,
		],
	],
	// A delta breakdown without the 1h key leaves the start's value.
	[
		"deltaBreakdownWithout1hKeepsStart",
		"anthropic",
		"claude-opus-4-8",
		[opusStart, delta({ output_tokens: 4, cache_creation: { ephemeral_5m_input_tokens: 600 } }), stop],
	],
	// So does a null cache_creation...
	[
		"deltaCacheCreationNullKeepsStart",
		"anthropic",
		"claude-opus-4-8",
		[opusStart, delta({ output_tokens: 4, cache_creation: null }), stop],
	],
	// ...and a null 1h count (`!= null`).
	[
		"delta1hNullKeepsStart",
		"anthropic",
		"claude-opus-4-8",
		[opusStart, delta({ output_tokens: 4, cache_creation: { ephemeral_1h_input_tokens: null } }), stop],
	],
	// An explicit 0 is a value, not an absence: it replaces the start's 400.
	[
		"deltaExplicitZero1h",
		"anthropic",
		"claude-opus-4-8",
		[opusStart, delta({ output_tokens: 4, cache_creation: { ephemeral_1h_input_tokens: 0 } }), stop],
	],
	// message_start assigns unconditionally
	// (`cache_creation?.ephemeral_1h_input_tokens || 0`), so a second start
	// with no breakdown resets the first start's 400...
	[
		"secondStartWithoutBreakdownResets",
		"anthropic",
		"claude-opus-4-8",
		[
			opusStart,
			start({ input_tokens: 1, output_tokens: 0, cache_creation_input_tokens: 1000 }),
			delta({ output_tokens: 4 }),
			stop,
		],
	],
	// ...as does one whose cache_creation is null...
	[
		"secondStartCacheCreationNullResets",
		"anthropic",
		"claude-opus-4-8",
		[
			opusStart,
			start({ input_tokens: 1, output_tokens: 0, cache_creation_input_tokens: 1000, cache_creation: null }),
			delta({ output_tokens: 4 }),
			stop,
		],
	],
	// ...or whose 1h count is null.
	[
		"secondStart1hNullResets",
		"anthropic",
		"claude-opus-4-8",
		[
			opusStart,
			start({
				input_tokens: 1,
				output_tokens: 0,
				cache_creation_input_tokens: 1000,
				cache_creation: { ephemeral_1h_input_tokens: null },
			}),
			delta({ output_tokens: 4 }),
			stop,
		],
	],
];

const context = normalizeContext({ messages: [{ role: "user", content: "hi", timestamp: 1 }] });
const rows = [];
for (const [name, provider, id, events] of cases) {
	const model = MODELS[provider]?.[id];
	if (!model) throw new Error(`catalog has no ${provider}/${id}`);
	const sse = sseBody(events);
	const message = await streamAnthropic(model, context, { client: createFakeAnthropicClient(sse) }).result();
	if (message.stopReason !== "stop") throw new Error(`${name}: stopReason ${message.stopReason}: ${message.errorMessage}`);
	rows.push({ name, model: `${provider}/${id}`, rates: model.cost, sse, usage: message.usage });
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(outFile, `{"sha":${JSON.stringify(sha)},"pi-ai":${JSON.stringify(version)},"rows":[\n${text}\n]}\n`);
console.log(`captured ${rows.length} rows from packages/ai/src at ${sha} (models: pi-ai ${version}) -> ${outFile}`);
