// Captures real pi's per-message token estimate (compaction's estimateTokens:
// ceil(chars / 4), chars in UTF-16 code units) — the oracle behind
// TestEstimateMessageTokensMatchesPiCapture in coding/compaction_estimate_test.go.
//
//   node capture-estimate.mts <entry> <out.json>
//   e.g. node capture-estimate.mts ~/.cache/pi-npm/0.87.0/node_modules/@earendil-works/pi-coding-agent/dist/index.js estimate-0.87.0.json
//
// <entry> is a module exporting estimateTokens: the npm build's dist/index.js
// (0.87.0 is the first build that estimates system messages, upstream
// 466db0fec). The output pairs each message, as pi reads it, with pi's
// estimate. The messages carry astral characters, HTML-significant characters
// and U+2028, where JS string length and JSON.stringify differ from Go's byte
// length and json.Marshal.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [entry, outFile] = process.argv.slice(2);
if (!entry || !outFile) {
	console.error("usage: node capture-estimate.mts <entry> <out.json>");
	process.exit(2);
}
const { estimateTokens } = await import(pathToFileURL(path.resolve(entry)).href);

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const tool = {
	name: "edit",
	description: "Replace <old> with <new> & keep the rest \u2028 intact 🙂",
	parameters: {
		type: "object",
		properties: {
			path: { type: "string", description: "File path, e.g. src/a&b.ts" },
			count: { type: "integer", minimum: 1, default: 1 },
			mode: { type: "string", enum: ["exact", "fuzzy"] },
		},
		required: ["path"],
		additionalProperties: false,
	},
};
const messages = [
	{
		role: "system",
		content: "You are pi. ✨ Follow AGENTS.md.",
		sections: { preamble: "Be brief 🙂", empty: "", removed: null, env: "cwd: /tmp/<p>" },
		toolsAdded: [tool, { name: "read", description: "Read a file", parameters: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } }],
		toolsRemoved: [{ name: "gone-and-not-counted" }],
		timestamp: 1,
	},
	{
		role: "system",
		content: [{ type: "text", text: "block prompt 😀" }, { type: "image", data: "aGk=", mimeType: "image/png" }],
		timestamp: 2,
	},
	{ role: "system", content: "", toolsAdded: [], timestamp: 3 },
	{ role: "system", content: "", sections: { only: "x" }, timestamp: 4 },
	{ role: "user", content: "héllo 😀 wörld", timestamp: 5 },
	{ role: "user", content: [{ type: "text", text: "日本語のテキスト" }, { type: "image", data: "aGk=", mimeType: "image/png" }], timestamp: 6 },
	{
		role: "assistant",
		content: [
			{ type: "text", text: "Sure 🚀" },
			{ type: "thinking", thinking: "consider <a> & ü" },
			{ type: "toolCall", id: "c1", name: "edit", arguments: { path: "a<b>&c.ts", note: "😀\u2028" } },
		],
		api: "openai-completions",
		provider: "p",
		model: "m",
		usage,
		stopReason: "toolUse",
		timestamp: 7,
	},
	{ role: "toolResult", toolCallId: "c1", toolName: "edit", content: [{ type: "text", text: "done ✓ 😀" }], isError: false, timestamp: 8 },
	// JSON lengths of 65 and 13: counting the astral character as one code unit
	// instead of two lands on a multiple of four and drops a token.
	{ role: "system", content: "", toolsAdded: [{ name: "😀", description: "dd", parameters: { type: "object" } }], timestamp: 9 },
	{
		role: "assistant",
		content: [{ type: "toolCall", id: "c2", name: "tcx", arguments: { e: "😀" } }],
		api: "openai-completions",
		provider: "p",
		model: "m",
		usage,
		stopReason: "toolUse",
		timestamp: 10,
	},
];
fs.writeFileSync(outFile, JSON.stringify(messages.map((message) => ({ message, tokens: estimateTokens(message) })), null, 2));
