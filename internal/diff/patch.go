package diff

import (
	"strconv"
	"strings"
)

// Hunk is one hunk of a unified patch; Lines carry their " ", "+" or "-"
// prefix and no newline.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Lines              []string
}

// StructuredPatch is jsdiff's structuredPatch without headers: the hunks
// between two texts with context lines around each change.
func StructuredPatch(oldText, newText string, context int) []Hunk {
	type run struct {
		Change
		lines []string
	}
	changes := DiffLines(oldText, newText)
	runs := make([]run, 0, len(changes)+1)
	for _, c := range changes {
		runs = append(runs, run{Change: c, lines: splitLines(c.Value)})
	}
	// An empty value at the end makes closing the last hunk uniform.
	runs = append(runs, run{lines: []string{}})

	contextLines := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, line := range lines {
			out[i] = " " + line
		}
		return out
	}
	var hunks []Hunk
	oldRangeStart, newRangeStart := 0, 0
	var curRange []string
	oldLine, newLine := 1, 1
	for i, current := range runs {
		lines := current.lines
		if current.Added || current.Removed {
			// Start with the previous context, if there is some.
			if oldRangeStart == 0 {
				oldRangeStart, newRangeStart = oldLine, newLine
				if i > 0 {
					curRange = nil
					if context > 0 {
						prev := runs[i-1].lines
						curRange = contextLines(prev[max(0, len(prev)-context):])
					}
					oldRangeStart -= len(curRange)
					newRangeStart -= len(curRange)
				}
			}
			prefix := "-"
			if current.Added {
				prefix = "+"
			}
			for _, line := range lines {
				curRange = append(curRange, prefix+line)
			}
			if current.Added {
				newLine += len(lines)
			} else {
				oldLine += len(lines)
			}
			continue
		}
		if oldRangeStart != 0 {
			if len(lines) <= context*2 && i < len(runs)-2 {
				// Overlapping: the context joins the next change.
				curRange = append(curRange, contextLines(lines)...)
			} else {
				contextSize := min(len(lines), context)
				curRange = append(curRange, contextLines(lines[:contextSize])...)
				hunks = append(hunks, Hunk{
					OldStart: oldRangeStart, OldLines: oldLine - oldRangeStart + contextSize,
					NewStart: newRangeStart, NewLines: newLine - newRangeStart + contextSize,
					Lines: curRange,
				})
				oldRangeStart, newRangeStart, curRange = 0, 0, nil
			}
		}
		oldLine += len(lines)
		newLine += len(lines)
	}

	// Drop each line's newline; a line without one is followed by the
	// no-newline marker.
	for h := range hunks {
		var lines []string
		for _, line := range hunks[h].Lines {
			if strings.HasSuffix(line, "\n") {
				lines = append(lines, line[:len(line)-1])
			} else {
				lines = append(lines, line, `\ No newline at end of file`)
			}
		}
		hunks[h].Lines = lines
	}
	return hunks
}

// CreateTwoFilesPatch is jsdiff's createTwoFilesPatch with no headers and
// FILE_HEADERS_ONLY: the "---"/"+++" lines and the hunks, newline-terminated.
func CreateTwoFilesPatch(oldName, newName, oldText, newText string, context int) string {
	ret := []string{"--- " + oldName, "+++ " + newName}
	for _, hunk := range StructuredPatch(oldText, newText, context) {
		// Unified diff quirk: a hunk of size 0 starts one line lower.
		if hunk.OldLines == 0 {
			hunk.OldStart--
		}
		if hunk.NewLines == 0 {
			hunk.NewStart--
		}
		ret = append(ret, "@@ -"+strconv.Itoa(hunk.OldStart)+","+strconv.Itoa(hunk.OldLines)+
			" +"+strconv.Itoa(hunk.NewStart)+","+strconv.Itoa(hunk.NewLines)+" @@")
		ret = append(ret, hunk.Lines...)
	}
	return strings.Join(ret, "\n") + "\n"
}

// splitLines splits text into lines keeping their "\n".
func splitLines(text string) []string {
	parts := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		parts = parts[:len(parts)-1]
		for i := range parts {
			parts[i] += "\n"
		}
		return parts
	}
	for i := range parts[:len(parts)-1] {
		parts[i] += "\n"
	}
	return parts
}
