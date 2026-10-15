package codemode

import (
	_ "embed"
	"fmt"
	"unicode/utf16"
)

// preludeSource is evaluated inside the VM before the script. It is pi
// v1.2.0's PRELUDE_SOURCE (packages/codemode/src/runtime/prelude-source.ts)
// with its constants substituted, byte for byte: TestPreludeIsPis pins its
// hash, which is also part of the deterministic fingerprint, so a replayed
// pi run and a run here agree on what the script observes.
//
// It evaluates to `(bridge, toolsJson, globalsJson, storeJson) => { settle,
// run, stalled }`; see the source for the bridge protocol.
//
//go:embed prelude.js
var preludeSource string

// preludeHash is FNV-1a over the prelude's UTF-16 code units, as 8 hex digits,
// as pi computes it for the fingerprint.
var preludeHash = fnv1a(preludeSource)

func fnv1a(text string) string {
	hash := uint32(0x811c9dc5)
	for _, unit := range utf16.Encode([]rune(text)) {
		hash ^= uint32(unit)
		hash *= 0x01000193
	}
	return fmt.Sprintf("%08x", hash)
}
