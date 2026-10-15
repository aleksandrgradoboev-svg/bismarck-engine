// Captures the record pi's headersToRecord builds from a real fetch Response —
// the headers every adapter hands onResponse — the oracle behind
// TestResponseHeadersRecordMatchesPi.
//
//   node --experimental-strip-types capture-response-headers.mts <extraction> <out.json> <sha>
//   e.g. ... capture-response-headers.mts <dir> response-headers-49681e1b7.json 49681e1b7
//
// <extraction> holds packages/ai at <sha> (`git archive <sha> packages/ai |
// tar -x -C <dir>` from the upstream clone); only utils/headers.ts is loaded,
// and it imports nothing at runtime. The Response comes from node's own fetch
// (undici) reading a loopback server that writes each row's exact bytes, so the
// Headers object is the one a provider response carries. Each row records the
// raw response (as responseBase64 when it is not text) and the record:
// Headers.entries() yields lowercased names in sorted order, joins a repeated
// name's values with ", " — except set-cookie, whose values it yields one by
// one, so the record keeps the last.
//
// undici keeps every header as sent, including those net/http takes out of
// its header map: Transfer-Encoding, a Trailer header, Connection: close on a
// body with a length or chunks, and — though fetch undoes a gzip body itself —
// Content-Encoding. Rows pin each. Left out: a gzip body with a
// Content-Length (net/http's transparent gunzip drops that header, and the
// length is gone), a close-delimited body's Connection: close (net/http
// leaves no trace of it), and trailing whitespace in a value (net/http trims
// it).
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { pathToFileURL } from "node:url";
import zlib from "node:zlib";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture-response-headers.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { headersToRecord } = await import(
	pathToFileURL(path.join(extraction, "packages/ai/src/utils/headers.ts")).href
);

const response = (headerLines: string[]) =>
	["HTTP/1.1 200 OK", ...headerLines, "Content-Length: 2", "", "ok"].join("\r\n");
// A chunked response: the head, then each chunk, then the last chunk and any
// trailer lines.
const chunked = (headerLines: string[], chunks: Buffer[], trailerLines: string[] = []) =>
	Buffer.concat([
		Buffer.from(["HTTP/1.1 200 OK", ...headerLines, "Transfer-Encoding: chunked", "", ""].join("\r\n")),
		...chunks.flatMap((c) => [Buffer.from(`${c.length.toString(16)}\r\n`), c, Buffer.from("\r\n")]),
		Buffer.from(["0", ...trailerLines, "", ""].join("\r\n")),
	]);

const cases: Array<[string, string | Buffer]> = [
	["repeatedNameJoinsInWireOrder", response(["Content-Type: text/event-stream", "X-Multi: a", "x-multi: b", "X-MULTI: c"])],
	["setCookieKeepsTheLast", response(["Set-Cookie: c=1", "set-cookie: d=2", "Set-Cookie: e=3"])],
	["emptyValuesStillJoin", response(["X-Empty:", "X-Empty2: ", "X-Empty2: v", "X-Mixed: v", "X-Mixed:"])],
	["namesAreLowercased", response(["X-Request-ID: req_1", "Retry-After: 5", "anthropic-ratelimit-requests-remaining: 9"])],
	// The streaming response every SSE adapter reads.
	["chunkedTransferEncodingIsKept", chunked(["Content-Type: text/event-stream"], [Buffer.from("data: {}\n\n")])],
	["connectionCloseIsKept", response(["Content-Type: text/event-stream", "Connection: close"])],
	["connectionCloseOnAChunkedBodyIsKept", chunked(["Connection: close"], [Buffer.from("ok")])],
	["trailerIsKept", chunked(["Trailer: X-Checksum"], [Buffer.from("ok")], ["X-Checksum: abc"])],
	// A trailer is no header: fetch's Headers never hold one, announced in a
	// Trailer header or not, however the body is read. (net/http merges every
	// trailer it reads into resp.Trailer, which is why the record is taken
	// before the body is read, as the adapters take it.)
	["aTrailerNobodyAnnouncedIsNoHeader", chunked(["Content-Type: text/event-stream"], [Buffer.from("ok")], ["X-Extra: 1"])],
	[
		"anUnannouncedTrailerBesideAnAnnouncedOne",
		chunked(["Trailer: X-Checksum"], [Buffer.from("ok")], ["X-Checksum: abc", "X-Extra: 1"]),
	],
	["gzipContentEncodingIsKept", chunked(["Content-Type: text/event-stream", "Content-Encoding: gzip"], [zlib.gzipSync("data: {}\n\n")])],
	// A body read until the connection closes has no Connection header to
	// keep (net/http marks it closing all the same).
	["closeDelimitedBodyHasNoConnection", ["HTTP/1.1 200 OK", "Content-Type: text/event-stream", "", "data: {}\n\n"].join("\r\n")],
];

async function capture(raw: string | Buffer): Promise<Record<string, string>> {
	const server = net.createServer((socket) => {
		socket.once("data", () => socket.end(raw));
	});
	await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
	try {
		const { port } = server.address() as net.AddressInfo;
		const res = await fetch(`http://127.0.0.1:${port}/`);
		await res.text();
		return headersToRecord(res.headers);
	} finally {
		server.close();
	}
}

const rows = [];
for (const [name, raw] of cases) {
	const text = typeof raw === "string" ? raw : raw.toString("utf8");
	const exact = typeof raw === "string" || Buffer.from(text, "utf8").equals(raw);
	rows.push({
		name,
		...(exact ? { response: text } : { responseBase64: (raw as Buffer).toString("base64") }),
		record: await capture(raw),
	});
}
// One row per line, so a re-capture diffs row by row.
const text = rows.map((row) => JSON.stringify(row)).join(",\n");
fs.writeFileSync(
	outFile,
	`{"sha":${JSON.stringify(sha)},"node":${JSON.stringify(process.version)},"undici":${JSON.stringify(process.versions.undici)},"rows":[\n${text}\n]}\n`,
);
console.log(`captured ${rows.length} rows from packages/ai/src/utils/headers.ts at ${sha} -> ${outFile}`);
