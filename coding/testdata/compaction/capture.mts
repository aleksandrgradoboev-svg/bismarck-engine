// Captures what real pi's prepareCompaction selects when the path carries
// system messages — the oracle behind TestPrepareCompactionMatchesPiCapture in
// coding/compaction_system_test.go.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> prepare-9e05370b2.json 9e05370b2
//
// <extraction> is laid out as for ../sessionprompt/capture.mts (packages/ai,
// packages/agent and packages/coding-agent at <sha>, the npm build's
// node_modules, the generated model catalog under
// packages/ai/src/providers/data, and the @earendil-works shims). These are src
// captures at 9e05370b2, which the npm build 0.85.1 predates: re-verify them
// against the first build that ships it (the BUILD wins).
//
// Each scenario is a linear path of message entries with no earlier compaction
// (boundaryStart 0). The output keeps the scenario's messages as pi reads them,
// and pi's preparation with its message lists as indexes into them, plus the
// serialized text of each list as the summarization request carries it.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const url = (file: string) => pathToFileURL(path.join(extraction, file)).href;
const { prepareCompaction } = await import(url("packages/coding-agent/src/core/compaction/compaction.ts"));
const { serializeConversation } = await import(url("packages/coding-agent/src/core/compaction/utils.ts"));
const { convertToLlm } = await import(url("packages/coding-agent/src/core/messages.ts"));

const big = "y".repeat(400); // 100 estimated tokens
const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
let clock = 0;
const user = (text: string) => ({ role: "user", content: [{ type: "text", text }], timestamp: ++clock });
const assistant = (text: string | null, calls: Array<[string, string, Record<string, unknown>]> = [], stopReason = "stop") => ({
	role: "assistant",
	content: [
		...(text === null ? [] : [{ type: "text", text }]),
		...calls.map(([id, name, args]) => ({ type: "toolCall", id, name, arguments: args })),
	],
	api: "openai-completions",
	provider: "p",
	model: "m",
	usage,
	stopReason,
	timestamp: ++clock,
});
const toolResult = (id: string, name: string, text: string) => ({
	role: "toolResult",
	toolCallId: id,
	toolName: name,
	content: [{ type: "text", text }],
	isError: false,
	timestamp: ++clock,
});
const readTool = { name: "read", description: "Read a file", parameters: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } };
const system = (fields: { content?: string; sections?: Record<string, string | null>; toolsAdded?: unknown[]; toolsRemoved?: unknown[] }) => ({
	role: "system",
	content: fields.content ?? "",
	...(fields.sections ? { sections: fields.sections } : {}),
	timestamp: ++clock,
	...(fields.toolsAdded ? { toolsAdded: fields.toolsAdded } : {}),
	...(fields.toolsRemoved ? { toolsRemoved: fields.toolsRemoved } : {}),
});

const scenarios: Array<{ name: string; keepRecentTokens: number; messages: any[] }> = [
	{
		name: "system-inside-turn-prefix",
		keepRecentTokens: 100,
		messages: [
			user(big),
			system({ content: "mid-turn note" }),
			assistant(big, [["r1", "read", { path: "/a" }]], "toolUse"),
			toolResult("r1", "read", big),
			system({ sections: { z: "<z>\n1\n</z>" }, toolsRemoved: [{ name: "read" }] }),
			assistant(big),
			user("small"),
		],
	},
	{
		name: "many-systems-split",
		keepRecentTokens: 120,
		messages: [
			system({ sections: { preamble: "a", cwd: "<cwd>\n/x\n</cwd>" }, toolsAdded: [readTool] }),
			user(big),
			system({ content: "c1" }),
			assistant(big),
			system({ sections: { cwd: null } }),
			assistant(big),
			system({ content: "c2" }),
			user("tail"),
		],
	},
	{
		name: "leading-system-in-history",
		keepRecentTokens: 100,
		messages: [
			system({ sections: { preamble: "p" }, toolsAdded: [readTool] }),
			user(big),
			assistant(big),
			system({ sections: { preamble: "q" } }),
			user(big),
			assistant("s"),
		],
	},
	{
		name: "crossing-on-tool-result-before-system",
		keepRecentTokens: 150,
		messages: [
			user(big),
			assistant(null, [["t", "read", { path: "/x" }]], "toolUse"),
			toolResult("t", "read", big),
			system({ sections: { preamble: "patched" } }),
			user(big),
		],
	},
	{
		name: "only-system-history",
		keepRecentTokens: 150,
		messages: [system({ sections: { preamble: "prompt" } }), user(big), assistant(big)],
	},
];

const iso = (i: number) => new Date(1789603200000 + i * 1000).toISOString();
const out = {
	sha,
	scenarios: scenarios.map(({ name, keepRecentTokens, messages }) => {
		let parentId: string | null = null;
		const entries = messages.map((message, i) => {
			const entry = { type: "message", id: `m${i}`, parentId, timestamp: iso(i), message };
			parentId = entry.id;
			return entry;
		});
		const preparation = prepareCompaction(entries, { enabled: true, reserveTokens: 200, keepRecentTokens });
		const indexes = (list: unknown[]) => list.map((message) => messages.indexOf(message));
		return {
			name,
			keepRecentTokens,
			messages,
			preparation: preparation
				? {
						firstKeptIndex: entries.findIndex((entry) => entry.id === preparation.firstKeptEntryId),
						isSplitTurn: preparation.isSplitTurn,
						messagesToSummarize: indexes(preparation.messagesToSummarize),
						turnPrefixMessages: indexes(preparation.turnPrefixMessages),
						historyText: serializeConversation(convertToLlm(preparation.messagesToSummarize)),
						turnPrefixText: serializeConversation(convertToLlm(preparation.turnPrefixMessages)),
					}
				: null,
		};
	}),
};
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
