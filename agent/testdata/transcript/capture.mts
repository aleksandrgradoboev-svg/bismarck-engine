// Captures what real pi's Agent and agent loop do with transcript-owned system
// prompts and tool loadouts — the oracle behind agent/transcript_test.go.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> agent-95fbc0499.json 95fbc0499
//
// <extraction> is either
//   - a src extraction: packages/agent and packages/ai at <sha>
//     (`git archive <sha> packages/agent packages/ai` from the upstream clone), a
//     node_modules resolving their dependencies (the npm build's), and
//     packages/agent/node_modules/@earendil-works/pi-ai resolving to
//     packages/ai/src; or
//   - an npm build (~/.cache/pi-npm/<version>), whose pi-agent-core is the one
//     pi-coding-agent depends on.
//
// agent-95fbc0499.json was captured 2026-09-22 from a src extraction at
// 95fbc0499, then re-captured from the npm build 0.87.0 (v0.87.0 = 16787ad5b,
// the first build that ships 9e05370b2 and 466db0fec): byte-identical but for
// "sha". packages/agent/src is unchanged between 466db0fec and 95fbc0499, and
// the only packages/ai/src change in v0.87.0..95fbc0499 (1b6ddca87,
// openai-completions) is not on these paths. 466db0fec replaced
// shouldStopAfterTurn with finishTurn, so the two scenarios that stopped a run
// through that hook now return { action: "end" } from finishTurn, as the
// renamed upstream tests do.
//
// Each scenario is the fixture of a packages/agent/test case at the sha
// (agent.test.ts, agent-loop.test.ts) or a direct probe of agent-loop.ts's
// declareToolChanges through the public loop. Date.now is pinned to NOW so
// every timestamp the loop stamps is recognisable; the Go test maps its own
// wall-clock stamps onto NOW before comparing bytes.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const NOW = 1700000000000;
Date.now = () => NOW;

const url = (file: string) => pathToFileURL(path.join(extraction, file)).href;
const fromSrc = fs.existsSync(path.join(extraction, "packages/agent/src/agent.ts"));
const buildCore = "node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works";
const buildAI = fs.existsSync(path.join(extraction, buildCore, "pi-ai"))
	? `${buildCore}/pi-ai`
	: "node_modules/@earendil-works/pi-ai";
const { Agent } = await import(url(fromSrc ? "packages/agent/src/agent.ts" : `${buildCore}/pi-agent-core/dist/agent.js`));
const { agentLoop, agentLoopContinue } = await import(
	url(fromSrc ? "packages/agent/src/agent-loop.ts" : `${buildCore}/pi-agent-core/dist/agent-loop.js`)
);
const { EventStream, getCurrentSystemMessage, toToolDeclaration } = await import(
	url(fromSrc ? "packages/ai/src/index.ts" : `${buildAI}/dist/index.js`)
);
const { Type } = await import(url("node_modules/typebox/build/index.mjs"));

class MockAssistantStream extends EventStream<any, any> {
	constructor() {
		super(
			(event: any) => event.type === "done" || event.type === "error",
			(event: any) => (event.type === "done" ? event.message : event.error),
		);
	}
}

function assistant(content: any[], stopReason = "stop") {
	return {
		role: "assistant",
		content,
		api: "openai-responses",
		provider: "openai",
		model: "mock",
		usage: {
			input: 0,
			output: 0,
			cacheRead: 0,
			cacheWrite: 0,
			totalTokens: 0,
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
		},
		stopReason,
		timestamp: Date.now(),
	};
}

function reply(message: any) {
	const stream = new MockAssistantStream();
	queueMicrotask(() => stream.push({ type: "done", reason: message.stopReason, message }));
	return stream;
}

const toolCall = (id: string, name: string, args: any) => assistant([{ type: "toolCall", id, name, arguments: args }], "toolUse");
const user = (text: string, timestamp = Date.now()) => ({ role: "user", content: text, timestamp });

