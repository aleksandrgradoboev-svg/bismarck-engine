// Captures what pi's adapters make of the base URL their request URL is built
// from — the oracle behind TestInvalidBaseURLMatchesPi and
// TestBaseURLRequestsMatchPi in this package.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> base-url-2b0a123de.json 2b0a123de
//
// <extraction> holds `git archive <sha> packages/ai package-lock.json` from the
// upstream clone, plus a node_modules resolving pi-ai's dependencies (the npm
// build's). The script refuses to write unless each SDK it runs
// (@anthropic-ai/sdk, openai, @google/genai) resolves to the version AND
// integrity the sha's package-lock.json locks at the path it resolved to:
// packages/ai/node_modules/<name> for a copy the lockfile nests for pi-ai,
// node_modules/<name> otherwise. Since ab30693d6 it nests openai 7.19.0 there
// (the root keeps 6.40.0): unpack `npm pack openai@7.19.0` (check its sha512
// against the lockfile) to packages/ai/node_modules/openai and record that
// integrity in packages/ai/node_modules/.package-lock.json under
// "node_modules/openai", as npm records an installed package.
//
// Each adapter streams with each base URL and an onPayload that counts its
// calls. pi's `new URL(...)` — the SDKs' request URL, pi-messages' own, built
// before onPayload — is the WHATWG URL parser, and fetch sends what it makes
// of the URL.
//
// `rows`: base URLs no request is meant to reach (the URLs that parse name
// port 1, which refuses). The parser throws TypeError "Invalid URL" for what
// it refuses. A row whose URL the parser takes but Go's net/url does not, or
// the reverse, is tagged `divergence` (the two parsers differ there, and the
// port's is net/url); each row records the stream's event types, its
// stopReason and errorMessage, and how often onPayload ran.
//
// `hrefs`: what node's `new URL(input)` makes of each input — its href, or
// "Invalid URL" — the parser itself, with nothing sent. A row the port reads
// differently is tagged `divergence`.
//
// `customFetch`: a base URL with credentials streamed through each adapter
// that takes a custom fetch (all but google), which records the URL it is
// handed and answers 400: no refusal, the credentials handed on.
//
// `requests`: base URLs naming a recording server by `{PORT}`, which answers
// every request 400. Each records the request the server received — its
// request line, and its Host and Authorization headers — or, when none came,
// the errorMessage (`{PORT}` standing for the port in both), and how often
// onPayload ran. The parser strips leading and trailing C0 controls and
// spaces and removes every tab and newline before it parses; for a special
// scheme it reads any run of slashes and backslashes after "scheme:" as the
// authority's start and a backslash as a path separator; it percent-encodes
// the other controls, lowercases the host and normalizes the port. A row the
// port gets wrong names the adapters it differs for in `divergentApis`, and
// how in `divergence`.
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}

const src = path.join(extraction, "packages/ai/src");
const require = createRequire(path.join(src, "api/google-generative-ai.ts"));
const lock = JSON.parse(fs.readFileSync(path.join(extraction, "package-lock.json"), "utf8"));
const sdks: string[] = [];
for (const name of ["@anthropic-ai/sdk", "openai", "@google/genai"]) {
	let dir = path.dirname(require.resolve(name));
	while (!fs.existsSync(path.join(dir, "package.json")) || JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name !== name) {
		dir = path.dirname(dir);
	}
	const version = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).version as string;
	let root = dir;
	while (path.basename(root) !== "node_modules") root = path.dirname(root);
	const installed = JSON.parse(fs.readFileSync(path.join(root, ".package-lock.json"), "utf8")).packages[`node_modules/${name}`];
	// The lockfile entry for the path it resolved to: pi-ai's own nested copy
	// (packages/ai/node_modules/<name>, as openai is since ab30693d6) or the
	// root one (node_modules/<name>). Where the lockfile nests a copy for
	// pi-ai, pi-ai must resolve that one.
	const resolvedKey = path.relative(extraction, dir);
	const key = resolvedKey.startsWith("..") || path.isAbsolute(resolvedKey) ? `node_modules/${name}` : resolvedKey;
	const nestedKey = `packages/ai/node_modules/${name}`;
	const locked = lock.packages[key];
	if ((lock.packages[nestedKey] && key !== nestedKey) || locked?.version !== version || locked.integrity !== installed?.integrity) {
		const want = lock.packages[nestedKey] ? nestedKey : key;
		console.error(`${name} mismatch: ${sha} locks ${lock.packages[want]?.version} ${lock.packages[want]?.integrity} at ${want}, resolved ${version} ${installed?.integrity} at ${key}`);
		process.exit(1);
	}
	sdks.push(`${name} ${version} ${locked.integrity}`);
}

