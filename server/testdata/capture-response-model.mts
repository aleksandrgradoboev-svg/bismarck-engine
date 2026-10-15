// Captures what real pi puts on the protocol wire for an assistant message whose
// provider reported a model other than the requested one — the oracle behind
// TestAssistantResponseModelReachesTheWire (server/response_model_test.go).
//
//   node --experimental-strip-types capture-response-model.mts <extraction> <out.json> <sha>
//   e.g. ... capture-response-model.mts <dir> response-model-48bfdbaff.json 48bfdbaff
//
// Since upstream 1283afd0d the Anthropic adapter keeps the REQUESTED id as the
// message's model and records a relay's relabel or a refusal fallback only as
// responseModel, so toProtocolAssistantMessage's responseModel arm is the one
// path either reaches a protocol client by.
//
// <sha> is 48bfdbaff, the last upstream commit carrying
// packages/server/src/protocol.ts (e52de91d0 deleted it); since 6189e53b3 the
// file changed only in its type-level field assertions. <extraction> holds
// packages/{ai,protocol,server} at <sha> (`git archive <sha> packages/ai
// packages/protocol packages/server` from the upstream clone), and a
// node_modules resolving typebox@1.3.7 plus @earendil-works/pi-ai and
// @earendil-works/pi-protocol as shims whose "." export is the extracted
// packages/<name>/src/index.ts. The messages are this script's own; every item
// and frame is what pi's toProtocolAssistantMessage and encodeServerMessage
// produced for them.
import path from "node:path";
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-response-model.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const load = (file: string) => import(pathToFileURL(path.join(extraction, "packages", file)).href);
const { toProtocolAssistantMessage } = await load("server/src/protocol.ts");
const { encodeServerMessage } = await load("protocol/src/codec.ts");

// An Anthropic turn the relay relabeled, as the adapter now returns it.
const relabeled = {
	role: "assistant",
	content: [{ type: "text", text: "done" }],
	api: "anthropic-messages",
	provider: "anthropic",
	model: "claude-opus-5",
	responseModel: "kimi-for-coding",
	usage: {
		input: 100,
		output: 20,
		cacheRead: 0,
		cacheWrite: 0,
		totalTokens: 120,
		cost: { input: 0.0005, output: 0.0005, cacheRead: 0, cacheWrite: 0, total: 0.001 },
		cacheWrite1h: 0,
	},
	stopReason: "stop",
	timestamp: 1001,
};
const { responseModel: _responseModel, ...requestedOnly } = relabeled;

const cases: Array<{ name: string; progress: "item_updated" | "item_finished"; message: unknown }> = [
	// A finished turn names the requested model and carries the relabel beside it.
	{ name: "complete", progress: "item_finished", message: relabeled },
	// So does one still streaming...
	{ name: "streaming", progress: "item_updated", message: { ...relabeled, stopReason: "pending" } },
	// ...an errored one...
	{
		name: "error",
		progress: "item_finished",
		message: {
			...relabeled,
			stopReason: "error",
			errorMessage: "Anthropic performed an unsupported mid-output model fallback",
		},
	},
	// ...and an aborted one.
	{
		name: "aborted",
		progress: "item_finished",
		message: { ...relabeled, stopReason: "aborted", errorMessage: "Request was aborted" },
	},
	// A turn served by the requested model has no responseModel on the wire.
	{ name: "requested only", progress: "item_finished", message: requestedOnly },
];

const out = {
	sha,
	cases: cases.map(({ name, progress, message }) => {
		const item = toProtocolAssistantMessage(message, { id: "a1" });
		const frame = encodeServerMessage({
			type: "event",
			event: { type: "session_progress", sessionId: "s1", progress: { type: progress, item } },
		});
		return { name, progress, message, item, frame: Buffer.from(frame).toString("hex") };
	}),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
