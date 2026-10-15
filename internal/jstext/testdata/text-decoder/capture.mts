// Captures what node's TextDecoder("utf-8") makes of a byte stream decoded
// one read at a time with decode(read, {stream: true}) and never flushed —
// @google/genai's processStreamResponse — the oracle behind
// TestTextDecoderMatchesNode in package jstext.
//
//   node capture.mts <out.json>
//   e.g. node capture.mts text-decoder-node.json
//
// Each input is decoded whole in one read, and split into two reads at every
// position and into three at every pair of positions; a row records the
// input (base64), the cut positions, and the text of each read's decode (the
// bytes still held when the stream ends are dropped, as the SDK drops them).
// The inputs cover a leading byte-order mark (whole, split, twice, after
// other text), two-, three- and four-byte characters, and invalid sequences:
// truncated ones, overlong forms, surrogates, code points past U+10FFFF,
// bytes no sequence starts with, and stray continuation bytes.
//
// node's UTF-8 fast path (lib/internal/encoding.js, v26) departs from the
// Encoding Standard in one case: when a read of three or more bytes completes
// a character the reads before it left unfinished, and no text has been
// decoded yet, the rest of that read is decoded with `ignoreBom || prefix` —
// a Uint8Array where the binding keeps a byte-order mark only for `true` — so
// a byte-order mark leading that rest is dropped as well. A row where the
// reads' texts joined differ from the Standard's — the input decoded in one
// read, which never takes that path — is tagged nodeQuirk and carries the
// Standard's join (standardJoin).
import fs from "node:fs";

const [outFile] = process.argv.slice(2);
if (!outFile) {
	console.error("usage: node capture.mts <out.json>");
	process.exit(2);
}

const b = (...parts: Array<string | number[]>) =>
	Buffer.concat(parts.map((p) => (typeof p === "string" ? Buffer.from(p, "latin1") : Buffer.from(p))));
const bom = [0xef, 0xbb, 0xbf];
const inputs: Buffer[] = [
	b("plain"),
	b(bom, "x"),
	b(bom),
	b(bom, bom, "x"),
	b("x", bom),
	b("caf", [0xc3, 0xa9]),
	b([0xe2, 0x82, 0xac], "z"),
	b([0xf0, 0x9f, 0x98, 0x80]),
	b("a", [0xe2, 0x82], "Z"),
	b("a", [0xe2, 0x82]),
	b([0xf0, 0x9f, 0x98]),
	b([0xe0, 0x80, 0x80]),
	b([0xe0, 0xa0, 0x80]),
	b([0xed, 0xa0, 0x80]),
	b([0xed, 0x9f, 0xbf]),
	b([0xf0, 0x80, 0x80, 0x80]),
	b([0xf4, 0x90, 0x80, 0x80]),
	b([0xf4, 0x8f, 0xbf, 0xbf]),
	b([0xc0, 0xaf], "d"),
	b([0xc1, 0xbf]),
	b([0xf5, 0x80]),
	b([0xff], "e"),
	b([0x80, 0x80], "f"),
	b([0xc3], "g", [0xa9]),
	b([0xe2, 0x82, 0xe2, 0x82, 0xac]),
	b(bom.slice(0, 2), "x"),
];

const decodeReads = (input: Buffer, cuts: number[]) => {
	const decoder = new TextDecoder();
	const reads: string[] = [];
	let at = 0;
	for (const cut of [...cuts, input.length]) {
		reads.push(decoder.decode(input.subarray(at, cut), { stream: true }));
		at = cut;
	}
	return reads;
};

const rows = [];
for (const input of inputs) {
	const splits: number[][] = [[]];
	for (let i = 1; i < input.length; i++) splits.push([i]);
	for (let i = 1; i < input.length; i++) for (let j = i + 1; j < input.length; j++) splits.push([i, j]);
	const standardJoin = decodeReads(input, [])[0];
	for (const cuts of splits) {
		const reads = decodeReads(input, cuts);
		const quirk = reads.join("") !== standardJoin;
		rows.push({ input: input.toString("base64"), cuts, reads, ...(quirk ? { nodeQuirk: true, standardJoin } : {}) });
	}
}
fs.writeFileSync(
	outFile,
	`{"node":${JSON.stringify(process.version)},"rows":[\n${rows.map((r) => JSON.stringify(r)).join(",\n")}\n]}\n`,
);
console.log(`captured ${rows.length} decodes -> ${outFile}`);
