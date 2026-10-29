package pgrwire

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSuggestTable(t *testing.T) {
	t.Parallel()
	names := []string{"countries", "orders", "order_items", "profiles", "users", "user_roles"}
	for name, want := range map[string]string{
		// One letter off, a plural dropped and a letter doubled: close.
		"countrys":  "countries",
		"order":     "orders",
		"profile":   "profiles",
		"usrs":      "users",
		"ordders":   "orders",
		"user_role": "user_roles",
		// The same name in another case is the table (the exact set is keyed
		// by the lower-cased name).
		"USERS": "users",
		// Nothing close.
		"nothing_like_it": "",
		"zzz":             "",
		"x":               "",
		"":                "",
	} {
		require.Equal(t, want, SuggestTable(names, name), "%q", name)
	}
}

func TestSuggestTable_aSchemaOfFiveHundredTablesHasNoIndex(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, MaxFuzzyTables)
	for i := 0; i < MaxFuzzyTables-1; i++ {
		names = append(names, fmt.Sprintf("zz_table_%03d", i))
	}
	names = append(names, "users")
	require.Equal(t, MaxFuzzyTables, len(names))
	require.Empty(t, SuggestTable(names, "usrs"), "500 tables is too many for PostgREST's fuzzy index")
	require.Equal(t, "users", SuggestTable(names[len(names)-250:], "usrs"))
}

func TestLevenshteinNorm(t *testing.T) {
	t.Parallel()
	require.InDelta(t, 1.0, levenshteinNorm("", ""), 0)
	require.InDelta(t, 1.0, levenshteinNorm("abc", "abc"), 0)
	require.InDelta(t, 0.0, levenshteinNorm("abc", "xyz"), 0)
	// kitten -> sitting is 3 edits of 7.
	require.InDelta(t, 1-3.0/7.0, levenshteinNorm("kitten", "sitting"), 1e-9)
}

func TestGramVector(t *testing.T) {
	t.Parallel()
	// Data.FuzzySet: the value is normalised and enclosed in dashes, so "ab"
	// is "-ab-".
	require.Equal(t, gramCounts{"-a": 1, "ab": 1, "b-": 1}, gramVector("AB", 2))
	require.Equal(t, gramCounts{"-ab": 1, "ab-": 1}, gramVector("ab", 3))
	// Punctuation other than a comma is dropped before the grams are cut.
	require.Equal(t, gramVector("ab", 2), gramVector("a_b", 2))
}