const load = async (file: string) => import(pathToFileURL(path.join(src, file)).href);
const adapters = [
	{ api: "google-generative-ai", provider: "google", id: "gemini-2.5-flash", module: await load("api/google-generative-ai.ts") },
	{ api: "anthropic-messages", provider: "anthropic", id: "claude-sonnet-4-5", module: await load("api/anthropic-messages.ts") },
	{ api: "pi-messages", provider: "radius", id: "auto", module: await load("api/pi-messages.ts") },
	{ api: "openai-completions", provider: "openai", id: "gpt-4o", module: await load("api/openai-completions.ts") },
	{ api: "openai-responses", provider: "openai", id: "gpt-5-mini", module: await load("api/openai-responses.ts") },
];

// stream runs one adapter with one base URL and returns the final message and
// how often onPayload ran.
async function stream(a: (typeof adapters)[number], baseUrl: string) {
	const model = {
		id: a.id,
		name: a.id,
		api: a.api,
		provider: a.provider,
		baseUrl,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 100000,
		maxTokens: 1000,
	};
	let onPayload = 0;
	const out = a.module.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, {
		apiKey: "test-api-key",
		onPayload: () => {
			onPayload++;
			return undefined;
		},
	});
	const events: string[] = [];
	for await (const ev of out) events.push(ev.type);
	return { events, msg: await out.result(), onPayload };
}

// [base URL, divergence]: a divergence names how the two parsers differ.
const bases: Array<[string, string?]> = [
	["not a url"],
	["127.0.0.1:1"],
	["api.example.com/v1"],
	["//127.0.0.1:1"],
	["ht!tp://127.0.0.1:1"],
	["http://[::1"],
	["http://exa mple.com"],
	["http://127.0.0.1:99999"],
	["http://127.0.0.1:1x"],
	["http://127.0.0.1:1/%zz", "a malformed percent escape: the WHATWG parser keeps it, net/url refuses it"],
	["http://127.0.0.1:1/a%", "a lone percent sign: the WHATWG parser keeps it, net/url refuses it"],
	["http://user:pa ss@127.0.0.1:1", "a space in the userinfo: the WHATWG parser percent-encodes it (fetch then refuses the credentials), net/url refuses it"],
	["http://256.0.0.1:1", "an IPv4 part past 255: the WHATWG parser refuses it, net/url takes it as a host name"],
];

const rows = [];
for (const a of adapters) {
	for (const [baseUrl, divergence] of bases) {
		const { events, msg, onPayload } = await stream(a, baseUrl);
		rows.push({
			api: a.api,
			baseUrl,
			...(divergence ? { divergence } : {}),
			events,
			stopReason: msg.stopReason,
			errorMessage: msg.errorMessage,
			onPayload,
		});
	}
}

