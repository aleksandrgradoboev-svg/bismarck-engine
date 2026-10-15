// Captures what pi's pi-messages adapter says for a non-2xx response, per
// status line — the oracle behind TestPiMessagesStatusTextMatchesPi.
//
//   node --experimental-strip-types capture-pi-messages-status.mts <extraction> <out.json> <sha>
//   e.g. ... capture-pi-messages-status.mts <dir> pi-messages-status-49681e1b7.json 49681e1b7
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone), with packages/ai/node_modules
// resolving pi-ai's dependencies; pi-messages calls node's own fetch, so no
// SDK is involved.
//
// A raw TCP server answers each request with a row's status line, byte for
// byte, then Content-Type: application/json and a fixed error body. pi's
// errorMessage is `${response.status} ${response.statusText}: <message>
// (<code>)` and its pi_messages_response_failure details carry the
// statusText, which undici reads from the status line as sent: the reason
// phrase after the code's one space (nothing when there is none), decoded as
// UTF-8. Each row records the status line (as statusLineBase64 when it is
// not UTF-8), the errorMessage, and the details' status and statusText.
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-pi-messages-status.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { stream } = await import(pathToFileURL(path.join(extraction, "packages/ai/src/api/pi-messages.ts")).href);

const body = '{"error":{"message":"nope","code":"c"}}';
const bytes = (...parts: Array<string | number[]>) =>
	Buffer.concat(parts.map((p) => (typeof p === "string" ? Buffer.from(p, "latin1") : Buffer.from(p))));
const cases: Array<[string, Buffer]> = [
	["theStandardReasonPhrase", bytes("HTTP/1.1 401 Unauthorized")],
	["aReasonPhraseOfTheServersOwn", bytes("HTTP/1.1 401 Token Expired")],
	["anEmptyReasonPhrase", bytes("HTTP/1.1 401 ")],
	["noReasonPhrase", bytes("HTTP/1.1 401")],
	["spacesInTheReasonPhraseAreKept", bytes("HTTP/1.1 401  two  spaces ")],
	["aStatusWithNoStandardReasonPhrase", bytes("HTTP/1.1 599 Network Connect Timeout")],
	["anHTTP10StatusLine", bytes("HTTP/1.0 503 Busy")],
	["aTabInTheReasonPhrase", bytes("HTTP/1.1 500 tab\there")],
	["aUTF8ReasonPhrase", bytes("HTTP/1.1 500 caf", [0xc3, 0xa9])],
	["aReasonPhraseThatIsNotUTF8", bytes("HTTP/1.1 500 caf", [0xe9], " ", [0xc3, 0xbf])],
];

let statusLine = Buffer.alloc(0);
const server = net.createServer((sock) => {
	let buf = Buffer.alloc(0);
	sock.on("data", (d) => {
		buf = Buffer.concat([buf, d]);
		const headEnd = buf.indexOf("\r\n\r\n");
		if (headEnd < 0) return;
		const len = Number(/content-length:\s*(\d+)/i.exec(buf.subarray(0, headEnd).toString())?.[1] ?? 0);
		if (buf.length < headEnd + 4 + len) return;
		buf = Buffer.alloc(0);
		sock.end(
			Buffer.concat([statusLine, Buffer.from(`\r\nContent-Type: application/json\r\nContent-Length: ${body.length}\r\n\r\n${body}`)]),
		);
	});
	sock.on("error", () => {});
});
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = (server.address() as net.AddressInfo).port;

const rows = [];
for (const [name, line] of cases) {
	statusLine = line;
	const model = {
		id: "auto",
		name: "Radius Auto",
		api: "pi-messages",
		provider: "radius",
		baseUrl: `http://127.0.0.1:${port}/v1`,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 128000,
		maxTokens: 16384,
	};
	const s = stream(model, { messages: [{ role: "user", content: "hi", timestamp: 1 }] }, { apiKey: "test-key" });
	for await (const _ of s) {
	}
	const message = await s.result();
	const details = message.diagnostics?.[0]?.details;
	const text = line.toString("utf8");
	rows.push({
		name,
		...(Buffer.from(text, "utf8").equals(line) ? { statusLine: text } : { statusLineBase64: line.toString("base64") }),
		errorMessage: message.errorMessage,
		status: details?.status,
		statusText: details?.statusText,
	});
}
server.close();

// One row per line, so a re-capture diffs row by row.
fs.writeFileSync(
	outFile,
	`{"sha":${JSON.stringify(sha)},"node":${JSON.stringify(process.version)},"undici":${JSON.stringify(process.versions.undici)},"body":${JSON.stringify(body)},"rows":[\n${rows.map((r) => JSON.stringify(r)).join(",\n")}\n]}\n`,
);
console.log(`captured ${rows.length} rows from packages/ai/src/api/pi-messages.ts at ${sha} -> ${outFile}`);
process.exit(0);
