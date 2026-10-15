// Emits v8 protocol vectors from upstream's real codec + TypeBox schemas
// (packages/protocol, PROTOCOL_VERSION 8). Last run at 49681e1b7, whose
// protocol src is 64eeb82a4's, with typebox 1.3.27 (the version and integrity
// that sha's package-lock.json names); the vectors 64eeb82a4 produced under
// typebox 1.3.7 came out byte-identical.
//
// Run from a copy placed in the extracted packages/protocol/src/, with typebox
// and @earendil-works/chord (packages/chord/src at the same sha) resolvable
// from there:
//
//	node --experimental-strip-types gen-protocol-v8.ts > upstream_protocol_v8.json
//
// Every frame is what encodeClientMessage / encodeServerMessage produced, every
// reject is what parseClientMessage / parseServerMessage refused. Nothing here
// is derived from the Go port.
import { isJsonValue } from "@earendil-works/chord";
import { decodeCbor, encodeCbor } from "./cbor/index.ts";
import { encodeClientMessage, encodeServerMessage, parseClientMessage, parseServerMessage } from "./codec.ts";
import { encodeFrame } from "./framing.ts";
import { PROTOCOL_VERSION } from "./protocol.ts";

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");

const serverId = "00000000-0000-4000-8000-000000000001";
const serverTarget = { serverId };
const sessionTarget = { serverId, sessionId: "session-1", attachmentId: "attachment-1" };
const clientHello = { type: "hello", version: PROTOCOL_VERSION };
const serverHello = { type: "hello", version: PROTOCOL_VERSION, serverId };

// Calls from upstream's protocol.test.ts.
const listCall = { serviceId: "pi.session-directory", member: "list", args: [] };
const opaqueCall = {
	serviceId: "application.custom",
	instance: { key: "instance-1", generation: 2 },
	member: "invoke",
	args: [{ arbitrary: true }, ["opaque"]],
};
// Insertion-ordered payloads. No key is in sorted position at any depth — top
// level, a nested object, and an object inside an array — so a Go peer that
// decoded these to maps and re-encoded them would re-order every level. The
// raw-span relay must reproduce them byte for byte.
const orderedArguments = {
	path: "/tmp",
	depth: 1,
	filters: [{ name: "go", enabled: true }],
	nested: { zeta: 1, alpha: 2 },
};
const orderedCall = {
	serviceId: "application.custom",
	member: "invoke",
	args: [orderedArguments],
	instance: { key: "instance-1", generation: 2 },
};
const orderedResult = {
	zeta: [{ m: 1.5, b: null }],
	alpha: "x",
	nested: { b: { d: 1, c: 2 }, a: [] },
};
const orderedUpdate = { applicationDefined: true, zeta: [{ m: 1.5, b: null }], alpha: "x" };
// The JSON scalar range an opaque payload may carry, each in the CBOR form
// upstream's encoder chooses for it. Integers stop at the safe-integer bound
// and every integer-valued float (1e300 included) counts as an integer there,
// so a float must have a fraction to be encodable.
const scalarsCall = {
	serviceId: "pi.echo",
	member: "echo",
	args: [
		0, -1, 1.5, -0.25, 3.141592653589793, 1e-7, 4294967296, 9007199254740991, -9007199254740991,
		"", "héllo ✓ 日本", true, false, null, {}, [], { "": [null] },
	],
};

const clientMessages: Array<[string, unknown]> = [
	["hello", clientHello],
	["hello_version_zero", { type: "hello", version: 0 }],
	["hello_version_next", { type: "hello", version: PROTOCOL_VERSION + 1 }],
	["request_server_target", { type: "request", id: "request-1", target: serverTarget, call: listCall }],
	["request_session_target_opaque", { type: "request", id: "request-1", target: sessionTarget, call: opaqueCall }],
	["request_ordered_call", { type: "request", id: "request-2", target: sessionTarget, call: orderedCall }],
	["request_scalars_call", { type: "request", id: "request-3", target: serverTarget, call: scalarsCall }],
	["request_call_null", { type: "request", id: "request-4", target: serverTarget, call: null }],
	["request_call_array", { type: "request", id: "request-5", target: serverTarget, call: [{ b: 1, a: 2 }, "x"] }],
	["request_call_string", {
		type: "request", id: "request-6", target: serverTarget,
		call: "strict JSON whose service meaning belongs to Chord",
	}],
	["cancel_server_target", { type: "cancel", id: "request-1", target: serverTarget }],
	["cancel_session_target", { type: "cancel", id: "request-2", target: sessionTarget }],
	// The edges of what the schemas admit: an id needs one character, any
	// character, and a hello's version is any safe non-negative integer.
	["request_single_character_ids", {
		type: "request", id: " ", target: { serverId, sessionId: "\t", attachmentId: "✓" }, call: null,
	}],
	["hello_version_max_safe", { type: "hello", version: Number.MAX_SAFE_INTEGER }],
];

