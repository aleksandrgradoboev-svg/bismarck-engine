// Captures what a TextDecoder makes of invalid UTF-8 — the oracle behind
// TestDecodeUTF8MatchesNode (../utf8_test.go).
//
//   node capture-utf8.mjs > utf8-node.json
//
// Plain node, no pi source: the openai SDK decodes each SSE line with a
// TextDecoder (internal/utils/bytes.js decodeUTF8), which is the WHATWG UTF-8
// decoder in replacement mode. The cases are "a", a lead byte from 0x80 to
// 0xFF, then up to three bytes from a set that straddles every boundary the
// decoder checks, and "b". Each lead byte's cases are hashed in order (SHA-256
// over each decoded string's UTF-8 and a 0xFF separator, which no decoded
// string contains), so every case is compared while the file stays small; the
// samples are there to read.
import { createHash } from "node:crypto";

const follow = [
	0x00, 0x41, 0x7f, 0x80, 0x8f, 0x90, 0x9f, 0xa0, 0xbf, 0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xed, 0xef, 0xf0, 0xf4,
	0xf5, 0xff,
];

function* cases(lead) {
	yield [lead];
	for (const b1 of follow) {
		yield [lead, b1];
		for (const b2 of follow) {
			yield [lead, b1, b2];
			for (const b3 of follow) yield [lead, b1, b2, b3];
		}
	}
}

const decoder = new TextDecoder();
const encoder = new TextEncoder();
const decode = (bytes) => decoder.decode(Uint8Array.from([0x61, ...bytes, 0x62]));
const digests = {};
for (let lead = 0x80; lead <= 0xff; lead++) {
	const hash = createHash("sha256");
	for (const bytes of cases(lead)) {
		hash.update(encoder.encode(decode(bytes)));
		hash.update(Uint8Array.of(0xff));
	}
	digests[lead.toString(16)] = hash.digest("hex");
}
const samples = [
	[0xe2, 0x82],
	[0xe2, 0x82, 0xac],
	[0xf0, 0x9f, 0x98],
	[0xc0, 0xaf],
	[0xed, 0xa0, 0x80],
	[0xf4, 0x90, 0x80, 0x80],
	[0x80, 0xbf],
	[0xff, 0xe0, 0x80],
	[0xe0, 0xa0],
	[0xf0, 0x90, 0x80],
];
const sampleOut = samples.map((bytes) => ({
	bytes: bytes.map((b) => b.toString(16).padStart(2, "0")).join(" "),
	decoded: [...decode(bytes)].map((c) => c.codePointAt(0).toString(16).padStart(4, "0")).join(" "),
}));
console.log(JSON.stringify({ node: process.version, follow, digests, samples: sampleOut }, null, "\t"));
