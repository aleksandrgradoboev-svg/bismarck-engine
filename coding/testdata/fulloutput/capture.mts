// Captures what pi's OutputAccumulator makes of shell output — the display
// snapshot the model sees and readFullOutput, the up-to-1-MiB copy a script
// gets — the oracle behind TestOutputAccumulatorSnapshotMatchesPi and
// TestOutputAccumulatorReadFullOutputMatchesPi in package coding.
//
//   node --experimental-strip-types capture.mts <extraction> <out.json> <sha>
//   e.g. ... capture.mts <dir> fulloutput-3dd803d7e.json 3dd803d7e
//
// <extraction> holds packages/coding-agent at <sha> (`git archive <sha>
// packages/coding-agent/src/core/tools`); output-accumulator.ts imports only
// node builtins and truncate.ts, which imports nothing.
//
// Each row appends its chunks (hex), notes whether the accumulator has opened
// a temp file yet, finishes, takes the bash tool's snapshot
// (persistIfTruncated), closes the temp file and reads the full output with
// readMax, as bash.ts does with 1 MiB. The rows use small limits so a few
// bytes cross them, and cover a byte-order mark, characters cut by a chunk or
// by the head/tail split, invalid bytes, and odd limits.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [extraction, outFile, sha] = process.argv.slice(2);
if (!extraction || !outFile || !sha) {
	console.error("usage: node --experimental-strip-types capture.mts <extraction> <out.json> <sha>");
	process.exit(2);
}
const { OutputAccumulator } = await import(
	pathToFileURL(path.join(extraction, "packages/coding-agent/src/core/tools/output-accumulator.ts")).href
);

type Row = { name: string; chunks: string[]; maxLines?: number; maxBytes?: number; readMax: number };
const MiB = 1024 * 1024;
const ascii = (s: string) => Buffer.from(s, "latin1").toString("hex");
const rows: Row[] = [
	{ name: "small output, no temp file", chunks: [ascii("hello\n"), ascii("world")], readMax: MiB },
	{ name: "no output", chunks: [], readMax: MiB },
	{ name: "a byte-order mark, no temp file", chunks: ["efbbbf" + ascii("a\n")], readMax: MiB },
	{ name: "invalid bytes, no temp file", chunks: ["61ff62e282"], readMax: MiB },
	{ name: "a character split across chunks", chunks: ["61e2", "82ac62"], readMax: MiB },
	{ name: "a temp file under readMax", chunks: [ascii("1\n2\n3\n")], maxLines: 2, maxBytes: 1000, readMax: 1000 },
	{ name: "over readMax", chunks: [ascii("0123456789"), ascii("ABCDEFGHIJ")], maxBytes: 8, readMax: 10 },
	{ name: "the head cuts a character", chunks: [ascii("abcd") + "c3a9" + ascii("PQRSTUVWXYZ"), "c3a9" + ascii("1234")], maxBytes: 5, readMax: 10 },
	{ name: "a byte-order mark leads the head", chunks: ["efbbbf" + ascii("hello world tail!")], maxBytes: 5, readMax: 10 },
	{ name: "a byte-order mark leads the tail", chunks: [ascii("AAAAAAAAAA") + "efbbbf" + ascii("zz")], maxBytes: 5, readMax: 10 },
	{ name: "the head ends in an invalid sequence", chunks: [ascii("abc") + "e080" + ascii("xxxxxxxxxx")], maxBytes: 5, readMax: 10 },
	{ name: "the tail ends in a cut character", chunks: [ascii("yyyyyyyyyy") + ascii("abc") + "e282"], maxBytes: 5, readMax: 10 },
	{ name: "an odd readMax", chunks: [ascii("abcdefghijklmnopqrstu")], maxBytes: 5, readMax: 11 },
	{ name: "the tail is all continuation bytes", chunks: [ascii("0123456789") + "8080808080"], maxBytes: 5, readMax: 10 },
	{ name: "invalid bytes and a cut character in the snapshot", chunks: ["61ff62", "e282"], readMax: MiB },
	{ name: "a byte-order mark in the snapshot", chunks: ["efbbbf" + ascii("hi\n")], readMax: MiB },
	{ name: "invalid bytes count decoded", chunks: ["ffffff"], maxBytes: 8, readMax: MiB },
	// 8 raw bytes, 5 decoded: only the raw count passes maxBytes.
	{ name: "a byte-order mark counts in the raw bytes", chunks: ["efbbbf" + ascii("abcde")], maxBytes: 5, readMax: MiB },
];

const out = [];
for (const row of rows) {
	const acc = new OutputAccumulator({ maxLines: row.maxLines, maxBytes: row.maxBytes, tempFilePrefix: "pi-capture" });
	for (const chunk of row.chunks) acc.append(Buffer.from(chunk, "hex"));
	// snapshot() without persisting names a temp file only once the
	// accumulator has opened one, which it does as soon as the raw bytes, the
	// decoded bytes or the lines pass its limits (shouldUseTempFile).
	const tempFileBeforeFinish = acc.snapshot().fullOutputPath !== undefined;
	acc.finish();
	const snapshot = acc.snapshot({ persistIfTruncated: true });
	await acc.closeTempFile();
	const full = await acc.readFullOutput(row.readMax);
	const t = snapshot.truncation;
	out.push({
		...row,
		snapshot: {
			content: snapshot.content,
			truncated: t.truncated,
			truncatedBy: t.truncatedBy,
			totalLines: t.totalLines,
			totalBytes: t.totalBytes,
			outputLines: t.outputLines,
			outputBytes: t.outputBytes,
			lastLinePartial: t.lastLinePartial,
			tempFile: snapshot.fullOutputPath !== undefined,
			tempFileBeforeFinish,
		},
		full,
	});
	if (snapshot.fullOutputPath) fs.rmSync(snapshot.fullOutputPath);
}
fs.writeFileSync(outFile, `${JSON.stringify({ sha, node: process.version, rows: out }, null, "\t")}\n`);
console.log(`captured ${out.length} rows at ${sha} -> ${outFile}`);
