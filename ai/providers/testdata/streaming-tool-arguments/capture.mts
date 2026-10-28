// Captures the partial arguments pi's providers show while a tool call's
// arguments stream (upstream f715a15c, #9265: every provider reads the
// argument deltas through ToolCallArgumentsStream). It is the oracle behind
// TestStreamingToolArgumentsMatchPi.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> streaming-tool-arguments-f715a15c.json f715a15c
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone, with packages/ai/node_modules resolving pi-ai's dependencies
// at the versions <sha>'s package-lock.json locks (@anthropic-ai/sdk 0.129.0,
// openai 7.19.0 at f715a15c; the adapters run from SOURCE). Captured at
// f715a15c on 2026-10-28 with node v24.18.0.
//
// Each case is one tool call whose argument text streams in pieces of `chunk`
// characters, through the Anthropic, OpenAI Completions, OpenAI Responses and
// pi-messages adapters, each served its own body through options.fetch, and,
// for argument text that is a JSON object with ASCII text, through the faux
// provider (whose pieces are its own: four characters). Each provider records:
//   body       the exact response body, which the Go test replays;
//   deltas     for every toolcall_delta, the delta and JSON.stringify of the
//              call's arguments AS THE EVENT WAS PUSHED (pi's events carry the
//              live message, which the stream mutates after the push, so they
//              are read in a patched push, not where they are consumed);
//   end        the toolcall_end call's arguments and argumentsError;
//   stopReason and errorMessage of the result.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const src = path.join(extraction, "packages/ai/src");
const load = (file: string) => import(pathToFileURL(path.join(src, file)).href);
const { AssistantMessageEventStream } = await load("utils/event-stream.ts");
const { stream: streamAnthropic } = await load("api/anthropic-messages.ts");
const { stream: streamCompletions } = await load("api/openai-completions.ts");
const { stream: streamResponses } = await load("api/openai-responses.ts");
const { stream: streamPiMessages } = await load("api/pi-messages.ts");
const { fauxProvider, fauxAssistantMessage, fauxToolCall } = await load("providers/faux.ts");
const { normalizeContext } = await load("utils/transcript.ts");

type Recorded = { delta: string; args: string };
let recording: { starts: string[]; deltas: Recorded[]; end?: unknown } | undefined;
const push = AssistantMessageEventStream.prototype.push;
AssistantMessageEventStream.prototype.push = function (event: any) {
	if (recording && event.type === "toolcall_start") {
		recording.starts.push(JSON.stringify(event.partial.content[event.contentIndex]?.arguments));
	} else if (recording && event.type === "toolcall_delta") {
		const call = event.partial.content[event.contentIndex];
		recording.deltas.push({ delta: event.delta, args: JSON.stringify(call?.arguments) });
	} else if (recording && event.type === "toolcall_end") {
		const { arguments: args, argumentsError } = event.toolCall;
		recording.end = argumentsError === undefined ? { arguments: args } : { arguments: args, argumentsError };
	}
	return push.call(this, event);
};

const sse = (data: unknown, event?: string) => `${event ? `event: ${event}\n` : ""}data: ${JSON.stringify(data)}\n\n`;
const fetchOf = (body: string) => async () =>
	new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
// Pieces of `size` characters, never splitting a surrogate pair: a wire's
// UTF-8 cannot, and a Go string cannot hold half of one.
const pieces = (text: string, size: number) => {
	const characters = Array.from(text);
	const out: string[] = [];
	for (let i = 0; i < characters.length; i += size) out.push(characters.slice(i, i + size).join(""));
	return out;
};

const baseModel = {
	reasoning: false,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 200_000,
	maxTokens: 64_000,
};
const models = {
	anthropic: { ...baseModel, id: "claude-test", name: "c", api: "anthropic-messages", provider: "anthropic", baseUrl: "https://api.anthropic.com" },
	completions: { ...baseModel, id: "gpt-test", name: "g", api: "openai-completions", provider: "openai", baseUrl: "https://api.openai.com/v1" },
	responses: { ...baseModel, id: "gpt-test", name: "g", api: "openai-responses", provider: "openai", baseUrl: "https://api.openai.com/v1" },
	piMessages: { ...baseModel, id: "auto", name: "a", api: "pi-messages", provider: "radius", baseUrl: "https://example.test/v1" },
};
const context = normalizeContext({ messages: [{ role: "user", content: "write", timestamp: 0 }] });

