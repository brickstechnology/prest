package controllers

// miniship (miniship-cloud#1305): the parts of a read that embeds another table
// which need no Postgres: how a select is read, how the relationship is found
// among the foreign keys, and how it is refused.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPgrParseSelectList_readsAliasesCastsPathsAndEmbeds(t *testing.T) {
	items, err := pgrParseSelectList("id,name:title,pages::text,kind:meta->>kind,n::numeric(4,2),posts!author_id!inner(id,tags:tag(*))", 0)
	require.NoError(t, err)
	require.Len(t, items, 6)

	require.Equal(t, `"t"."id"`, items[0].field.sql("t"))
	require.Equal(t, `"t"."title" AS "name"`, items[1].field.sql("t"))
	require.Equal(t, `CAST("t"."pages" AS text) AS "pages"`, items[2].field.sql("t"))
	require.Equal(t, `("t"."meta"->>'kind') AS "kind"`, items[3].field.sql("t"))
	require.Equal(t, `CAST("t"."n" AS numeric(4,2)) AS "n"`, items[4].field.sql("t"))

	embed := items[5].embed
	require.Equal(t, "posts", embed.name)
	require.Equal(t, "author_id", embed.hint)
	require.True(t, embed.inner)
	require.Len(t, embed.items, 2)
	require.Equal(t, "tags", embed.items[1].embed.label())
	require.Equal(t, "tag", embed.items[1].embed.name)
}

func TestPgrParseSelectList_refusesWhatItDoesNotAnswer(t *testing.T) {
	for _, raw := range []string{
		"...posts(id)",           // a spread
		"id,",                    // an empty item
		"posts(id",               // an unclosed embed
		"a:b:c",                  // two aliases
		"id::text; DROP TABLE x", // a cast is a type
		"meta->>a->b",            // nothing follows ->>
		"posts!a!b(id)",          // two hints
		"a(b(c(d(e(f(id))))))",   // deeper than the ceiling
		"*:everything",           // the star takes no alias
	} {
		_, err := pgrParseSelectList(raw, 0)
		require.Error(t, err, raw)
		var refused *pgrError
		require.ErrorAs(t, err, &refused, raw)
		require.Equal(t, "PGRST100", refused.code, raw)
	}
}

func TestPgrParseRead_addressesAnEmbeddedTablesFilters(t *testing.T) {
	query := url.Values{
		"select":           {"id,tags:tag(id,posts(id))"},
		"tags.name":        {"eq.go"},
		"tags.order":       {"id.desc"},
		"tags.limit":       {"2"},
		"tags.posts.id":    {"gt.3"},
		"views":            {"gt.1"},
		"tags.or":          {"(name.eq.a,name.eq.b)"},
		"tags.offset":      {"1"},
		"not.or":           {"(id.eq.9)"},
		"tags.not.and":     {"(name.eq.a)"},
		"tags.meta->>kind": {"eq.x"},
	}
	read, err := pgrParseRead(query, 10)
	require.NoError(t, err)

	tags := read.items[1].embed
	require.Equal(t, "id.desc", tags.order)
	require.Equal(t, 2, tags.limit)
	require.Equal(t, 1, tags.offset)
	require.Len(t, tags.keys, 4)
	require.Len(t, tags.items[1].embed.keys, 1)
	// The top read keeps its own two filters, and nothing of the embedded ones.
	require.Len(t, read.where, 2)
	require.Equal(t, len(read.args), read.whereArgs)
}

func TestPgrParseRead_refusesABadEmbeddedFilterBeforeAnySQL(t *testing.T) {
	for key, value := range map[string]string{
		"tags.name":    "regex.x",
		"tags.limit":   "many",
		"tags.limit ":  "1",
		"tags.order":   "name.sideways",
		"tags.name;--": "eq.1",
	} {
		_, err := pgrParseRead(url.Values{"select": {"tags:tag(id)"}, key: {value}}, 10)
		require.Error(t, err, key)
	}
	_, err := pgrParseRead(url.Values{"select": {"tag(id)"}, "tag.limit": {"11"}}, 10)
	var refused *pgrError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "MS100", refused.code)
}

