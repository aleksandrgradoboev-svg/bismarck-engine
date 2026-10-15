// Emits protocol message vectors from upstream's real codec + TypeBox schemas.
import { encodeClientMessage, encodeServerMessage, parseClientMessage, parseServerMessage } from "./codec.ts";
// The tool-call ordering vectors additionally run pi's own execution->protocol
// bridge, so their `input` is whatever toProtocolJsonValue really produces.
import { toProtocolAssistantMessage, toProtocolToolResultMessage } from "../../server/src/protocol.ts";

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");

const model = { provider: "anthropic", id: "claude-opus-5" };
const usage = {
	input: 10, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 30,
	cost: { input: 0.1, output: 0.2, cacheRead: 0, cacheWrite: 0, total: 0.3 },
};
const userItem = {
	id: "u1", role: "user",
	content: [{ type: "text", text: "hi" }],
	timestamp: 1000,
};
const assistantComplete = {
	id: "a1", role: "assistant",
	content: [{ type: "text", text: "hello" }],
	model, timestamp: 1001, status: "complete", stopReason: "stop",
};
const assistantStreaming = {
	id: "a2", role: "assistant",
	content: [{ type: "thinking", thinking: "hmm", redacted: false }],
	model, responseModel: "claude-opus-5-20260501", usage, timestamp: 1002, status: "streaming",
};
const assistantError = {
	id: "a3", role: "assistant", content: [], model, timestamp: 1003,
	status: "error", stopReason: "error", errorMessage: "boom",
};
const assistantAborted = {
	id: "a4", role: "assistant", content: [], model, timestamp: 1004,
	status: "aborted", stopReason: "aborted", errorMessage: "",
};
const toolItem = {
	id: "t1", role: "tool", toolCallId: "tc1", toolName: "bash",
	input: { args: [1, 2, null], cmd: "ls", nested: { deep: true } },
	content: [{ type: "text", text: "out" }],
	details: { exitCode: 0 }, usage, timestamp: 1005, status: "complete", isError: false,
};
const toolError = {
	id: "t2", role: "tool", toolCallId: "tc2", toolName: "bash",
	input: null, content: [{ type: "image", data: "aGk=", mimeType: "image/png" }],
	timestamp: 1006, status: "error", isError: true,
};
// Tool-call arguments in the order a model authored them. No key is in sorted
// position at any depth — top level, a nested object, and an object inside an
// array — so a frame built from these pins insertion order rather than the
// alphabetical order a Go map would otherwise impose.
const orderedArguments = {
	path: "/tmp",
	depth: 1,
	filters: [{ name: "go", enabled: true }],
	nested: { zeta: 1, alpha: 2 },
};
const orderedCall = { type: "toolCall", id: "tc3", name: "bash", arguments: orderedArguments } as const;
const assistantOrderedToolCall = toProtocolAssistantMessage(
	{
		role: "assistant",
		content: [orderedCall],
		api: "anthropic-messages",
		provider: "anthropic",
		model: "claude-opus-5",
		usage,
		stopReason: "toolUse",
		timestamp: 1007,
	} as never,
	{ id: "a5" } as never,
);
const toolOrderedInput = toProtocolToolResultMessage(
	{
		role: "toolResult",
		content: [{ type: "text", text: "out" }],
		toolCallId: "tc3",
		toolName: "bash",
		isError: false,
		timestamp: 1008,
	} as never,
	{ id: "t3", call: orderedCall } as never,
);
// Upstream 6189e53b3 replaced SessionSummary with SessionMetadata and stopped
// deriving the snapshot from it, so the two no longer share a field list.
// Every optional metadata field is populated here, parentSessionId included,
// so the vectors exercise the whole shape.
const sessionMetadata = {
	id: "s1", createdAt: 1, updatedAt: 2,
	parentSessionId: "s0", sessionName: "work", cwd: "/tmp",
};
const sessionSnapshot = {
	id: "s1", name: "work", cwd: "/tmp", createdAt: 1, updatedAt: 2,
	phase: "idle", model, thinkingLevel: "medium", attached: true, locked: false,
	revision: 7,
	transcript: [userItem, assistantComplete, toolItem],
	queuedSteer: [userItem], queuedSteerCount: 1,
};
const modelMetadata = {
	provider: "anthropic", id: "claude-opus-5", name: "Claude Opus 5", api: "anthropic-messages",
	reasoning: true, input: ["text", "image"], contextWindow: 200000, maxTokens: 64000,
	cost: { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
	supportedThinkingLevels: ["off", "low", "high"], authenticated: true,
};
const serverSnapshot = {
	serverId: "srv1", protocolVersion: 1, revision: 3,
	sessions: [sessionMetadata], models: [modelMetadata],
};

const clientMessages: Array<[string, unknown]> = [
	["hello", { type: "hello", version: 1 }],
	["hello_version_zero", { type: "hello", version: 0 }],
	["req_list", { type: "request", id: "r1", request: { command: "list" } }],
	["req_create_empty", { type: "request", id: "r2", request: { command: "create" } }],
	["req_create_full", {
		type: "request", id: "r3",
		request: { command: "create", cwd: "/tmp", name: "n", model, thinkingLevel: "high" },
	}],
	["req_attach", { type: "request", id: "r4", request: { command: "attach", sessionId: "s1" } }],
	["req_detach", { type: "request", id: "r5", request: { command: "detach", sessionId: "s1" } }],
	["req_prompt", { type: "request", id: "r6", request: { command: "prompt", sessionId: "s1", text: "hi" } }],
	["req_steer", { type: "request", id: "r7", request: { command: "steer", sessionId: "s1", text: "no" } }],
	["req_abort", { type: "request", id: "r8", request: { command: "abort", sessionId: "s1" } }],
	["req_set_model", { type: "request", id: "r9", request: { command: "set_model", sessionId: "s1", model } }],
	["req_set_thinking", {
		type: "request", id: "r10",
		request: { command: "set_thinking", sessionId: "s1", thinkingLevel: "max" },
	}],
];

const serverMessages: Array<[string, unknown]> = [
	["hello", { type: "hello", version: 1, connectionId: "c1", snapshot: serverSnapshot }],
	["hello_error", { type: "hello_error", error: { code: "busy", message: "server busy" } }],
	["hello_error_details", {
		type: "hello_error",
		error: { code: "version", message: "nope", details: { supported: [1] } },
	}],
	["resp_list", { type: "response", id: "r1", ok: true, result: { command: "list", sessions: [sessionMetadata] } }],
	["resp_detach", { type: "response", id: "r5", ok: true, result: { command: "detach", sessionId: "s1" } }],
	["resp_create", {
		type: "response", id: "r2", ok: true,
		result: { command: "create", session: sessionSnapshot },
	}],
	["resp_prompt", {
		type: "response", id: "r6", ok: true,
		result: { command: "prompt", session: sessionSnapshot },
	}],
	["resp_error", {
		type: "response", id: "r9", ok: false,
		error: { code: "not_found", message: "no such session" },
	}],
	// The two sanitized failure codes, carrying the exact messages the server
	// substitutes for a failure's own text.
	["resp_not_implemented", {
		type: "response", id: "r9", ok: false,
		error: { code: "not_implemented", message: "Operation is not implemented" },
	}],
	["resp_internal_error", {
		type: "response", id: "r9", ok: false,
		error: { code: "internal_error", message: "Internal server error" },
	}],
	["evt_server_snapshot", { type: "event", event: { type: "server_snapshot", snapshot: serverSnapshot } }],
	["evt_session_snapshot", { type: "event", event: { type: "session_snapshot", snapshot: sessionSnapshot } }],
	["evt_removed", { type: "event", event: { type: "session_removed", sessionId: "s1" } }],
	["evt_item_started", {
		type: "event",
		event: { type: "session_progress", sessionId: "s1", progress: { type: "item_started", item: userItem } },
	}],
	["evt_delta", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "assistant_delta", messageId: "a1", contentIndex: 0, kind: "text", delta: "he" },
		},
	}],
	["evt_item_updated", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_updated", item: assistantStreaming },
		},
	}],
	["evt_item_finished_assistant", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: assistantComplete },
		},
	}],
	["evt_item_finished_error", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: assistantError },
		},
	}],
	["evt_item_finished_aborted", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: assistantAborted },
		},
	}],
	["evt_item_finished_tool_error", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: toolError },
		},
	}],
];