// [input, divergence]
const hrefInputs: Array<[string, string?]> = [
	["http://example.com/v1/messages"],
	["http://example.com:80/v1"],
	["https://example.com:443/v1"],
	["https://example.com:0443/v1"],
	["http://example.com:08080/v1"],
	["http://example.com:/v1"],
	["http://EXAMPLE.com/v1"],
	["HTTP://example.com/v1"],
	["http://example.com"],
	["http://[::1]:1/v1"],
	["http://[::1]:80/v1"],
	[" http://example.com/v1 "],
	[`http://example.com/v1${String.fromCharCode(13)}/messages`],
	[`http://exa${String.fromCharCode(9)}mple.com/v1`],
	[`${String.fromCharCode(0)}http://example.com/v1${String.fromCharCode(31)}`],
	["http:example.com/v1"],
	["http:\\\\example.com\\v1"],
	["http://example.com/a\\b?x=\\y"],
	[`http://example.com/v1${String.fromCharCode(0)}/x`],
	[`http://example.com/v1${String.fromCharCode(0x7f)}/x`],
	[`http://example.com/v1?q=${String.fromCharCode(1)}x`],
	["http://example.com/v 1"],
	["http://example.com/a^b"],
	["not a url"],
	["http://example.com:65536/v1"],
	["http://"],
	["http://user:pass@example.com/v1"],
	["http://user@example.com/v1"],
	["http://:pass@example.com/v1"],
	["http://@example.com/v1"],
	["http://:@example.com/v1"],
	["http://user:p%40ss@example.com/v1"],
	["http://user:pa:ss@example.com/v1"],
	["http://example.com/a/../v1", "the WHATWG parser resolves dot segments; net/url keeps them"],
	["http://127.1/v1", "the WHATWG parser reads 127.1 as 127.0.0.1; net/url keeps the host name"],
	["http://example.com/a|b", "the WHATWG path percent-encode set leaves | alone; net/url escapes it"],
	["http://256.0.0.1/v1", "the WHATWG parser refuses an IPv4 part past 255; net/url takes it as a host name"],
	["http://example.com/%zz", "the WHATWG parser keeps a malformed percent escape; net/url refuses it"],
	["http://ex%61mple.com/v1", "the WHATWG parser percent-decodes a host; net/url refuses an escape below 0x80 there"],
];
const hrefs = hrefInputs.map(([input, divergence]) => {
	let href: string;
	try {
		href = new URL(input).href;
	} catch (e) {
		href = (e as Error).message;
	}
	return { input, ...(divergence ? { divergence } : {}), href };
});

const customFetch = [];
for (const a of adapters) {
	if (a.api === "google-generative-ai") continue; // it refuses a custom fetch outright
	const baseUrl = "http://user:pass@example.invalid/v1";
	let handed = "";
	const model = {
		id: a.id,
		name: a.id,
		api: a.api,
		provider: a.provider,
		baseUrl,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 100000,
		maxTokens: 1000,
	};
	const out = a.module.stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, {
		apiKey: "test-api-key",
		fetch: async (url: unknown) => {
			handed = String(url);
			return new Response('{"error":{"message":"stop here"}}', { status: 400, headers: { "content-type": "application/json" } });
		},
	});
	for await (const _ of out) {
	}
	await out.result();
	customFetch.push({ api: a.api, baseUrl, url: handed });
}