// A library: books by author and editor, chapters of a book, tags through a
// join table, and authors who have a mentor.
func libraryKeys() []pgrForeignKey {
	return []pgrForeignKey{
		{table: "books", foreign: "authors", cons: "books_author_id_fkey", cols: []string{"author_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
		{table: "books", foreign: "authors", cons: "books_editor_id_fkey", cols: []string{"editor_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
		{table: "chapters", foreign: "books", cons: "chapters_book_id_fkey", cols: []string{"book_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
		{table: "covers", foreign: "books", cons: "covers_book_id_fkey", cols: []string{"book_id"}, foreignCols: []string{"id"}, oneToOne: true, primaryKey: []string{"book_id"}},
		{table: "book_tags", foreign: "books", cons: "book_tags_book_id_fkey", cols: []string{"book_id"}, foreignCols: []string{"id"}, primaryKey: []string{"book_id", "tag_id"}},
		{table: "book_tags", foreign: "tags", cons: "book_tags_tag_id_fkey", cols: []string{"tag_id"}, foreignCols: []string{"id"}, primaryKey: []string{"book_id", "tag_id"}},
		{table: "authors", foreign: "authors", cons: "authors_mentor_id_fkey", cols: []string{"mentor_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
		// A table with two keys whose columns are not its primary key is not a
		// join table.
		{table: "reviews", foreign: "books", cons: "reviews_book_id_fkey", cols: []string{"book_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
		{table: "reviews", foreign: "authors", cons: "reviews_author_id_fkey", cols: []string{"author_id"}, foreignCols: []string{"id"}, primaryKey: []string{"id"}},
	}
}

func TestPgrRelations_findsTheKindsPostgRESTFinds(t *testing.T) {
	all := pgrBuildRelations(libraryKeys())
	find := func(origin, target, hint string) *pgrRelation {
		t.Helper()
		rel, err := all.find(origin, target, hint)
		require.NoError(t, err, "%s -> %s!%s", origin, target, hint)
		return rel
	}

	// Many to one by the table, the constraint, the column, and as the hint.
	require.Equal(t, pgrManyToOne, find("books", "authors", "books_author_id_fkey").cardinality)
	require.Equal(t, pgrManyToOne, find("books", "authors", "editor_id").cardinality)
	require.Equal(t, pgrManyToOne, find("books", "author_id", "").cardinality)
	require.Equal(t, pgrManyToOne, find("books", "books_editor_id_fkey", "").cardinality)
	require.Equal(t, "editor_id", find("books", "authors", "books_editor_id_fkey").cols[0][0])

	// One to many, one to one, many to many.
	require.Equal(t, pgrOneToMany, find("books", "chapters", "").cardinality)
	require.Equal(t, pgrOneToOne, find("books", "covers", "").cardinality)
	require.True(t, find("covers", "books", "").toOne())
	require.True(t, find("books", "covers", "").toOne())
	require.False(t, find("books", "chapters", "").toOne())
	many := find("books", "tags", "")
	require.Equal(t, pgrManyToMany, many.cardinality)
	require.Equal(t, "book_tags", many.junction.table)
	require.Equal(t, [][2]string{{"id", "book_id"}}, many.junction.source)
	require.Equal(t, [][2]string{{"id", "tag_id"}}, many.junction.target)
	require.Equal(t, pgrManyToMany, find("tags", "books", "").cardinality)
	require.Same(t, many, find("books", "tags", "book_tags"))

	// A table pointing at itself: its own name is the children, the key's
	// column the parent, and the column as a hint the children again.
	require.Equal(t, pgrOneToMany, find("authors", "authors", "").cardinality)
	require.Equal(t, pgrManyToOne, find("authors", "mentor_id", "").cardinality)
	require.Equal(t, pgrOneToMany, find("authors", "authors", "mentor_id").cardinality)
}

func TestPgrRelations_refusesAsPostgRESTRefuses(t *testing.T) {
	all := pgrBuildRelations(libraryKeys())

	// Two keys to one table.
	_, err := all.find("books", "authors", "")
	var ambiguous *pgrError
	require.ErrorAs(t, err, &ambiguous)
	require.Equal(t, 300, ambiguous.status)
	require.Equal(t, "PGRST201", ambiguous.code)
	require.Contains(t, ambiguous.hint, "'authors!books_author_id_fkey', 'authors!books_editor_id_fkey'")
	require.Len(t, ambiguous.detailsList, 2)

	// A table with two keys that are not a primary key is not a join table: the
	// books and the authors it reviews are not related through it.
	_, err = all.find("books", "reviews", "")
	require.NoError(t, err)
	_, err = all.find("tags", "reviews", "")
	require.Error(t, err)

	// A name that is nothing, with and without a near miss.
	for _, c := range []struct{ origin, target, hint, suggests string }{
		{"books", "chapter", "", "Perhaps you meant 'chapters' instead of 'chapter'."},
		{"bookz", "chapters", "", "Perhaps you meant 'books' instead of 'bookz'."},
		{"books", "zebra", "", ""},
		{"books", "chapters", "nope", ""},
	} {
		_, err := all.find(c.origin, c.target, c.hint)
		var missing *pgrError
		require.ErrorAs(t, err, &missing, c.target)
		require.Equal(t, 400, missing.status)
		require.Equal(t, "PGRST200", missing.code)
		require.Equal(t, c.suggests, missing.hint, c.target)
		if c.hint != "" {
			require.True(t, strings.Contains(missing.details, "using the hint 'nope'"))
		}
	}
}

func TestPgrRead_rendersAnEmbedAsACorrelatedSubquery(t *testing.T) {
	all := pgrBuildRelations(libraryKeys())
	query := url.Values{
		"select":            {"id,chapters:chapters(heading),tags!inner(label)"},
		"chapters.position": {"gt.1"},
		"chapters.order":    {"position.desc"},
		"tags.label":        {"eq.go"},
		"id":                {"gt.0"},
	}
	read, err := pgrParseRead(query, 50)
	require.NoError(t, err)

	next := 0
	require.NoError(t, pgrResolve(all, "books", read.items, &next))
	// What plan does after loading the relationships.
	for _, embed := range pgrEmbedsOf(read.items) {
		if embed.inner {
			predicate, err := read.embedSQL(embed, "t", true)
			require.NoError(t, err)
			read.where = append(read.where, predicate)
		}
	}
	read.whereArgs = len(read.args)
	rows, err := read.rowsSQL("books")

	require.NoError(t, err)
	require.Equal(t, `SELECT coalesce(json_agg(x), '[]'::json)::text FROM (SELECT "t"."id", `+
		`(SELECT coalesce(json_agg("x"), '[]'::json) FROM (SELECT "e1"."heading" FROM "public"."chapters" AS "e1" `+
		`WHERE "e1"."book_id" = "t"."id" AND "e1"."position" > $3 ORDER BY "e1"."position" DESC LIMIT 50 OFFSET 0) AS "x") AS "chapters", `+
		`(SELECT coalesce(json_agg("x"), '[]'::json) FROM (SELECT "e2"."label" FROM "public"."tags" AS "e2" `+
		`JOIN "public"."book_tags" AS "j2" ON "j2"."tag_id" = "e2"."id" `+
		`WHERE "j2"."book_id" = "t"."id" AND "e2"."label" = $4 LIMIT 50 OFFSET 0) AS "x") AS "tags" `+
		`FROM "public"."books" AS "t" WHERE "t"."id" > $1 AND EXISTS (SELECT 1 FROM "public"."tags" AS "e2" `+
		`JOIN "public"."book_tags" AS "j2" ON "j2"."tag_id" = "e2"."id" `+
		`WHERE "j2"."book_id" = "t"."id" AND "e2"."label" = $2) LIMIT 50 OFFSET 0) AS x`, rows)

	// The count is asked with the filters' arguments alone.
	require.Equal(t, []any{"0", "go"}, read.countArgs())
	require.Equal(t, `SELECT count(*) FROM "public"."books" AS "t" WHERE "t"."id" > $1 AND EXISTS (SELECT 1 FROM "public"."tags" AS "e2" `+
		`JOIN "public"."book_tags" AS "j2" ON "j2"."tag_id" = "e2"."id" WHERE "j2"."book_id" = "t"."id" AND "e2"."label" = $2)`,
		read.countSQL("books"))
	require.Len(t, read.args, 4)
}