// agent.test.ts createTool
function createTool(name: string) {
	return {
		name,
		label: name,
		description: `${name} tool`,
		parameters: Type.Object({}),
		execute: async () => ({ content: [{ type: "text", text: name }], details: {} }),
	};
}
const echoTool = () => ({
	name: "echo",
	label: "Echo",
	description: "Echo input",
	parameters: Type.Object({}),
	execute: async () => ({ content: [{ type: "text", text: "echo" }], details: {} }),
});
const echoValueTool = () => ({
	name: "echo",
	label: "Echo",
	description: "Echo tool",
	parameters: Type.Object({ value: Type.String() }),
	async execute(_id: string, params: any) {
		return { content: [{ type: "text", text: `echoed: ${params.value}` }], details: { value: params.value } };
	},
});
const identityConverter = (messages: any[]) =>
	messages.filter((m) => m.role === "system" || m.role === "user" || m.role === "assistant" || m.role === "toolResult");
const unusedStreamFn = () => {
	throw new Error("Unexpected stream call");
};

// Serialized system messages of a transcript, in order.
const systemJSON = (messages: any[]) => messages.filter((m) => m.role === "system").map((m) => JSON.stringify(m));
const roles = (messages: any[]) => messages.map((m) => m.role);

async function drain(stream: any) {
	const events: string[] = [];
	for await (const event of stream) {
		events.push(event.type);
		if (event.type === "message_end" && event.message.role === "system") events.push(`system:${JSON.stringify(event.message)}`);
	}
	return { events, messages: await stream.result() };
}

const out: Record<string, unknown> = { sha, now: NOW };

// agent.test.ts "should create an agent instance with default state"
{
	const agent = new Agent({ streamFn: unusedStreamFn });
	out.defaultState = { messages: JSON.stringify(agent.state.messages), systemPrompt: agent.state.systemPrompt };
}

// agent.test.ts "should create an agent instance with custom initial state"
{
	const agent = new Agent({
		streamFn: unusedStreamFn,
		initialState: { systemPrompt: "You are a helpful assistant.", thinkingLevel: "low" },
	});
	out.customInitialState = { messages: JSON.stringify(agent.state.messages), systemPrompt: agent.state.systemPrompt };
}

// agent.test.ts "converts initial prompt and tools into transcript state"
{
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [echoTool()] },
		streamFn: unusedStreamFn,
	});
	out.initialPromptAndTools = { messages: JSON.stringify(agent.state.messages), systemPrompt: agent.state.systemPrompt };
}

// An initial transcript that already starts with a system message keeps it;
// the seed prompt and tools are not prepended.
{
	const agent = new Agent({
		initialState: {
			systemPrompt: "ignored",
			tools: [echoTool()],
			messages: [{ role: "system", content: "kept", timestamp: 5 }, user("old", 6)],
		},
		streamFn: unusedStreamFn,
	});
	out.initialTranscriptStartsWithSystem = {
		messages: JSON.stringify(agent.state.messages),
		systemPrompt: agent.state.systemPrompt,
	};
}

// Only a LEADING system message stops seeding: a transcript whose system
// message comes later still gets the seed prompt prepended.
{
	const agent = new Agent({
		initialState: {
			systemPrompt: "P",
			messages: [user("u", 1), { role: "system", content: "late", timestamp: 2 }],
		},
		streamFn: unusedStreamFn,
	});
	out.initialTranscriptWithLaterSystem = {
		messages: JSON.stringify(agent.state.messages),
		systemPrompt: agent.state.systemPrompt,
	};
}

// agent.test.ts "declares tool loadout changes to the model before the next request"
{
	const first = createTool("first");
	const second = createTool("second");
	const requests: string[][] = [];
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [first] },
		streamFn: (_model: any, context: any) => {
			requests.push(
				context.messages.flatMap((message: any) =>
					message.role === "system"
						? [
								`+${(message.toolsAdded ?? []).map((tool: any) => tool.name).join(",")}`,
								`-${(message.toolsRemoved ?? []).map((tool: any) => tool.name).join(",")}`,
							]
						: [],
				),
			);
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	});
	await agent.prompt("one");
	agent.state.tools = [second];
	await agent.prompt("two");
	await agent.prompt("three");
	out.declaresLoadoutChanges = {
		requests,
		roles: roles(agent.state.messages),
		system: systemJSON(agent.state.messages),
		systemPrompt: agent.state.systemPrompt,
	};
}