// The recording server: it answers every request 400 and keeps the last
// request's line and its Host and Authorization headers.
let received: { line: string; host: string; authorization: string } | undefined;
const server = net.createServer((sock) => {
	let buf = Buffer.alloc(0);
	sock.on("data", (d) => {
		buf = Buffer.concat([buf, d]);
		const headEnd = buf.indexOf("\r\n\r\n");
		if (headEnd < 0) return;
		const head = buf.subarray(0, headEnd).toString("latin1");
		const len = Number(/content-length:\s*(\d+)/i.exec(head)?.[1] ?? 0);
		if (buf.length < headEnd + 4 + len) return;
		buf = Buffer.alloc(0);
		received = {
			line: head.split("\r\n")[0],
			host: /^host:[ \t]*(.*?)[ \t]*$/im.exec(head)?.[1] ?? "",
			authorization: /^authorization:[ \t]*(.*?)[ \t]*$/im.exec(head)?.[1] ?? "",
		};
		const body = '{"error":{"message":"stop here"}}';
		sock.end(`HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: ${body.length}\r\n\r\n${body}`);
	});
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = String((server.address() as net.AddressInfo).port);

const C = (...codes: number[]) => String.fromCharCode(...codes);
const all = adapters.map((a) => a.api);
type RequestBase = { label: string; baseUrl: string; divergence?: string; divergentApis?: string[] };
const requestBases: RequestBase[] = [
	{ label: "a plain base URL", baseUrl: "http://127.0.0.1:{PORT}/v1" },
	{ label: "a trailing slash", baseUrl: "http://127.0.0.1:{PORT}/v1/" },
	{ label: "two trailing slashes", baseUrl: "http://127.0.0.1:{PORT}/v1//" },
	{ label: "a slash alone after the host", baseUrl: "http://127.0.0.1:{PORT}/" },
	{ label: "a trailing CR", baseUrl: `http://127.0.0.1:{PORT}/v1${C(13)}` },
	{ label: "a trailing LF", baseUrl: `http://127.0.0.1:{PORT}/v1${C(10)}` },
	{ label: "a trailing CRLF", baseUrl: `http://127.0.0.1:{PORT}/v1${C(13, 10)}` },
	{ label: "a leading space", baseUrl: " http://127.0.0.1:{PORT}/v1" },
	{ label: "a tab in the path", baseUrl: `http://127.0.0.1:{PORT}/v${C(9)}1` },
	{ label: "a CR a placeholder brought into the path", baseUrl: `http://127.0.0.1:{PORT}/v1/account${C(13)}/gateway` },
	{ label: "a tab in the scheme", baseUrl: `ht${C(9)}tp://127.0.0.1:{PORT}/v1` },
	{ label: "an LF in the host", baseUrl: `http://127.0.0${C(10)}.1:{PORT}/v1` },
	{ label: "C0 controls leading and trailing", baseUrl: `${C(1)}http://127.0.0.1:{PORT}/v1${C(31)}` },
	{ label: "a trailing NUL", baseUrl: `http://127.0.0.1:{PORT}/v1${C(0)}` },
	{ label: "a vertical tab in the path", baseUrl: `http://127.0.0.1:{PORT}/v${C(11)}1` },
	{ label: "a DEL in the path", baseUrl: `http://127.0.0.1:{PORT}/v1${C(0x7f)}` },
	{ label: "a trailing space", baseUrl: "http://127.0.0.1:{PORT}/v1 " },
	{ label: "a space in the path", baseUrl: "http://127.0.0.1:{PORT}/v 1" },
	{ label: "a backslash after the host", baseUrl: "http://127.0.0.1:{PORT}\\v1" },
	{ label: "backslashes after the scheme", baseUrl: "http:\\\\127.0.0.1:{PORT}\\v1" },
	{ label: "a backslash in the path", baseUrl: "http://127.0.0.1:{PORT}/a\\b" },
	{ label: "no slashes after the scheme", baseUrl: "http:127.0.0.1:{PORT}/v1" },
	{ label: "one slash after the scheme", baseUrl: "http:/127.0.0.1:{PORT}/v1" },
	{ label: "three slashes after the scheme", baseUrl: "http:///127.0.0.1:{PORT}/v1" },
	{ label: "slashes and backslashes after the scheme", baseUrl: "http:/\\/127.0.0.1:{PORT}/v1" },
	{ label: "an uppercase scheme", baseUrl: "HTTP://127.0.0.1:{PORT}/v1" },
	{ label: "an uppercase host", baseUrl: "http://LOCALHOST:{PORT}/v1" },
	{ label: "a port with a leading zero", baseUrl: "http://127.0.0.1:0{PORT}/v1" },
	{ label: "credentials", baseUrl: "http://user:pass@127.0.0.1:{PORT}/v1" },
	{ label: "a username alone", baseUrl: "http://user@127.0.0.1:{PORT}/v1" },
	{ label: "a password alone", baseUrl: "http://:pass@127.0.0.1:{PORT}/v1" },
	{ label: "an empty userinfo", baseUrl: "http://@127.0.0.1:{PORT}/v1" },
	{ label: "an empty username and password", baseUrl: "http://:@127.0.0.1:{PORT}/v1" },
	{ label: "credentials with a CR", baseUrl: `http://us${C(13)}er:pass@127.0.0.1:{PORT}/v1` },
	{ label: "credentials and an uppercase host", baseUrl: "http://user:pass@LOCALHOST:{PORT}/v1" },
	{ label: "credentials and a default port", baseUrl: "http://user:pass@127.0.0.1:80/v1" },
	{ label: "credentials with a percent-encoded password", baseUrl: "http://user:p%40ss@127.0.0.1:{PORT}/v1" },
	{ label: "credentials with a colon in the password", baseUrl: "http://user:pa:ss@127.0.0.1:{PORT}/v1" },
	{ label: "credentials before a backslash", baseUrl: "http://user:pass@127.0.0.1:{PORT}\\v1" },
	// The SDKs take a rejected fetch whose text says it timed out for a
	// timeout, and fetch's credentials refusal quotes the URL.
	{ label: "credentials at a /timeout path", baseUrl: "http://user:pass@127.0.0.1:{PORT}/timeout" },
	{
		label: "dot segments",
		baseUrl: "http://127.0.0.1:{PORT}/a/../v1",
		divergence: "the WHATWG parser resolves dot segments; net/url keeps them in the path",
		divergentApis: all,
	},
	{
		label: "a percent-encoded dot segment",
		baseUrl: "http://127.0.0.1:{PORT}/a/%2e%2e/v1",
		divergence: "the WHATWG parser resolves %2e%2e as ..; net/url keeps it in the path",
		divergentApis: all,
	},
	{
		label: "an IPv4 shorthand",
		baseUrl: "http://127.1:{PORT}/v1",
		divergence: "the WHATWG parser reads 127.1 as 127.0.0.1; net/url takes it as a host name",
		divergentApis: all,
	},
	{
		label: "a pipe in the path",
		baseUrl: "http://127.0.0.1:{PORT}/a|b",
		divergence: "the WHATWG path percent-encode set leaves | alone; net/url escapes it",
		divergentApis: all,
	},
	{
		label: "a query in the base URL",
		baseUrl: "http://127.0.0.1:{PORT}/v1?x=1",
		divergence: "the SDKs re-serialize a query the joined path lands in; the port sends the string's",
		divergentApis: ["google-generative-ai", "anthropic-messages", "openai-completions", "openai-responses"],
	},
	{
		label: "a fragment in the base URL",
		baseUrl: "http://127.0.0.1:{PORT}/v1#frag",
		divergence: "@google/genai sets alt=sse on the parsed URL, before its fragment; the port's alt=sse lands in the fragment",
		divergentApis: ["google-generative-ai"],
	},
];

const requests = [];
for (const a of adapters) {
	for (const b of requestBases) {
		received = undefined;
		const { msg, onPayload } = await stream(a, b.baseUrl.replaceAll("{PORT}", port));
		const unport = (s: string) => s.replaceAll(port, "{PORT}");
		requests.push({
			api: a.api,
			label: b.label,
			baseUrl: b.baseUrl,
			...(b.divergentApis?.includes(a.api) ? { divergence: b.divergence } : {}),
			...(received
				? { request: { line: unport(received.line), host: unport(received.host), authorization: received.authorization } }
				: { errorMessage: unport(msg.errorMessage ?? "") }),
			onPayload,
		});
	}
}
server.close();

fs.writeFileSync(
	outFile,
	`{"source":${JSON.stringify(`upstream ${sha} packages/ai/src/api, ${sdks.join(", ")}, node ${process.version}`)},"rows":[\n${rows.map((r) => JSON.stringify(r)).join(",\n")}\n],"hrefs":[\n${hrefs.map((r) => JSON.stringify(r)).join(",\n")}\n],"customFetch":[\n${customFetch.map((r) => JSON.stringify(r)).join(",\n")}\n],"requests":[\n${requests.map((r) => JSON.stringify(r)).join(",\n")}\n]}\n`,
);
console.log(`captured ${rows.length} rows, ${hrefs.length} hrefs and ${requests.length} requests -> ${outFile}`);
process.exit(0);
