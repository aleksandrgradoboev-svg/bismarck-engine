package coding

import "github.com/aleksandrgradoboev-svg/bismarck-engine/internal/editmatch"

// The edit tool's matching lives in internal/editmatch, shared with the
// durable edit tool; these are its names in this package.

type editEntry = editmatch.Edit

func normalizeForFuzzyMatch(text string) string { return editmatch.NormalizeForFuzzyMatch(text) }

func applyEditsToNormalizedContent(normalizedContent string, edits []editEntry, path string) (base, result string, err error) {
	return editmatch.ApplyEditsToNormalizedContent(normalizedContent, edits, path)
}

func normalizeToLF(s string) string { return editmatch.NormalizeToLF(s) }

// detectLineEnding returns CRLF only if the first CRLF precedes the first bare
// LF (port of edit-diff.ts detectLineEnding).
func detectLineEnding(s string) string { return editmatch.DetectLineEnding(s) }

func restoreLineEndings(s, ending string) string { return editmatch.RestoreLineEndings(s, ending) }