// agent.test.ts "merges tool changes into a pending system message"
{
	const systemCounts: number[] = [];
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful." },
		streamFn: (_model: any, context: any) => {
			systemCounts.push(context.messages.filter((message: any) => message.role === "system").length);
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	});
	agent.state.tools = [echoTool()];
	await agent.prompt([
		{ role: "system", content: "", sections: { skills: "<skills>x</skills>" }, timestamp: 1 },
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	out.mergesPendingSystemMessage = {
		systemCounts,
		roles: roles(agent.state.messages),
		system: systemJSON(agent.state.messages),
		systemPrompt: agent.state.systemPrompt,
	};
}

// agent.test.ts "rewrites pending tool declarations to match the executable set"
{
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [createTool("first")] },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	await agent.prompt([
		{
			role: "system",
			content: "",
			sections: { note: "<note>x</note>" },
			toolsAdded: [toToolDeclaration(createTool("second"))],
			toolsRemoved: [{ name: "first" }],
			timestamp: 1,
		},
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	out.rewritesPendingDeclarations = {
		system: systemJSON(agent.state.messages),
		currentTools: getCurrentSystemMessage(agent.state.messages)?.toolsAdded?.map((tool: any) => tool.name),
	};
}

// A pending message that already declares exactly the missing tools is still
// rebuilt: its declared tools are intent, the delta against the committed
// transcript replaces them (and the tool keys move after timestamp).
{
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful." },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	agent.state.tools = [echoTool()];
	await agent.prompt([
		{ role: "system", content: "", toolsAdded: [toToolDeclaration(echoTool())], timestamp: 1 },
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	out.pendingDeclaresTheDelta = { system: systemJSON(agent.state.messages) };
}

// A pending message whose tool fields change nothing is rebuilt without them:
// only a pending message declaring no tool fields at all is kept as is.
{
	const restate = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [echoTool()] },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	await restate.prompt([
		{ role: "system", content: "", toolsAdded: [toToolDeclaration(echoTool())], timestamp: 1 },
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	const removeUnknown = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [echoTool()] },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	await removeUnknown.prompt([
		{ role: "system", content: "", toolsRemoved: [{ name: "ghost" }], timestamp: 1 },
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	out.pendingNoOpToolFieldsAreDropped = {
		restated: systemJSON(restate.state.messages),
		removedUnknown: systemJSON(removeUnknown.state.messages),
	};
}

// Several pending system messages: only the last carries the delta; earlier
// ones keep their own declarations, which count toward the baseline.
{
	const agent = new Agent({
		initialState: { tools: [echoTool()] },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	await agent.prompt([
		{ role: "system", content: "a", toolsAdded: [toToolDeclaration(createTool("x"))], timestamp: 1 },
		{ role: "system", content: "b", timestamp: 2 },
		{ role: "user", content: "hi", timestamp: 3 },
	]);
	out.lastPendingSystemMessageCarriesTheDelta = {
		roles: roles(agent.state.messages),
		system: systemJSON(agent.state.messages),
	};
}

// A new declaration goes before the first non-system pending message.
{
	const agent = new Agent({
		initialState: { tools: [echoTool()], messages: [user("old", 1)] },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	agent.state.tools = [echoTool(), createTool("second")];
	await agent.prompt([user("one", 2), user("two", 3)]);
	out.declarationInsertedBeforeFirstNonSystem = {
		roles: roles(agent.state.messages),
		system: systemJSON(agent.state.messages),
	};
}

// agent.test.ts "restores the transcript baseline when reset", then the
// replayed baseline of a transcript that changed prompt and tools.
{
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful.", tools: [echoTool()], messages: [user("old", 1)] },
		streamFn: unusedStreamFn,
	});
	agent.reset();
	const replayed = new Agent({
		initialState: {
			messages: [
				{ role: "system", content: "base", sections: { a: "A" }, toolsAdded: [toToolDeclaration(echoTool())], timestamp: 3 },
				user("u", 4),
				{ role: "system", content: "more", sections: { a: null, b: "B" }, toolsRemoved: [{ name: "echo" }], timestamp: 5 },
			],
		},
		streamFn: unusedStreamFn,
	});
	replayed.reset();
	out.resetBaseline = {
		messages: JSON.stringify(agent.state.messages),
		replayed: JSON.stringify(replayed.state.messages),
		replayedSystemPrompt: replayed.state.systemPrompt,
	};
	const empty = new Agent({ initialState: { messages: [user("old", 1)] }, streamFn: unusedStreamFn });
	empty.reset();
	out.resetWithoutSystemMessage = { messages: JSON.stringify(empty.state.messages) };
}

// agent.ts continue(): a transcript of system messages only has nothing to
// continue from.
{
	const agent = new Agent({ initialState: { systemPrompt: "You are helpful." }, streamFn: unusedStreamFn });
	let error = "";
	try {
		await agent.continue();
	} catch (e: any) {
		error = e.message;
	}
	out.continueSystemOnly = { error };
}

// agent.ts continue(): a transcript that merely ENDS in a system message has a
// non-system message to continue from, so it makes a request.
{
	const requests: string[][] = [];
	const agent = new Agent({
		initialState: {
			messages: [
				{ role: "system", content: "P", timestamp: 0 },
				user("u", 1),
				{ role: "system", content: "late", timestamp: 2 },
			],
		},
		streamFn: (_model: any, context: any) => {
			requests.push(roles(context.messages));
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	});
	let error = "";
	try {
		await agent.continue();
	} catch (e: any) {
		error = e.message;
	}
	out.continueLastSystem = {
		error,
		requests,
		roles: roles(agent.state.messages),
		system: systemJSON(agent.state.messages),
		systemPrompt: agent.state.systemPrompt,
	};
}

// agent.test.ts "forwards finishTurn through AgentOptions with the active abort signal"
{
	const tool = {
		name: "noop",
		label: "Noop",
		description: "Noop tool",
		parameters: Type.Object({}),
		execute: async () => ({ content: [{ type: "text", text: "tool complete" }], details: {} }),
	};
	let requestCount = 0;
	let sawAbortSignal = false;
	let callbackContextRoles: string[] = [];
	const agent = new Agent({
		initialState: { tools: [tool] },
		finishTurn: (context: any, signal: any) => {
			sawAbortSignal = signal instanceof AbortSignal;
			callbackContextRoles = context.context.messages.map((message: any) => message.role);
			return { action: "end" };
		},
		streamFn: () => {
			requestCount++;
			return reply(requestCount === 1 ? toolCall("tool-1", "noop", {}) : assistant([{ type: "text", text: "should not run" }]));
		},
	});
	await agent.prompt("start");
	out.agentFinishTurnEnd = { requestCount, sawAbortSignal, callbackContextRoles };
}

// The prompt a patch message changes is what state.systemPrompt reports.
{
	const agent = new Agent({
		initialState: { systemPrompt: "You are helpful." },
		streamFn: () => reply(assistant([{ type: "text", text: "done" }])),
	});
	await agent.prompt([
		{ role: "system", content: "", sections: { skills: "<skills>x</skills>" }, timestamp: 1 },
		{ role: "user", content: "hi", timestamp: 2 },
	]);
	out.derivedSystemPrompt = { systemPrompt: agent.state.systemPrompt };
}

// agent-loop.test.ts "should build provider context exclusively from transcript messages"
{
	const initialSystem = { role: "system", content: "Transcript prompt", toolsAdded: [], timestamp: 1 };
	let keys: string[] = [];
	let sameObject = false;
	let systemCount = 0;
	const stream = agentLoop(
		[initialSystem, user("Hello")],
		{ messages: [], tools: [] },
		{ model: { id: "mock", api: "openai-responses", provider: "openai" }, convertToLlm: identityConverter },
		undefined,
		(_model: any, providerContext: any) => {
			keys = Object.keys(providerContext);
			sameObject = providerContext.messages[0] === initialSystem;
			systemCount = providerContext.messages.filter((m: any) => m.role === "system").length;
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopProviderContext = { keys, sameObject, systemCount, events, roles: roles(messages) };
}

// The same with executable tools the pending message already declares.
{
	const initialSystem = {
		role: "system",
		content: "Transcript prompt",
		toolsAdded: [toToolDeclaration(echoValueTool())],
		timestamp: 1,
	};
	let sameObject = false;
	let systemCount = 0;
	const stream = agentLoop(
		[initialSystem, user("Hello")],
		{ messages: [], tools: [echoValueTool()] },
		{ model: { id: "mock", api: "openai-responses", provider: "openai" }, convertToLlm: identityConverter },
		undefined,
		(_model: any, providerContext: any) => {
			sameObject = providerContext.messages[0] === initialSystem;
			systemCount = providerContext.messages.filter((m: any) => m.role === "system").length;
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopProviderContextDeclaredTools = { sameObject, systemCount, events, roles: roles(messages) };
}

// agent-loop.test.ts "should use prepareNextTurn snapshot before continuing"
{
	let prepareCalls = 0;
	let prepared = false;
	let convertedSecondTurnHasUpdate = false;
	let llmCalls = 0;
	const stream = agentLoop(
		[user("echo something")],
		{ messages: [], tools: [echoValueTool()] },
		{
			model: { id: "mock", api: "openai-responses", provider: "openai" },
			convertToLlm: identityConverter,
			prepareNextTurn: async ({ context: currentContext }: any) => {
				prepareCalls++;
				if (prepared) return undefined;
				prepared = true;
				return {
					context: { messages: currentContext.messages.slice(), tools: currentContext.tools },
					messages: [{ role: "system", content: "updated guidance", timestamp: 1 }],
				};
			},
		},
		undefined,
		(_model: any, ctx: any) => {
			llmCalls++;
			if (llmCalls === 2) {
				convertedSecondTurnHasUpdate = ctx.messages.some(
					(message: any) => message.role === "system" && message.content === "updated guidance",
				);
			}
			return reply(llmCalls === 1 ? toolCall("tool-1", "echo", { value: "hello" }) : assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopPrepareNextTurn = { llmCalls, prepareCalls, convertedSecondTurnHasUpdate, events, roles: roles(messages) };
}

// Prepared messages go ahead of queued steering, and a tool change in the
// prepared context lands on the prepared system message.
{
	let prepared = false;
	let steered = false;
	let llmCalls = 0;
	const secondRequestRoles: string[] = [];
	const stream = agentLoop(
		[user("echo something")],
		{ messages: [], tools: [echoValueTool()] },
		{
			model: { id: "mock", api: "openai-responses", provider: "openai" },
			convertToLlm: identityConverter,
			getSteeringMessages: async () => {
				if (llmCalls === 1 && !steered) {
					steered = true;
					return [user("steer", 7)];
				}
				return [];
			},
			prepareNextTurn: async ({ context: currentContext }: any) => {
				if (prepared) return undefined;
				prepared = true;
				return {
					context: { messages: currentContext.messages.slice(), tools: [...currentContext.tools, createTool("second")] },
					messages: [
						{ role: "system", content: "updated guidance", timestamp: 1 },
						user("prepared", 2),
					],
				};
			},
		},
		undefined,
		(_model: any, ctx: any) => {
			llmCalls++;
			if (llmCalls === 2) secondRequestRoles.push(...roles(ctx.messages));
			return reply(llmCalls === 1 ? toolCall("tool-1", "echo", { value: "hello" }) : assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopPreparedBeforeSteering = {
		llmCalls,
		secondRequestRoles,
		events,
		roles: roles(messages),
		contents: messages.map((m: any) => (typeof m.content === "string" ? m.content : "")),
	};
}

// A tool change in a prepared context with no prepared messages inserts a
// declaration of its own before the request.
{
	let prepared = false;
	let llmCalls = 0;
	const stream = agentLoop(
		[user("echo something")],
		{ messages: [], tools: [echoValueTool()] },
		{
			model: { id: "mock", api: "openai-responses", provider: "openai" },
			convertToLlm: identityConverter,
			prepareNextTurn: async ({ context: currentContext }: any) => {
				if (prepared) return undefined;
				prepared = true;
				return { context: { messages: currentContext.messages.slice(), tools: [] } };
			},
		},
		undefined,
		() => {
			llmCalls++;
			return reply(llmCalls === 1 ? toolCall("tool-1", "echo", { value: "hello" }) : assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopPreparedToolRemoval = { llmCalls, events, roles: roles(messages) };
}

// A snapshot with prepared messages but no context: the messages are appended
// all the same, declared against the unchanged context (so the prepared system
// message's declaration of a tool the runtime cannot execute is dropped).
{
	let prepared = false;
	let llmCalls = 0;
	const secondRequestRoles: string[] = [];
	const stream = agentLoop(
		[user("echo something")],
		{ messages: [], tools: [echoValueTool()] },
		{
			model: { id: "mock", api: "openai-responses", provider: "openai" },
			convertToLlm: identityConverter,
			prepareNextTurn: async () => {
				if (prepared) return undefined;
				prepared = true;
				return {
					messages: [
						{ role: "system", content: "guide", toolsAdded: [toToolDeclaration(createTool("ghost"))], timestamp: 5 },
					],
				};
			},
		},
		undefined,
		(_model: any, ctx: any) => {
			llmCalls++;
			if (llmCalls === 2) secondRequestRoles.push(...roles(ctx.messages));
			return reply(llmCalls === 1 ? toolCall("tool-1", "echo", { value: "hello" }) : assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopPreparedMessagesWithoutContext = { llmCalls, secondRequestRoles, events, roles: roles(messages) };
}

// A new declaration goes before the first non-SYSTEM pending message, even a
// custom one the provider never sees — not before the first user message.
{
	const requests: string[][] = [];
	const stream = agentLoop(
		[{ role: "notification", text: "n", timestamp: 1 }, user("u", 2)],
		{ messages: [], tools: [createTool("echo")] },
		{ model: { id: "mock", api: "openai-responses", provider: "openai" }, convertToLlm: identityConverter },
		undefined,
		(_model: any, ctx: any) => {
			requests.push(roles(ctx.messages));
			return reply(assistant([{ type: "text", text: "done" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopDeclarationBeforeCustomMessage = { requests, events, roles: roles(messages) };
}

// agent-loop.test.ts "action:end receives finalized turn context and stops before queue polling"
{
	const executed: string[] = [];
	const tool = {
		...echoValueTool(),
		async execute(_id: string, params: any) {
			executed.push(params.value);
			return { content: [{ type: "text", text: `echoed: ${params.value}` }], details: { value: params.value } };
		},
	};
	let steeringPolls = 0;
	let followUpPolls = 0;
	let callbackToolResultIds: string[] = [];
	let callbackContextRoles: string[] = [];
	let llmCalls = 0;
	const stream = agentLoop(
		[user("echo something")],
		{ messages: [], tools: [tool] },
		{
			model: { id: "mock", api: "openai-responses", provider: "openai" },
			convertToLlm: identityConverter,
			getSteeringMessages: async () => {
				steeringPolls++;
				return [];
			},
			getFollowUpMessages: async () => {
				followUpPolls++;
				return [user("follow up should stay queued")];
			},
			finishTurn: async ({ toolResults, context }: any) => {
				callbackToolResultIds = toolResults.map((toolResult: any) => toolResult.toolCallId);
				callbackContextRoles = context.messages.map((contextMessage: any) => contextMessage.role);
				return { action: "end" };
			},
		},
		undefined,
		() => {
			llmCalls++;
			return reply(llmCalls === 1 ? toolCall("tool-1", "echo", { value: "hello" }) : assistant([{ type: "text", text: "should not run" }]));
		},
	);
	const { events, messages } = await drain(stream);
	out.loopFinishTurnEnd = {
		llmCalls,
		executed,
		steeringPolls,
		followUpPolls,
		callbackToolResultIds,
		callbackContextRoles,
		roles: roles(messages),
		events,
	};
}

// agent-loop.test.ts "should continue after parallel tool calls when not all tool results terminate"
{
	const tool = {
		...echoValueTool(),
		async execute(_id: string, params: any) {
			return {
				content: [{ type: "text", text: `echoed: ${params.value}` }],
				details: { value: params.value },
				terminate: params.value === "first",
			};
		},
	};
	let callIndex = 0;
	const stream = agentLoop(
		[user("echo both")],
		{ messages: [], tools: [tool] },
		{ model: { id: "mock", api: "openai-responses", provider: "openai" }, convertToLlm: identityConverter, toolExecution: "parallel" },
		undefined,
		() => {
			const message =
				callIndex === 0
					? assistant(
							[
								{ type: "toolCall", id: "tool-1", name: "echo", arguments: { value: "first" } },
								{ type: "toolCall", id: "tool-2", name: "echo", arguments: { value: "second" } },
							],
							"toolUse",
						)
					: assistant([{ type: "text", text: "done" }]);
			callIndex++;
			return reply(message);
		},
	);
	const { messages } = await drain(stream);
	out.loopParallelNotAllTerminate = { callIndex, roles: roles(messages) };
}

// agentLoopContinue declares the executable tools before its first request.
{
	const stream = agentLoopContinue(
		{ messages: [user("Hello", 1)], tools: [echoValueTool()] },
		{ model: { id: "mock", api: "openai-responses", provider: "openai" }, convertToLlm: identityConverter },
		undefined,
		() => reply(assistant([{ type: "text", text: "done" }])),
	);
	const { events, messages } = await drain(stream);
	out.loopContinueDeclaresTools = { events, roles: roles(messages) };
}

fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
