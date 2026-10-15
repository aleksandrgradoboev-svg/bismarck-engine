// Captures whether real pi compacts on the first prompt after resuming a
// session file — the oracle behind TestResumedCompactionTriggerMatchesPi in
// coding/compaction_resume_test.go.
//
//   node capture-trigger.mts <npm-root> <out.json> <version>
//   e.g. node capture-trigger.mts ~/.cache/pi-npm/0.87.1 trigger-0.87.1.json 0.87.1
//
// <npm-root> holds node_modules/@earendil-works/pi-coding-agent at <version>.
// The faux provider is registered on the pi-ai copy pi-coding-agent resolves
// (its nested node_modules), since each copy keeps its own api registry.
//
// Each scenario writes a session file with pi's SessionManager, opens it with
// SessionManager.open, and prompts once through a real AgentSession
// (createAgentSession) over a faux model whose window is contextWindow. The
// faux provider records each request in order: a summarization request (its
// system prompt is SUMMARIZATION_SYSTEM_PROMPT) as its user text, the prompt's
// own request as its non-system messages, one line per message. pi decides in
// _checkCompaction, before the prompt is sent: an assistant older than the
// latest compaction is skipped, and the threshold estimate trusts usage only
// when it was recorded after the latest compaction or context_edit
// (estimateProjectedContextTokens). Each scenario also records whether
// prepareCompaction, on the file as written, finds anything to compact: it
// finds nothing when the last entry is the compaction itself.
//
// Each message is stamped as it is appended: an hour before the run until the
// compaction or edit, an hour after it from then on, so pi's timestamp checks
// read them as they would in a real session.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [root, outFile, version] = process.argv.slice(2);
if (!root || !outFile || !version) {
	console.error("usage: node capture-trigger.mts <npm-root> <out.json> <version>");
	process.exit(2);
}
const agentRoot = path.join(path.resolve(root), "node_modules/@earendil-works/pi-coding-agent");
const load = (file: string) => import(pathToFileURL(path.join(agentRoot, file)).href);
const { registerFauxProvider, fauxAssistantMessage } = await load("node_modules/@earendil-works/pi-ai/dist/compat.js");
const { SessionManager, SettingsManager, ModelRuntime, createAgentSession, convertToLlm } = await load("dist/index.js");
const { AuthStorage } = await load("dist/core/auth-storage.js");
const { SUMMARIZATION_SYSTEM_PROMPT } = await load("dist/core/compaction/utils.js");
const { prepareCompaction } = await load("dist/core/compaction/index.js");
const { getSystemMessageText } = await load("node_modules/@earendil-works/pi-ai/dist/utils/text.js");

const contextWindow = 2000;
const settings = { enabled: true, reserveTokens: 200, keepRecentTokens: 150 };

// A fixed api id, so the recorded messages do not carry a random one.
const faux = registerFauxProvider({ api: "faux", models: [{ id: "faux-1", contextWindow, maxTokens: 8192 }] });
const model = faux.getModel();
const auth = AuthStorage.inMemory();
await auth.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
const runtime = await ModelRuntime.create({ credentials: auth, modelsPath: null, allowModelNetwork: false });
runtime.registerProvider(model.provider, {
	baseUrl: model.baseUrl,
	apiKey: "faux-key",
	api: faux.api,
	models: faux.models.map((m: any) => ({
		id: m.id,
		name: m.name,
		api: m.api,
		reasoning: m.reasoning,
		input: m.input,
		cost: m.cost,
		contextWindow: m.contextWindow,
		maxTokens: m.maxTokens,
		baseUrl: m.baseUrl,
	})),
});

const hour = 3_600_000;
const before = Date.now() - hour;
const after = Date.now() + hour;
let clock = 0;
const usage = (totalTokens: number) => ({
	input: totalTokens,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
});
const user = (text: string) => ({ role: "user", content: [{ type: "text", text }], timestamp: 0 });
const assistant = (text: string, totalTokens: number, stopReason = "stop") => ({
	role: "assistant",
	content: [{ type: "text", text }],
	api: faux.api,
	provider: model.provider,
	model: model.id,
	usage: usage(totalTokens),
	stopReason,
	...(stopReason === "error" ? { errorMessage: "upstream unavailable" } : {}),
	timestamp: 0,
});
const system = (preamble: string) => ({ role: "system", content: "", sections: { preamble }, timestamp: 0 });
// A message long enough (225 estimated tokens) to cross keepRecentTokens on its
// own, so the cut lands on it whichever side of the prompt the check runs.
const long = (text: string) => `${text} ${"lorem ".repeat(150)}`.trimEnd();

