package pgrwire

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// MaxFuzzyTables is maxDbTablesForFuzzySearch (SchemaCache.hs:148): a schema
// of this many tables or more has no fuzzy index, so no hint.
const MaxFuzzyTables = 500

// TableMinScore is the least similarity getFuzzyHint accepts for a table or a
// function name (Error.hs, getFuzzyHint).
const TableMinScore = 0.75

// SuggestTable is tableNotFoundHint's choice (Error.hs): the table of names
// closest to name with a score of at least TableMinScore, or "" when none is
// that close or the schema is too large to have an index.
func SuggestTable(names []string, name string) string {
	if len(names) == 0 || len(names) >= MaxFuzzyTables {
		return ""
	}
	return Suggest(names, name, TableMinScore)
}

type gramCounts map[string]int

// RelationshipMinScore is the score Fuzzy.getOne gives a relationship, a
// parameter list, or a function's parameters (Error.hs): fuzzyset's default.
const RelationshipMinScore = 0.33

// Suggest is Data.FuzzySet.getOneWithMinScore of fuzzyset 0.2.4, the version
// PostgREST 14 builds with (stack.yaml): gram sizes 3 then 2, cosine
// similarity over the grams, the best 50 re-scored by normalised Levenshtein
// distance, and the first size that has a match at or above minScore wins.
// An exact match, in any case, is the answer. "" is no match.
func Suggest(names []string, value string, minScore float64) string {
	exact := map[string]string{}
	for _, name := range names {
		key := strings.ToLower(name)
		if _, seen := exact[key]; !seen {
			exact[key] = name
		}
	}
	key := strings.ToLower(value)
	if match, ok := exact[key]; ok {
		return match
	}
	type scored struct {
		name   string
		cosine float64
		score  float64
	}
	for _, size := range []int{3, 2} {
		query := gramVector(key, size)
		queryMagnitude := magnitude(query)
		var candidates []scored
		for lowered, original := range exact {
			item := gramVector(lowered, size)
			dot := 0
			for gram, count := range query {
				dot += count * item[gram]
			}
			if dot == 0 {
				continue
			}
			candidates = append(candidates, scored{
				name:   original,
				cosine: float64(dot) / (queryMagnitude * magnitude(item)),
			})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].cosine != candidates[j].cosine {
				return candidates[i].cosine > candidates[j].cosine
			}
			return candidates[i].name < candidates[j].name
		})
		if len(candidates) > 50 {
			candidates = candidates[:50]
		}
		for i := range candidates {
			candidates[i].score = levenshteinNorm(key, strings.ToLower(candidates[i].name))
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
		if len(candidates) > 0 && candidates[0].score >= minScore {
			return candidates[0].name
		}
	}
	return ""
}

// gramVector is Data.FuzzySet.Internal.gramVector: the counts of the
// size-grams of the value, normalised and enclosed in dashes.
func gramVector(value string, size int) gramCounts {
	counts := gramCounts{}
	text := []rune("-" + normalised(value) + "-")
	for offset := 0; offset+size <= len(text); offset++ {
		counts[string(text[offset:offset+size])]++
	}
	return counts
}

// normalised is Data.FuzzySet.Util.normalized: lower case, and only letters,
// digits, spaces and commas kept.
func normalised(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || r == ',' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func magnitude(counts gramCounts) float64 {
	sum := 0
	for _, count := range counts {
		sum += count * count
	}
	return math.Sqrt(float64(sum))
}

// levenshteinNorm is Data.Text.Metrics.levenshteinNorm: 1 minus the edit
// distance over the length of the longer text, and 1 for two empty texts.
func levenshteinNorm(a, b string) float64 {
	left, right := []rune(a), []rune(b)
	longest := len(left)
	if len(right) > longest {
		longest = len(right)
	}
	if longest == 0 {
		return 1
	}
	previous := make([]int, len(right)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(left); i++ {
		current := make([]int, len(right)+1)
		current[0] = i
		for j := 1; j <= len(right); j++ {
			cost := 1
			if left[i-1] == right[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return 1 - float64(previous[len(right)])/float64(longest)
}