// Frames whose tool-call `input` carries the model's own key order. They are
// kept apart from serverMessages because the round-trip test re-encodes what it
// decoded, and Go's decoder resolves a CBOR map to an unordered map — pi's
// resolves it to an ordered JS object, so only the Node side is a fixed point
// on these. What they pin is the emitting direction, which is the one a Node
// peer observes.
const orderedToolCalls: Array<[string, unknown]> = [
	["evt_item_finished_assistant", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: assistantOrderedToolCall },
		},
	}],
	["evt_item_finished_tool", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s1",
			progress: { type: "item_finished", item: toolOrderedInput },
		},
	}],
];

function encodeAll(cases: Array<[string, unknown]>, encode: (m: any) => Uint8Array) {
	return cases.map(([name, value]) => {
		try {
			return { name, frame: hex(encode(value)) };
		} catch (error) {
			return { name, error: (error as Error).message };
		}
	});
}

// Values the parser must REJECT. Each is a mutation of a valid message.
const clientRejects: Array<[string, unknown]> = [
	["unknown_property", { type: "hello", version: 1, extra: 1 }],
	["credential_field", { type: "hello", version: 1, token: "secret" }],
	["version_string", { type: "hello", version: "1" }],
	["version_float", { type: "hello", version: 1.5 }],
	["version_negative", { type: "hello", version: -1 }],
	["unknown_type", { type: "goodbye" }],
	["unknown_command", { type: "request", id: "r", request: { command: "nope" } }],
	["nested_unknown_property", {
		type: "request", id: "r", request: { command: "attach", sessionId: "s", extra: true },
	}],
	["empty_session_id", { type: "request", id: "r", request: { command: "attach", sessionId: "" } }],
	["bad_thinking_level", {
		type: "request", id: "r", request: { command: "set_thinking", sessionId: "s", thinkingLevel: "ultra" },
	}],
	["null_message", null],
	["array_message", []],
	["string_message", "hello"],
];