type Step = { message: any } | { compactKeepingFrom: number; summary: string } | { editContentOf: number; text: string };
type Scenario = { name: string; steps: Step[]; prompt: string };
// Messages are numbered by their step index; a compaction keeps from the
// numbered message, an edit replaces the numbered message's content.
const scenarios: Scenario[] = [
	{
		// pi compacts at agent_end, right after the assistant whose usage crossed
		// the threshold; that assistant stays in the kept range. Its usage
		// predates the compaction, so the next prompt does not compact again.
		name: "stale-usage-in-kept-range",
		steps: [
			{ message: user("q1") },
			{ message: assistant("a1", 900) },
			{ message: user("q2") },
			{ message: assistant(long("a2"), 1900) },
			{ compactKeepingFrom: 2, summary: "## Goal\nold work" },
		],
		prompt: "q3",
	},
	{
		// The only usage after the compaction is an error's, which pi does not
		// trust; the stale usage before it is skipped as well.
		name: "stale-usage-then-error",
		steps: [
			{ message: user("q1") },
			{ message: assistant("a1", 900) },
			{ message: user("q2") },
			{ message: assistant(long("a2"), 1900) },
			{ compactKeepingFrom: 2, summary: "## Goal\nold work" },
			{ message: user("q3") },
			{ message: assistant("", 0, "error") },
		],
		prompt: "q4",
	},
	{
		// Usage recorded after the compaction is trusted: over the threshold,
		// the prompt compacts first. The cut splits a3's turn.
		name: "fresh-usage-over-threshold",
		steps: [
			{ message: user("q1") },
			{ message: assistant("a1", 900) },
			{ message: user("q2") },
			{ message: assistant(long("a2"), 1900) },
			{ compactKeepingFrom: 2, summary: "## Goal\nold work" },
			{ message: user("q3") },
			{ message: assistant(long("a3"), 1900) },
		],
		prompt: "q4",
	},
	{
		name: "fresh-usage-under-threshold",
		steps: [
			{ message: user("q1") },
			{ message: assistant("a1", 900) },
			{ message: user("q2") },
			{ message: assistant(long("a2"), 1900) },
			{ compactKeepingFrom: 2, summary: "## Goal\nold work" },
			{ message: user("q3") },
			{ message: assistant(long("a3"), 100) },
		],
		prompt: "q4",
	},
	{
		// Without trusted usage, the estimate counts the current system message
		// once: seven patches of one section weigh as the last of them alone.
		name: "stale-usage-then-system-patches",
		steps: [
			{ message: user("q1") },
			{ message: assistant("a1", 900) },
			{ message: user("q2") },
			{ message: assistant(long("a2"), 1900) },
			{ compactKeepingFrom: 2, summary: "## Goal\nold work" },
			...[1, 2, 3, 4, 5, 6, 7].map((n) => ({ message: system(`${n} ${"rule ".repeat(240)}`) })),
		],
		prompt: "q3",
	},
	{
		// A context edit after the usage invalidates it too.
		name: "usage-before-context-edit",
		steps: [
			{ message: user("q1") },
			{ message: assistant(long("a1"), 1900) },
			{ editContentOf: 0, text: "q1, shortened" },
		],
		prompt: "q2",
	},
	{
		// Without a later compaction or edit, the usage is trusted.
		name: "usage-without-compaction",
		steps: [{ message: user("q1") }, { message: assistant(long("a1"), 1900) }],
		prompt: "q2",
	},
];

const textOf = (content: unknown): string =>
	typeof content === "string"
		? content
		: (content as Array<{ type: string; text?: string }>)
				.filter((block) => block.type === "text")
				.map((block) => block.text)
				.join("");

const out = { version, contextWindow, settings, scenarios: [] as unknown[] };
for (const scenario of scenarios) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "capture-trigger-"));
	// The header records a fixed cwd, not the temporary directory.
	const writer = SessionManager.create("/tmp/capture-trigger", dir);
	const ids: string[] = [];
	clock = before;
	for (const step of scenario.steps) {
		if ("message" in step) {
			ids.push(writer.appendMessage({ ...step.message, timestamp: ++clock }));
		} else if ("compactKeepingFrom" in step) {
			writer.appendCompaction(step.summary, ids[step.compactKeepingFrom], 1900, { readFiles: [], modifiedFiles: [] }, false);
			clock = after;
		} else {
			writer.appendContextEdit(ids[step.editContentOf], { content: step.text });
			clock = after;
		}
	}
	// prepareCompaction on the file as written finds nothing to compact when
	// its last entry is the compaction itself.
	const prepared = prepareCompaction(writer.getBranch(), settings) !== undefined;
	const file = writer.getSessionFile();
	const entries = fs
		.readFileSync(file, "utf8")
		.split("\n")
		.filter((line: string) => line !== "")
		.map((line: string) => JSON.parse(line));

	// Requests up to the prompt's own: a compaction pi runs at agent_end, after
	// the reply, is the next prompt's check.
	const requests: Array<{ summarization?: string; prompt?: string[] }> = [];
	let prompted = false;
	let summaries = 0;
	const step = (context: any) => {
		const system = context.messages.find((message: any) => message.role === "system");
		if (system && getSystemMessageText(system) === SUMMARIZATION_SYSTEM_PROMPT) {
			const users = context.messages.filter((message: any) => message.role === "user");
			if (!prompted) requests.push({ summarization: textOf(users[0].content) });
			return fauxAssistantMessage(`SUMMARY ${++summaries}`);
		}
		prompted = true;
		requests.push({
			prompt: convertToLlm(context.messages)
				.filter((message: any) => message.role !== "system")
				.map((message: any) => `${message.role}:${textOf(message.content)}`),
		});
		return fauxAssistantMessage("reply");
	};
	faux.setResponses([step, step, step, step]);
	const { session } = await createAgentSession({
		cwd: dir,
		agentDir: dir,
		modelRuntime: runtime,
		model,
		sessionManager: SessionManager.open(file, dir),
		settingsManager: SettingsManager.inMemory({ compaction: settings }),
		noTools: "all",
	});
	await session.prompt(scenario.prompt);
	session.dispose?.();
	out.scenarios.push({ name: scenario.name, entries, prepared, prompt: scenario.prompt, requests });
}
faux.unregister();
fs.writeFileSync(outFile, `${JSON.stringify(out, null, "\t")}\n`);