const bodies = {
	anthropic: (deltas: string[]) =>
		sse({ type: "message_start", message: { id: "msg_1", model: "claude-test", usage: { input_tokens: 1, output_tokens: 1 } } }, "message_start") +
		sse({ type: "content_block_start", index: 0, content_block: { type: "tool_use", id: "toolu_1", name: "write", input: {} } }, "content_block_start") +
		deltas.map((d) => sse({ type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: d } }, "content_block_delta")).join("") +
		sse({ type: "content_block_stop", index: 0 }, "content_block_stop") +
		sse({ type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { output_tokens: 10 } }, "message_delta") +
		sse({ type: "message_stop" }, "message_stop"),
	completions: (deltas: string[]) => {
		const base = { id: "chat_1", object: "chat.completion.chunk", created: 1, model: "gpt-test" };
		const call = (fields: object) => ({ ...base, choices: [{ index: 0, delta: { tool_calls: [{ index: 0, ...fields }] } }] });
		return (
			sse(call({ id: "call_1", type: "function", function: { name: "write", arguments: "" } })) +
			deltas.map((d) => sse(call({ function: { arguments: d } }))).join("") +
			sse({ ...base, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) +
			"data: [DONE]\n\n"
		);
	},
	responses: (deltas: string[], text: string) => {
		const item = { type: "function_call", id: "fc_1", call_id: "call_1", name: "write", arguments: "", status: "in_progress" };
		let sequence = 0;
		const event = (fields: object) => sse({ ...fields, sequence_number: sequence++ });
		return (
			event({ type: "response.created", response: { id: "resp_1", status: "in_progress", output: [] } }) +
			event({ type: "response.output_item.added", output_index: 0, item }) +
			deltas.map((delta) => event({ type: "response.function_call_arguments.delta", output_index: 0, item_id: "fc_1", delta })).join("") +
			event({ type: "response.function_call_arguments.done", output_index: 0, item_id: "fc_1", arguments: text }) +
			event({ type: "response.output_item.done", output_index: 0, item: { ...item, arguments: text, status: "completed" } }) +
			event({ type: "response.completed", response: { id: "resp_1", status: "completed", output: [], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } })
		);
	},
	piMessages: (deltas: string[], _text: string, args: unknown) =>
		sse({ type: "start" }) +
		sse({ type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "write" }) +
		deltas.map((delta) => sse({ type: "toolcall_delta", contentIndex: 0, delta })).join("") +
		sse({ type: "toolcall_end", contentIndex: 0, toolCall: { type: "toolCall", id: "call_1", name: "write", arguments: args } }) +
		sse({ type: "done", reason: "toolUse", usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } }),
};
const streams = {
	anthropic: (body: string) => streamAnthropic(models.anthropic, context, { apiKey: "sk-ant-test", fetch: fetchOf(body) }),
	completions: (body: string) => streamCompletions(models.completions, context, { apiKey: "sk-test", fetch: fetchOf(body) }),
	responses: (body: string) => streamResponses(models.responses, context, { apiKey: "sk-test", fetch: fetchOf(body) }),
	piMessages: (body: string) => streamPiMessages(models.piMessages, context, { apiKey: "test", fetch: fetchOf(body) }),
};

async function record(run: () => any) {
	recording = { starts: [], deltas: [] };
	const events = run();
	for await (const _ of events);
	const result = await events.result();
	const out = { ...recording, stopReason: result.stopReason, errorMessage: result.errorMessage ?? null };
	recording = undefined;
	return out;
}

const BS = String.fromCharCode(92); // a backslash, spelled so no tool decodes an escape
// pi's streaming-tool-arguments.test.ts writeCall, shorter.
const content = Array.from({ length: 12 }, (_, line) => `const line${line} = "value ${line}"; // ${BS} done\n`).join("");
// Argument texts, as the model writes them (not all of them valid JSON).
const texts: Record<string, string> = {
	write: JSON.stringify({ path: "/src/file.ts", content }),
	trailingSpaces: `{"a":"x   ","b":"  y  "}`,
	trailingSpacesCutOff: `{"a":"x   `,
	rawControlCharacters: `{"a":"x\ty\u0001z\nw"}`,
	invalidEscape: `{"re":"${BS}d+${BS}s","u":"${BS}usepackage"}`,
	nonFinite: `{"a":NaN,"b":Infinity,"c":-Infinity,"d":1}`,
	nested: `{"o":{"p":[1,{"q":true},[]],"r":null,"e":{}},"s":"t","f":false}`,
	numbers: `{"n":-12.5e+3,"m":0,"z":-0,"big":12345678901234567890,"tiny":1e-400,"huge":1e400}`,
	unicodeEscapes: `{"u":"${BS}u00e9${BS}ud83d${BS}ude00x${BS}ud800y"}`,
	literalText: `{"u":"é😀漢","k😀":1}`,
	keyOrder: `{"b":1,"a":2,"10":3,"2":4,"-1":5}`,
	duplicateKeys: `{"a":1,"b":2,"a":3}`,
	escapes: `{"s":"a${BS}"b${BS}${BS}c${BS}/${BS}b${BS}f${BS}n${BS}r${BS}t"}`,
	missingComma: `{"a":1 "b":2}`,
	missingColon: `{"a" 1,"b":2}`,
	trailingText: `{"a":1} trailing`,
	array: `[1,2,3]`,
	string: `"text"`,
	whitespaceAround: `  { "a" : [ 1 , 2 ] , "b" : "c" }  `,
	partialLiterals: `{"t":true,"f":false,"n":null,"x":tru}`,
	emptyObject: `{}`,
	notJSON: `not json at all`,
};
// Piece sizes; the long write call only in pi's test's 24, to keep the file
// small.
const chunksOf = (name: string) => (name === "write" ? [24] : [1, 3, 7]);