const serverRejects: Array<[string, unknown]> = [
	["bad_protocol_version", {
		type: "hello", version: 2, connectionId: "c", snapshot: serverSnapshot,
	}],
	["snapshot_bad_version", {
		type: "hello", version: 1, connectionId: "c",
		snapshot: { ...serverSnapshot, protocolVersion: 2 },
	}],
	["response_ok_with_error", {
		type: "response", id: "r", ok: true, error: { code: "not_found", message: "m" },
	}],
	["response_missing_result", { type: "response", id: "r", ok: true }],
	["bad_error_code", { type: "hello_error", error: { code: "teapot", message: "m" } }],
	["auth_error_code", { type: "hello_error", error: { code: "auth", message: "Authentication failed" } }],
	["streaming_with_stop_reason", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: { type: "item_started", item: { ...assistantStreaming, stopReason: "stop" } },
		},
	}],
	["item_finished_streaming", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: { type: "item_finished", item: assistantStreaming },
		},
	}],
	["tool_running_is_error_true", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: { type: "item_started", item: { ...toolItem, status: "running", isError: true } },
		},
	}],
	["assistant_with_image_content", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: {
				type: "item_started",
				item: { ...assistantComplete, content: [{ type: "image", data: "x", mimeType: "image/png" }] },
			},
		},
	}],
	["user_with_thinking_content", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: {
				type: "item_started",
				item: { ...userItem, content: [{ type: "thinking", thinking: "x" }] },
			},
		},
	}],
	["negative_timestamp", {
		type: "event",
		event: {
			type: "session_progress", sessionId: "s",
			progress: { type: "item_started", item: { ...userItem, timestamp: -1 } },
		},
	}],
	["context_window_zero", {
		type: "hello", version: 1, connectionId: "c",
		snapshot: { ...serverSnapshot, models: [{ ...modelMetadata, contextWindow: 0 }] },
	}],
	["empty_thinking_levels", {
		type: "hello", version: 1, connectionId: "c",
		snapshot: { ...serverSnapshot, models: [{ ...modelMetadata, supportedThinkingLevels: [] }] },
	}],
	["negative_cost", {
		type: "hello", version: 1, connectionId: "c",
		snapshot: {
			...serverSnapshot,
			models: [{ ...modelMetadata, cost: { input: -1, output: 0, cacheRead: 0, cacheWrite: 0 } }],
		},
	}],
];

function parseAll(cases: Array<[string, unknown]>, parse: (v: unknown) => unknown) {
	return cases.map(([name, value]) => {
		try {
			parse(value);
			return { name, accepted: true };
		} catch (error) {
			return { name, accepted: false, error: (error as Error).message };
		}
	});
}

console.log(JSON.stringify({
	client: encodeAll(clientMessages, encodeClientMessage),
	server: encodeAll(serverMessages, encodeServerMessage),
	orderedToolCalls: encodeAll(orderedToolCalls, encodeServerMessage),
	clientRejects: parseAll(clientRejects, parseClientMessage),
	serverRejects: parseAll(serverRejects, parseServerMessage),
}, null, "\t"));
