// Captures JavaScript's own answer to "what does String.prototype.toLowerCase
// return?" -- the oracle behind TestToLowerMatchesNode (../jstext_test.go).
//
//   node capture-lower.mjs > lower-node.json
//
// Plain node, no pi source: toLowerCase is the ECMAScript primitive pi calls
// to fold header names (providerHeadersToRecord, mergeHeaders, hasHeader). It
// applies the full Unicode default case mapping, SpecialCasing included, so a
// character can lower to more than one (U+0130 lowers to "i" + U+0307), and a
// capital sigma lowers by context (Final_Sigma). This records the engine's
// answer for every Unicode scalar value that toLowerCase changes (surrogates
// excluded, since a Go string cannot hold a lone one), plus a table of whole
// strings that pins the context-dependent sigma.
const MAX = 0x10ffff;
const lower = [];
for (let c = 0; c <= MAX; c++) {
	if (c >= 0xd800 && c <= 0xdfff) continue;
	const s = String.fromCodePoint(c);
	const l = s.toLowerCase();
	if (l !== s) lower.push([c, l]);
}

const samples = [
	"",
	"x",
	"ABC",
	"X-\u0130D",
	"\u0130",
	"X\u03a3",
	"\u03a3",
	"X\u03a3-A",
	"A\u03a3\u03a3",
	"\u03a3A",
	"A.\u03a3",
	"A\u03a3.",
	"A'\u03a3",
	"A\u03a3'",
	"A\u03a3'B",
	"A\u00ad\u03a3",
	"1\u03a3",
	"\u212a",
	"X-\u1e9e",
	"A\u03a3 B\u03a3",
	"\ud835\udc00\u03a3",
	"A\u0345\u03a3",
	"\u0130\u0130",
	"I\u0307",
];

const out = {
	node: process.version,
	unicode: process.versions.unicode,
	lower,
	strings: samples.map((s) => ({ in: s, lower: s.toLowerCase() })),
};
process.stdout.write(`${JSON.stringify(out)}\n`);