const cases = [];
for (const [name, text] of Object.entries(texts)) {
	for (const chunk of chunksOf(name)) {
		const deltas = pieces(text, chunk);
		let args: unknown = {};
		try {
			const parsed = JSON.parse(text);
			if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) args = parsed;
		} catch {}
		const providers: Record<string, unknown> = {};
		for (const provider of ["anthropic", "completions", "responses", "piMessages"] as const) {
			const body = bodies[provider](deltas, text, args);
			providers[provider] = { body, ...(await record(() => streams[provider](body))) };
		}
		cases.push({ name, chunk, text, providers });
	}
	// The faux provider streams JSON.stringify of the call's arguments in
	// pieces of its own.
	let parsed: unknown;
	try {
		parsed = JSON.parse(text);
	} catch {
		continue;
	}
	if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) continue;
	const fauxText = JSON.stringify(parsed);
	if (!/^[\x20-\x7e]*$/.test(fauxText)) continue;
	const faux = fauxProvider({ tokenSize: { min: 1, max: 1 } });
	faux.setResponses([fauxAssistantMessage([fauxToolCall("write", parsed, { id: "call_1" })], { stopReason: "toolUse" })]);
	cases.push({
		name,
		chunk: 4,
		text: fauxText,
		providers: { faux: await record(() => faux.provider.streamSimple(faux.getModel(), context, {})) },
	});
}

// Bodies of their own, for the paths the generated ones do not take (text is
// unused for them).
const special: Array<{ name: string; provider: keyof typeof streams; body: string }> = [
	// A function_call item's own arguments show from its start event.
	{
		name: "responsesItemCarriesArguments",
		provider: "responses",
		body: bodies
			.responses(['2}'], '{"a":1,"b":2}')
			.replace('"arguments":"","status":"in_progress"', '"arguments":"{\\"a\\":1,\\"b\\":","status":"in_progress"'),
	},
	// A custom call's initial input goes with its first delta, and a
	// custom_tool_call_input.done or output_item.done without an input closes
	// with the input so far.
	{
		name: "responsesCustomInputClosedWithoutInput",
		provider: "responses",
		body: (() => {
			const item = { type: "custom_tool_call", id: "ctc_1", call_id: "call_1", name: "g", input: "ab" };
			const { input: _, ...done } = item;
			return (
				sse({ type: "response.output_item.added", output_index: 0, item }) +
				sse({ type: "response.custom_tool_call_input.delta", output_index: 0, item_id: "ctc_1", delta: "c" }) +
				sse({ type: "response.custom_tool_call_input.done", output_index: 0, item_id: "ctc_1" }) +
				sse({ type: "response.output_item.done", output_index: 0, item: done }) +
				sse({ type: "response.completed", response: { id: "resp_1", status: "completed", output: [], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } })
			);
		})(),
	},
	// A Chat Completions custom call takes custom input only; function
	// arguments in the same or a later delta are dropped.
	{
		name: "completionsCustomCallDropsFunctionArguments",
		provider: "completions",
		body: (() => {
			const base = { id: "chat_1", object: "chat.completion.chunk", created: 1, model: "gpt-test" };
			const call = (fields: object) => ({ ...base, choices: [{ index: 0, delta: { tool_calls: [{ index: 0, ...fields }] } }] });
			return (
				sse(call({ id: "call_1", type: "custom", custom: { name: "g", input: "ab" } })) +
				sse(call({ function: { arguments: '{"x":1}' }, custom: { input: "c" } })) +
				sse(call({ function: { arguments: '{"y":2}' } })) +
				sse({ ...base, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } }) +
				"data: [DONE]\n\n"
			);
		})(),
	},
];
for (const { name, provider, body } of special) {
	cases.push({ name, chunk: 0, text: "", providers: { [provider]: { body, ...(await record(() => streams[provider](body))) } } });
}

fs.writeFileSync(outFile, `{"sha":${JSON.stringify(sha)},"node":${JSON.stringify(process.version)},"cases":[\n${cases.map((c) => JSON.stringify(c)).join(",\n")}\n]}\n`);
console.log(`captured ${cases.length} cases from packages/ai/src at ${sha} -> ${outFile}`);
