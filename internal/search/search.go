// Package search is the BM25 tool search of pi's coding agent and of
// pi-durable's copy of it (durable/src/search.ts): tokens split at camelCase
// boundaries and non-alphanumerics, without stop words and naively singular,
// ranked by Okapi BM25. The coding tool_search and codemode, and the durable
// tool_search, share it.
package search

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Document is a tool as the ranker sees it: its name and the text built by
// NewDocument.
type Document struct {
	Name string
	Text string
}

// Match is a ranked tool.
type Match struct {
	Name  string
	Score float64
}

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
	"by": true, "for": true, "from": true, "in": true, "is": true, "it": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "this": true, "to": true, "with": true,
}

var (
	camelLower  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	camelUpper  = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	nonAlnum    = regexp.MustCompile(`[^a-z0-9]+`)
	sibilantEnd = regexp.MustCompile(`(ches|shes|sses|xes|zes)$`)
)

// stem is a naive singular form, so issues matches issue and searches matches
// search.
func stem(term string) string {
	switch {
	case len(term) > 4 && strings.HasSuffix(term, "ies"):
		return term[:len(term)-3] + "y"
	case len(term) > 4 && sibilantEnd.MatchString(term):
		return term[:len(term)-2]
	case len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss"):
		return term[:len(term)-1]
	}
	return term
}

// Tokenize splits text into lowercase terms at camelCase boundaries and
// non-alphanumerics and drops stop words. Only ASCII letters and digits make
// terms, as in pi: text in other scripts, Cyrillic included, contributes
// nothing.
func Tokenize(text string) []string {
	text = camelLower.ReplaceAllString(text, "$1 $2")
	text = camelUpper.ReplaceAllString(text, "$1 $2")
	var terms []string
	for _, term := range nonAlnum.Split(jstext.ToLower(text), -1) {
		if term != "" && !stopWords[term] {
			terms = append(terms, stem(term))
		}
	}
	return terms
}

// SchemaText appends schema descriptions and property names, recursively.
func SchemaText(schema *ai.Schema, parts []string) []string {
	if schema == nil {
		return parts
	}
	if schema.Description != "" {
		parts = append(parts, schema.Description)
	}
	for _, name := range schema.OrderedProperties() {
		parts = append(parts, name)
		parts = SchemaText(schema.Properties[name], parts)
	}
	parts = SchemaText(schema.Items, parts)
	for _, variants := range [][]*ai.Schema{schema.AnyOf, schema.OneOf, schema.AllOf} {
		for _, variant := range variants {
			parts = SchemaText(variant, parts)
		}
	}
	return parts
}

// Ranker is Okapi BM25 (pi Bm25Ranker). The zero value has k1 1.2 and b 0.75.
type Ranker struct {
	K1, B float64
	// NamesFirst ranks a matching tool first when the query names it: its
	// full name, or the part after a "__" separator, so thread_read and
	// app__thread_read both name mcp__app__thread_read; case and characters
	// that are not identifier characters do not count (pi ae823bc0, #11331).
	// Length normalization could otherwise rank a short description sharing
	// the name's words above it. pi's coding agent ranks so (tool_search,
	// codemode searchTools), its durable search does not.
	NamesFirst bool
}

// normalizeToolName is pi's normalizeName: trimmed, lowercased, and every
// UTF-16 code unit outside [a-z0-9_$] replaced by "_", so dev-radius matches
// dev_radius.
func normalizeToolName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(jstext.Trim(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '$':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("__")
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// isNameQuery is pi's: whether the normalized query names the tool.
func isNameQuery(query, name string) bool {
	normalized := normalizeToolName(name)
	return normalized == query || strings.HasSuffix(normalized, "__"+query)
}

// Rank returns the documents matching query, best first, at most limit. Ties
// keep document order.
func (r Ranker) Rank(query string, documents []Document, limit int) []Match {
	k1, b := r.K1, r.B
	if k1 == 0 {
		k1 = 1.2
	}
	if b == 0 {
		b = 0.75
	}
	var queryTerms []string
	for _, term := range Tokenize(query) {
		if !slices.Contains(queryTerms, term) {
			queryTerms = append(queryTerms, term)
		}
	}
	if len(queryTerms) == 0 || len(documents) == 0 || limit <= 0 {
		return []Match{}
	}
	termCounts := make([]map[string]int, len(documents))
	lengths := make([]float64, len(documents))
	total := 0.0
	for i, document := range documents {
		counts := map[string]int{}
		for _, term := range Tokenize(document.Text) {
			counts[term]++
		}
		termCounts[i] = counts
		for _, count := range counts {
			lengths[i] += float64(count)
		}
		total += lengths[i]
	}
	averageLength := total / float64(len(documents))
	if averageLength == 0 {
		averageLength = 1
	}
	idf := map[string]float64{}
	for _, term := range queryTerms {
		frequency := 0
		for _, counts := range termCounts {
			if counts[term] > 0 {
				frequency++
			}
		}
		idf[term] = math.Log(1 + (float64(len(documents)-frequency)+0.5)/(float64(frequency)+0.5))
	}
	type ranked struct {
		Match
		named bool
	}
	matches := []ranked{}
	nameQuery := normalizeToolName(query)
	for i, document := range documents {
		score := 0.0
		for _, term := range queryTerms {
			count := float64(termCounts[i][term])
			if count == 0 {
				continue
			}
			norm := k1 * (1 - b + (b*lengths[i])/averageLength)
			score += idf[term] * ((count * (k1 + 1)) / (count + norm))
		}
		if score > 0 {
			matches = append(matches, ranked{Match{Name: document.Name, Score: score}, r.NamesFirst && isNameQuery(nameQuery, document.Name)})
		}
	}
	slices.SortStableFunc(matches, func(x, y ranked) int {
		if x.named != y.named {
			if x.named {
				return -1
			}
			return 1
		}
		return cmp.Compare(y.Score, x.Score)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]Match, len(matches))
	for i, m := range matches {
		out[i] = m.Match
	}
	return out
}
