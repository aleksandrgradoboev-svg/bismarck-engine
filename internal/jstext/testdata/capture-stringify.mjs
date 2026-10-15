// Captures JSON.stringify output for the cases TestStringifyMatchesNode pins.
//   node capture-stringify.mjs > stringify-node.json
const cases = [
	{ s: '<a href="x">&amp;</a>' },
	{ s: "line\u2028sep\u2029para" },
	{ s: "back\\u2028slash" },
	{ s: "\\\u2028" },
	{ s: "emoji \u{1F600} and \u00e9" },
	{ s: 'tab\tnl\ncr\rquote"bs\\' },
	{ s: "ctl\u0001\u001f del\u007f" },
	{ a: [1, 2.5, -0.25, true, null], n: { k: "v" } },
	{},
	[],
	"plain",
];
process.stdout.write(JSON.stringify(cases.map((v) => ({ input: v, json: JSON.stringify(v) })), null, 1) + "\n");