const serverMessages: Array<[string, unknown]> = [
	["hello", serverHello],
	["hello_error", {
		type: "hello_error",
		error: { code: "unsupported_version", message: "Unsupported protocol version 7" },
	}],
	["response_ok_void", { type: "response", id: "request-1", ok: true }],
	["response_ok_empty_array", { type: "response", id: "request-1", ok: true, result: [] }],
	["response_ok_ordered_result", { type: "response", id: "request-2", ok: true, result: orderedResult }],
	["response_ok_null", { type: "response", id: "request-3", ok: true, result: null }],
	["response_ok_number", { type: "response", id: "request-4", ok: true, result: 42 }],
	["response_ok_string", { type: "response", id: "request-5", ok: true, result: "done" }],
	...(["wrong_server", "cancelled", "service_not_found", "application_error"] as const).map(
		(code): [string, unknown] => [
			`response_error_${code}`,
			{ type: "response", id: "request-1", ok: false, error: { code, message: "safe" } },
		],
	),
	["service_update_ordered", { type: "service_update", subscriptionId: "subscription-1", update: orderedUpdate }],
	["service_update_array", { type: "service_update", subscriptionId: "subscription-2", update: [1, "two", null] }],
	["service_update_null", { type: "service_update", subscriptionId: "subscription-3", update: null }],
	["attachment_attached", { type: "attachment", attachment: sessionTarget }],
	["attachment_detached", { type: "attachment", attachment: null }],
	// An error's code must be non-empty; its message may be empty.
	["response_error_empty_message", { type: "response", id: "request-1", ok: false, error: { code: "cancelled", message: "" } }],
	["hello_error_empty_message", { type: "hello_error", error: { code: "busy", message: "" } }],
];

function encodeAll(cases: Array<[string, unknown]>, encode: (m: never) => Uint8Array) {
	return cases.map(([name, value]) => {
		if (!isJsonValue(value)) throw new Error(`${name}: accepted vectors must be JSON`);
		return { name, value, frame: hex(encode(value as never)) };
	});
}

// Values the parser must REJECT: the invalid inputs of upstream's
// protocol.test.ts (source = the test's name) plus a few the schemas imply
// (source = "schema"). Each carries its wire form only when upstream's own
// encoder produces one AND upstream refuses that form once decoded; the
// remaining rejects exist only as JS values (a NaN or a cycle cannot be
// encoded, an undefined property is dropped by the encoder so its bytes are a
// valid message) and say so.
const cyclic: { self?: unknown } = {};
cyclic.self = cyclic;
const request = { type: "request", id: "request-1", target: serverTarget, call: { serviceId: "pi.models", member: "list", args: [] } };
const cancel = { type: "cancel", id: "request-1", target: serverTarget };
const attached = { type: "attachment", attachment: sessionTarget };
const okResponse = { type: "response", id: "request-1", ok: true, result: [] };
const errorResponse = { type: "response", id: "request-1", ok: false, error: { code: "application_error", message: "bad" } };
const withArgs = (value: unknown) => ({ ...request, call: { ...request.call, args: [value] } });
const nonJson: Array<[string, unknown]> = [
	["byte_array", new Uint8Array([1])],
	["non_finite_number", Number.NaN],
	["undefined_property", { value: undefined }],
	["cycle", cyclic],
];

const clientRejects: Array<[string, string, unknown]> = [
	["hello_version_string", "rejects an invalid client hello", { type: "hello", version: String(PROTOCOL_VERSION) }],
	["hello_version_fraction", "rejects an invalid client hello", { type: "hello", version: PROTOCOL_VERSION + 0.5 }],
	["hello_extra_field", "rejects an invalid client hello", { ...clientHello, extra: true }],
	...([
		["server_id_empty", ""],
		["server_id_not_uuid", "server-1"],
		["server_id_version_7", "00000000-0000-7000-8000-000000000001"],
		["server_id_variant_7", "00000000-0000-4000-7000-000000000001"],
		["server_id_uppercase", "00000000-0000-4000-8000-00000000000A"],
	] as const).map(([name, id]): [string, string, unknown] => [
		name, "rejects non-canonical UUIDv4 server ID", { ...request, target: { serverId: id } },
	]),
	...nonJson.map(([label, value]): [string, string, unknown] => [
		`call_${label}`, "rejects non-JSON opaque payloads", withArgs(value),
	]),
	["cancel_empty_id", "validates request cancellation envelopes", { ...cancel, id: "" }],
	["cancel_extra_field", "validates request cancellation envelopes", { ...cancel, extra: true }],
	["request_empty_id", "rejects malformed request boundaries", { ...request, id: "" }],
	["request_extra_field", "rejects malformed request boundaries", { ...request, extra: true }],
	["string_message", "does not parse JSON strings as messages", JSON.stringify(clientHello)],
	["hello_version_negative", "schema", { type: "hello", version: -1 }],
	["unknown_type", "schema", { type: "goodbye" }],
	["request_missing_target", "schema", { type: "request", id: "request-1", call: request.call }],
	["request_missing_call", "schema", { type: "request", id: "request-1", target: serverTarget }],
	["target_session_missing_attachment", "schema", { ...request, target: { serverId, sessionId: "session-1" } }],
	["target_server_with_attachment", "schema", { ...request, target: { serverId, attachmentId: "attachment-1" } }],
	["target_not_object", "schema", { ...request, target: serverId }],
	["null_message", "schema", null],
	["array_message", "schema", []],
];

