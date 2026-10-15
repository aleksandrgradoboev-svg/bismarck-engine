// Captures JavaScript's own answer to "which code points does String.prototype.trim
// remove?" — the oracle behind TestWhitespaceMatchesNode (../jstext_test.go).
//
//   node capture-whitespace.mjs > whitespace-node.json
//
// Plain node, no pi source: trim, trimStart and trimEnd are the ECMAScript
// primitives pi calls, and this records the engine's answer for every Unicode
// scalar value (surrogates excluded, since a Go string cannot hold a lone
// one), plus Number.parseFloat's leading-whitespace set, which ECMA-262 defines
// with the same StrWhiteSpaceChar production, and a table of whole strings so
// the trims' behaviour on mixed runs is pinned too.
const MAX = 0x10ffff;
const scalars = function* () {
	for (let c = 0; c <= MAX; c++) {
		if (c >= 0xd800 && c <= 0xdfff) continue;
		yield c;
	}
};
const collect = (pred) => {
	const out = [];
	for (const c of scalars()) if (pred(String.fromCodePoint(c))) out.push(c);
	return out;
};

const samples = [
	"",
	"x",
	" \t\n\r x \r\n",
	"\ufeffx\ufeff",
	"\u0085x\u0085",
	"\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000x\u3000",
	"\u180ex\u180e",
	"\u200bx\u200b",
	"\u0085\ufeff",
	"\ufeff\u0085",
	"a b",
	"\u2003\u0085\u2003",
];

const out = {
	node: process.version,
	unicode: process.versions.unicode,
	trim: collect((s) => s.trim() === ""),
	trimStart: collect((s) => s.trimStart() === ""),
	trimEnd: collect((s) => s.trimEnd() === ""),
	parseFloatLeading: collect((s) => Number.parseFloat(`${s}-7.5`) === -7.5),
	strings: samples.map((s) => ({ in: s, trim: s.trim(), trimStart: s.trimStart(), trimEnd: s.trimEnd() })),
};
process.stdout.write(`${JSON.stringify(out, null, "\t")}\n`);
