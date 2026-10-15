// Emits FrameDecoder behaviour vectors from the real upstream implementation.
//
// NOTE: upstream deleted assertCompleteFrame in the 2026-09-03 rewrite, so this
// script no longer emits the "assertCases" section. That section in
// upstream_framing.json is FROZEN at 96317e50b (the last sha that had the
// function) and must be preserved when regenerating — the Go AssertCompleteFrame
// is still live and TestAssertCompleteFrameMatchesUpstream still pins it.
// Regenerate with: node gen-framing.ts > out.json, then merge, keeping
// assertCases. The test fails loudly if the section goes missing.
import { encodeFrame, FrameDecoder } from "./framing.ts";

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");
const bytes = (h: string) => Uint8Array.from(Buffer.from(h, "hex"));

// A stream of three frames: "a", empty, "bcd".
const stream = Buffer.concat([
	encodeFrame(new Uint8Array([0x61])),
	encodeFrame(new Uint8Array()),
	encodeFrame(new Uint8Array([0x62, 0x63, 0x64])),
]);

// Push the same stream split at every possible chunk boundary; the frame
// sequence must be identical regardless of how the bytes arrive.
const splits: Array<{ at: number; frames: string[]; error?: string }> = [];
for (let at = 0; at <= stream.byteLength; at++) {
	const decoder = new FrameDecoder();
	try {
		const frames: string[] = [];
		for (const chunk of [stream.subarray(0, at), stream.subarray(at)]) {
			if (chunk.byteLength === 0) continue;
			for (const frame of decoder.push(Uint8Array.from(chunk))) frames.push(hex(frame));
		}
		decoder.end();
		splits.push({ at, frames });
	} catch (error) {
		splits.push({ at, frames: [], error: (error as Error).message });
	}
}

// Byte-at-a-time delivery.
const dripDecoder = new FrameDecoder();
const drip: string[] = [];
for (const byte of stream) {
	for (const frame of dripDecoder.push(Uint8Array.from([byte]))) drip.push(hex(frame));
}
dripDecoder.end();

function decoderCase(name: string, chunks: string[], opts?: { maxFrameLength?: number }) {
	const decoder = new FrameDecoder(opts);
	const frames: string[] = [];
	try {
		for (const chunk of chunks) {
			for (const frame of decoder.push(bytes(chunk))) frames.push(hex(frame));
		}
		decoder.end();
		return { name, frames, ended: true };
	} catch (error) {
		return { name, frames, error: (error as Error).message };
	}
}

const cases = [
	decoderCase("truncated_header", ["0000"]),
	decoderCase("truncated_payload", ["0000000461"]),
	decoderCase("over_limit", ["00000010"], { maxFrameLength: 8 }),
	decoderCase("at_limit", ["000000080011223344556677"], { maxFrameLength: 8 }),
	decoderCase("zero_length_frames", ["000000000000000000000000"]),
	decoderCase("two_frames_one_chunk", ["0000000161000000026263"]),
	decoderCase("push_after_failure", ["00000010", "61"], { maxFrameLength: 8 }),
];

console.log(JSON.stringify({ stream: hex(stream), splits, drip, cases }, null, "\t"));
