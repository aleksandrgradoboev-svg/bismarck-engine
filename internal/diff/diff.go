// Package diff is the part of jsdiff 8.0.4 (https://github.com/kpdecker/jsdiff,
// BSD-3-Clause, see LICENSE-jsdiff) that pi's edit tool calls: diffLines and
// createTwoFilesPatch with file headers only. It ports jsdiff's variant of
// Myers's algorithm, with its diagonal pruning and component merging, so the
// changes and patches equal jsdiff's byte for byte; options pi does not pass
// (callbacks, timeouts, maxEditLength, ignore*) are not ported.
package diff

import "math"

// Change is one run of lines (jsdiff's change object): common, added, or
// removed.
type Change struct {
	Count   int
	Added   bool
	Removed bool
	Value   string
}

// component is a change under construction, linked to the one before it.
type component struct {
	count          int
	added, removed bool
	previous       *component
}

type path struct {
	oldPos int
	last   *component
}

// DiffLines compares two texts line by line, a line keeping its newline
// ("\n" or "\r\n").
func DiffLines(oldText, newText string) []Change {
	return diffTokens(tokenizeLines(oldText), tokenizeLines(newText))
}

// tokenizeLines splits value after each "\n" or "\r\n", as jsdiff's line
// tokenizer does; no token is empty.
func tokenizeLines(value string) []string {
	var tokens []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '\n' {
			tokens = append(tokens, value[start:i+1])
			start = i + 1
		}
	}
	if start < len(value) {
		tokens = append(tokens, value[start:])
	}
	return tokens
}

func diffTokens(oldTokens, newTokens []string) []Change {
	newLen, oldLen := len(newTokens), len(oldTokens)
	maxEditLength := newLen + oldLen
	// bestPath is indexed by diagonal; offset maps diagonals -maxEditLength-1
	// ..maxEditLength+1 onto the slice.
	offset := maxEditLength + 1
	bestPath := make([]*path, 2*offset+1)
	at := func(diagonal int) **path { return &bestPath[diagonal+offset] }

	*at(0) = &path{oldPos: -1}
	newPos := extractCommon(*at(0), newTokens, oldTokens, 0)
	if (*at(0)).oldPos+1 >= oldLen && newPos+1 >= newLen {
		return buildValues((*at(0)).last, newTokens, oldTokens)
	}

	// Once the right edge of the edit graph is hit on some diagonal k, the end
	// is reachable in no more than k edits, so diagonals beyond are not
	// considered; likewise below once the bottom is hit.
	minDiagonal, maxDiagonal := math.MinInt, math.MaxInt
	for editLength := 1; editLength <= maxEditLength; editLength++ {
		for diagonal := max(minDiagonal, -editLength); diagonal <= min(maxDiagonal, editLength); diagonal += 2 {
			removePath, addPath := *at(diagonal - 1), *at(diagonal + 1)
			if removePath != nil {
				// No one else is going to use this value.
				*at(diagonal - 1) = nil
			}
			canAdd := false
			if addPath != nil {
				addPathNewPos := addPath.oldPos - diagonal
				canAdd = 0 <= addPathNewPos && addPathNewPos < newLen
			}
			canRemove := removePath != nil && removePath.oldPos+1 < oldLen
			if !canAdd && !canRemove {
				*at(diagonal) = nil
				continue
			}
			// Branch from the prior path farthest from the origin in the old
			// text that stays within the graph.
			var basePath *path
			if !canRemove || (canAdd && removePath.oldPos < addPath.oldPos) {
				basePath = addToPath(addPath, true, false, 0)
			} else {
				basePath = addToPath(removePath, false, true, 1)
			}
			newPos = extractCommon(basePath, newTokens, oldTokens, diagonal)
			if basePath.oldPos+1 >= oldLen && newPos+1 >= newLen {
				return buildValues(basePath.last, newTokens, oldTokens)
			}
			*at(diagonal) = basePath
			if basePath.oldPos+1 >= oldLen {
				maxDiagonal = min(maxDiagonal, diagonal-1)
			}
			if newPos+1 >= newLen {
				minDiagonal = max(minDiagonal, diagonal+1)
			}
		}
	}
	return nil // unreachable: maxEditLength edits always reach the end
}

func addToPath(p *path, added, removed bool, oldPosInc int) *path {
	last := p.last
	if last != nil && last.added == added && last.removed == removed {
		return &path{oldPos: p.oldPos + oldPosInc, last: &component{count: last.count + 1, added: added, removed: removed, previous: last.previous}}
	}
	return &path{oldPos: p.oldPos + oldPosInc, last: &component{count: 1, added: added, removed: removed, previous: last}}
}

func extractCommon(basePath *path, newTokens, oldTokens []string, diagonal int) int {
	newLen, oldLen := len(newTokens), len(oldTokens)
	oldPos := basePath.oldPos
	newPos := oldPos - diagonal
	common := 0
	for newPos+1 < newLen && oldPos+1 < oldLen && oldTokens[oldPos+1] == newTokens[newPos+1] {
		newPos++
		oldPos++
		common++
	}
	if common > 0 {
		basePath.last = &component{count: common, previous: basePath.last}
	}
	basePath.oldPos = oldPos
	return newPos
}

func buildValues(last *component, newTokens, oldTokens []string) []Change {
	var components []*component
	for c := last; c != nil; c = c.previous {
		components = append(components, c)
	}
	changes := make([]Change, len(components))
	newPos, oldPos := 0, 0
	for i := range components {
		c := components[len(components)-1-i]
		change := Change{Count: c.count, Added: c.added, Removed: c.removed}
		if !c.removed {
			change.Value = join(newTokens[newPos : newPos+c.count])
			newPos += c.count
			if !c.added {
				oldPos += c.count
			}
		} else {
			change.Value = join(oldTokens[oldPos : oldPos+c.count])
			oldPos += c.count
		}
		changes[i] = change
	}
	return changes
}

func join(tokens []string) string {
	n := 0
	for _, t := range tokens {
		n += len(t)
	}
	b := make([]byte, 0, n)
	for _, t := range tokens {
		b = append(b, t...)
	}
	return string(b)
}