const serverRejects: Array<[string, string, unknown]> = [
	...nonJson.map(([label, value]): [string, string, unknown] => [
		`result_${label}`, "rejects non-JSON opaque payloads", { ...okResponse, result: value },
	]),
	["attachment_session_id_only", "validates attachment route updates", { ...attached, attachment: { sessionId: "session-1" } }],
	["hello_invalid_server_id", "rejects malformed server boundaries", { ...serverHello, serverId: "server-1" }],
	["response_extra_field", "rejects malformed server boundaries", { ...okResponse, extra: true }],
	["response_empty_error_code", "rejects malformed server boundaries", { ...errorResponse, error: { code: "", message: "bad" } }],
	["hello_with_snapshot", "rejects unknown messages and fields", { ...serverHello, snapshot: {} }],
	["unknown_type", "rejects unknown messages and fields", { type: "unknown", event: {} }],
	["string_message", "does not parse JSON strings as messages", JSON.stringify(serverHello)],
	["hello_version_previous", "schema", { ...serverHello, version: PROTOCOL_VERSION - 1 }],
	["hello_version_string", "schema", { ...serverHello, version: String(PROTOCOL_VERSION) }],
	["hello_error_extra_field", "schema", { type: "hello_error", error: { code: "busy", message: "m" }, extra: true }],
	["hello_error_missing_message", "schema", { type: "hello_error", error: { code: "busy" } }],
	["response_ok_with_error", "schema", { ...okResponse, error: errorResponse.error }],
	["response_error_with_result", "schema", { ...errorResponse, result: [] }],
	["response_error_missing_error", "schema", { type: "response", id: "request-1", ok: false }],
	["response_ok_string", "schema", { ...okResponse, ok: "true" }],
	["response_empty_id", "schema", { ...okResponse, id: "" }],
	["service_update_missing_update", "schema", { type: "service_update", subscriptionId: "subscription-1" }],
	["service_update_empty_subscription", "schema", { type: "service_update", subscriptionId: "", update: null }],
	["attachment_missing", "schema", { type: "attachment" }],
	["attachment_server_target", "schema", { type: "attachment", attachment: serverTarget }],
	["null_message", "schema", null],
	["array_message", "schema", []],
];

function rejectAll(cases: Array<[string, string, unknown]>, parse: (v: unknown) => unknown) {
	return cases.map(([name, source, value]) => {
		let error: string;
		try {
			parse(value);
			throw new Error(`${name}: upstream accepts a value listed as a reject`);
		} catch (caught) {
			error = (caught as Error).message;
			if (error.startsWith(`${name}:`)) throw caught;
		}
		const out: Record<string, unknown> = { name, source };
		if (isJsonValue(value)) out.value = value;
		let bytes: Uint8Array;
		try {
			bytes = encodeCbor(value);
		} catch (caught) {
			out.noWire = `upstream's encoder refuses the value: ${(caught as Error).message}`;
			out.error = error;
			return out;
		}
		try {
			parse(decodeCbor(bytes));
			out.noWire = "upstream's encoder drops what made the value invalid, so its bytes decode to a valid message";
		} catch {
			out.frame = hex(encodeFrame(bytes));
		}
		out.error = error;
		return out;
	});
}

console.log(JSON.stringify({
	protocolVersion: PROTOCOL_VERSION,
	client: encodeAll(clientMessages, encodeClientMessage),
	server: encodeAll(serverMessages, encodeServerMessage),
	clientRejects: rejectAll(clientRejects, parseClientMessage),
	serverRejects: rejectAll(serverRejects, parseServerMessage),
}, null, "\t"));
