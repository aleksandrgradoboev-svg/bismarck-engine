// Captures V8's JSON.parse SyntaxError messages — the oracle behind
// TestJSONSyntaxErrorMatchesNode (../jsonparse_test.go).
//
//   node capture-json-parse-errors.mjs > json-parse-errors-node.json
//
// Plain node, no pi source: JSON.parse is the engine's. The corpus is a
// hand table covering every message template and V8's choices between them
// (quoted context lengths, special strings, line and column counting,
// UTF-16 positions), then every prefix of three seed documents and every
// single code-unit replacement in two of them, over an alphabet of the code
// units that change which error V8 reports. Each row is [input, message],
// message null where JSON.parse accepts the input.
const table = [
	"", " ", "\r\n", "{", "[", "{not json}", '{"a"', '{"a":', '{"a":1', '{"a":1,', '{"a":1,}', '{"a" 1}', '{"a":1 "b":2}',
	"[1", "[1,", "[1,]", "[1 2]", "[,1]", "{,}", '{"a":1}x', '{"a":1} ', '{"a":1}\n\n', '{"a":1}\ndata: {"b":2}',
	"tru", "trux", "nul1", 'tr"', "tr-", "nan", "NaN", " NaN", "NaN ", "undefined", "Infinity", "-Infinity", "[object Object]",
	"[object Object] ", "x", "xyz", "n", "f", "fals", "falsy", "truee", "true false",
	"01", "-", "--1", "-.5", "-e5", "-a", "-01", "1.", "1.a", "1e", "1e+", "1e-", "1ea", "0x10", "00", "-0", "-0x", "0.e1", "0e",
	"0e1", "0.0e+1", "1.5e3", "1E5", ".5", "+1", "[1.]", "[-]", "[01]", "[-0x]", "[1e5x]", "1_000", "9999999999", "123456789012",
	"-123456789.5", "1234567890x", "0+", "-0+", "0-1",
	'"abc', '"a\tb"', '"a\\', '"a\\x"', '"\\u12"', '"\\u12g4"', '"\\u"', '"\\u00"', '"\\u004G"', '"a\\u00e9"', '"\\é"', '"\\中"',
	'"\\/"', '"\\b\\f\\n\\r\\t"', '"\\a"', '"\\U0041"', '"x\u007f"', '"x\u001f"', '"\u0000"', '"\\ud800"', '"\r\n"',
	'{"candidates":[{"content":{"parts":[{"text":"a\tb"}]}}]}', '{"candidates":[]}\ndata: {"candidates":[]}',
	"abcdefghijklmnopqrs", "abcdefghijklmnopqrst", "abcdefghijklmnopqrstu", "abcdefghijklmnopqrstuv",
	"[1,2,3,4,x,6,7,8,9,10,11,12]", "[1,2,3,4,5,x,7,8,9,10,11,12]", "[1,2,3,4,5x,6,7,8,9,10,11,12]",
	"[1,2,3,4,5,6,7,8,9,10,11,x,13]", "[1,2,3,4,5,6,7,8,9,10,11,1x3]", "[1,2,3,4,5,6,7,8,9,10,11,12,x]",
	"[1,2,3,4,5,6,7,8,9,10,11,12,1,x]", "[1,2,3,4,5,6,7,8,9,10,11,12,13,x]", "[1,2,3,4,5,6,7,8,9,10,11,12,1,2,x]",
	"[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,}", "x234567890123456789012", "12345678901234567890x",
	'{"aaaaaaaaaaaaaaaaaaaaaaa": x}', '{"a":1, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": 2, x}',
	'{"a":1}\r\n\r\nx', '{\r"a":x}', '{\r"a":1 x}', '{\r\r"a":1 x}', '{\n\r"a":1 x}', '{\r\n\n"a":1 x}', '{"a":1,\n  "b" 2}',
	"x\n", "\n\nx", '{"a":é}', '{"a":中}', '{"a":"中文", x}', '["\u{1F600}", x]', '"\u{1F600}\u0001"',
	'{"a":1}}', "[]]", "{}{", '{"a":tru}', '{"a":nulx}', '{"\\u0041":1,x}', "[1,2", '{"a":[1,{"b":2}', '{"a"x', "{1:2}",
	"{'a':1}", "[\t]", "[ ]", '{"a": 1}', '{"a":1 }', "﻿{}", '[1,"a",tx]', '{"a":1,"b":[true,fals]}',
];
const seeds = [
	'{"candidates":[{"content":{"parts":[{"text":"hi\\n","thought":true}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"totalTokenCount":3}}',
	'[1,-2.5e+3,0,-0,true,false,null,"s\\u00e9\\"",{"a":[]},{}]',
	' \t\r\n{ "k" : [ 1 , 2 ] , "l" : "中\u{1F600}" } \n',
];
const alphabet = ["x", '"', "\\", "}", "]", ",", ":", "-", "0", ".", "e", " ", "\n", "\u0001", "{", "[", "t", "n", "é", "中"];

const inputs = new Set(table);
for (const seed of seeds) for (let i = 0; i <= seed.length; i++) inputs.add(seed.slice(0, i));
for (const seed of seeds.slice(1)) {
	for (let i = 0; i < seed.length; i++) {
		for (const c of alphabet) inputs.add(seed.slice(0, i) + c + seed.slice(i + 1));
	}
}
const rows = [];
for (const input of inputs) {
	let message = null;
	try {
		JSON.parse(input);
	} catch (e) {
		message = e.message;
	}
	rows.push([input, message]);
}
process.stdout.write(
	`{"node":${JSON.stringify(process.version)},"rows":[\n${rows.map((r) => JSON.stringify(r)).join(",\n")}\n]}\n`,
);
