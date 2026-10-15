// Captures what one decode call of a fresh TextDecoder("utf-8") makes of a
// byte sequence, with and without {stream: true} — the oracle behind
// TestDecodeTextMatchesNode (../utf8_test.go).
//
//   node capture-textdecode.mjs > textdecode-node.json
//
// Plain node, no pi source: pi decodes whole files and file halves this way
// (coding-agent's OutputAccumulator.readFullOutput). The inputs cover a
// byte-order mark (alone, leading, second, split, after text), sequences cut
// short at the end, invalid bytes at the end, and valid text; each is decoded
// once in each mode, the decoder never called again.
const hex = (s) => Uint8Array.from(s.match(/../g) ?? [], (h) => parseInt(h, 16));
const inputs = [
	"", "61", "efbbbf", "efbbbf61", "efbbbfefbbbf61", "61efbbbf", "efbb", "ef", "efbbbfc3",
	"c3", "c3a9", "61c3", "61e2", "61e282", "61e282ac", "61f0", "61f09f", "61f09f98", "61f09f9880",
	"61e080", "61e0", "61eda0", "61ed", "61f490", "61f4", "61f5", "61ff", "6180", "61c0", "61c1",
	"80", "bfbf", "c3a9c3", "e282ace2", "f09f9880f09f", "61ff62e282", "efbbbfe282", "efbbbf80",
	"e282acefbbbf", "0a0a0a",
];
const rows = [];
for (const input of inputs) {
	for (const stream of [false, true]) {
		rows.push({ input, stream, text: new TextDecoder().decode(hex(input), { stream }) });
	}
}
process.stdout.write(`${JSON.stringify({ node: process.version, rows }, null, "\t")}\n`);
