package anonymousrole_test

// miniship (miniship-cloud#1305): the Supabase SDK's read that brings related
// rows of another table, `select=*,other(*)`, against a live Postgres. Each
// kind of relationship is held to the nested rows written out from the data;
// the rows a role may read are held to a direct SQL query asked as that role;
// the refusals are PostgREST's, which the SDK reads.
//
// The public repository's docker/tests/database-sdk.test.ts asks the same
// questions with the real SDK through the gateway.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

// The tables these subjects read, with the shape of a small library:
//
//	authors  <-- books (author_id, editor_id)        two keys to one table
//	authors  <-- authors (mentor_id)                 a table pointing at itself
//	books    <-- chapters (book_id)                  one to many
//	books    <-- covers (book_id, the primary key)   one to one
//	books    <-> tags through book_tags              many to many
//	books    <-- secrets (book_id)                   no grant for the reader
const (
	authors  = "rest_emb_authors"
	books    = "rest_emb_books"
	chapters = "rest_emb_chapters"
	covers   = "rest_emb_covers"
	tags     = "rest_emb_tags"
	bookTags = "rest_emb_book_tags"
	secrets  = "rest_emb_secrets"
)

func stageEmbed(t *testing.T, db *sql.DB) {
	t.Helper()
	stagePostgREST(t, db)
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s, public.%s, public.%s, public.%s, public.%s, public.%s, public.%s CASCADE;`,
		secrets, bookTags, tags, covers, chapters, books, authors)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (id int PRIMARY KEY, name text NOT NULL, mentor_id int REFERENCES public.%[1]s (id));
		INSERT INTO public.%[1]s VALUES (1, 'Ada', NULL), (2, 'Grace', 1), (3, 'Edsger', 1);

		CREATE TABLE public.%[2]s (
			id int PRIMARY KEY, title text NOT NULL, pages int NOT NULL, meta jsonb,
			author_id int REFERENCES public.%[1]s (id),
			editor_id int REFERENCES public.%[1]s (id));
		INSERT INTO public.%[2]s VALUES
			(1, 'Bundles', 300, '{"kind": "novel", "pub": {"year": 2021}}', 1, 2),
			(2, 'Notes', 120, '{"kind": "essay", "pub": {"year": 2019}}', 2, 1),
			(3, 'Orphan', 80, NULL, NULL, NULL);

		CREATE TABLE public.%[3]s (
			id int PRIMARY KEY, book_id int NOT NULL REFERENCES public.%[2]s (id),
			heading text NOT NULL, position int NOT NULL, published boolean NOT NULL, owner text);
		INSERT INTO public.%[3]s VALUES
			(1, 1, 'Intro', 1, true, '%[8]s'),
			(2, 1, 'Middle', 2, true, '%[9]s'),
			(3, 1, 'Draft', 3, false, '%[8]s'),
			(4, 2, 'One', 1, true, '%[9]s');

		CREATE TABLE public.%[4]s (book_id int PRIMARY KEY REFERENCES public.%[2]s (id), color text NOT NULL);
		INSERT INTO public.%[4]s VALUES (1, 'red');

		CREATE TABLE public.%[5]s (id int PRIMARY KEY, label text NOT NULL);
		INSERT INTO public.%[5]s VALUES (1, 'sql'), (2, 'go'), (3, 'rest');
		CREATE TABLE public.%[6]s (
			book_id int REFERENCES public.%[2]s (id), tag_id int REFERENCES public.%[5]s (id),
			PRIMARY KEY (book_id, tag_id));
		INSERT INTO public.%[6]s VALUES (1, 1), (1, 2), (2, 2), (2, 3);

		CREATE TABLE public.%[7]s (id int PRIMARY KEY, book_id int REFERENCES public.%[2]s (id), body text);
		INSERT INTO public.%[7]s VALUES (1, 1, 'not for the reader');

		ALTER TABLE public.%[3]s ENABLE ROW LEVEL SECURITY;
		CREATE POLICY reader_reads_published ON public.%[3]s FOR SELECT TO %[10]s USING (published);
		CREATE POLICY a_user_reads_their_own ON public.%[3]s FOR SELECT TO %[11]s
			USING (owner = current_setting('request.jwt.claim.sub', true));
		GRANT SELECT ON public.%[1]s, public.%[2]s, public.%[3]s, public.%[4]s, public.%[5]s, public.%[6]s
			TO %[10]s, %[11]s, %[12]s;
		GRANT SELECT ON public.%[7]s TO %[12]s;
	`, authors, books, chapters, covers, tags, bookTags, secrets, userA, userB, reader, signedIn, service))
}

// embedRest is rest for the SDK's address and the bearer tokens of the three
// callers these subjects ask as.
type embedRest struct {
	server *httptest.Server
	anon   string
	user   map[string]string
	admin  string
}

func newEmbedRest(t *testing.T) (embedRest, *sql.DB) {
	t.Helper()
	cfg, db := needsPostgres(t)
	stageEmbed(t, db)
	install := newInstall(t)
	pool := newProjectPool(t, alias)
	return embedRest{
		// The time limit is the machine's to spare: these subjects ask what
		// the answer is, and the bound itself is held by query_bounds_test.go.
		server: restForTheSDK(t, cfg, install, pool, func(c *config.Prest) { c.PGStatementTimeoutMS = 120000 }),
		anon:   token(t, install.private, reader),
		user: map[string]string{
			userA: userToken(t, pool.private, userA),
			userB: userToken(t, pool.private, userB),
		},
		admin: token(t, install.private, service),
	}, db
}

func (e embedRest) read(t *testing.T, bearer, query string, headers ...map[string]string) (int, http.Header, string) {
	t.Helper()
	extra := map[string]string{}
	for _, h := range headers {
		for k, v := range h {
			extra[k] = v
		}
	}
	return ask(t, e.server, http.MethodGet, "/rest/v1/"+query, bearer, extra)
}

// ok reads query as bearer and holds the answer to the JSON written out.
func (e embedRest) ok(t *testing.T, bearer, query, expected string) {
	t.Helper()
	status, _, body := e.read(t, bearer, query)
	require.Equal(t, http.StatusOK, status, "%s: %s", query, body)
	require.JSONEq(t, expected, body, query)
}

// refused reads query as bearer and returns the status and the refusal's JSON.
func (e embedRest) refused(t *testing.T, bearer, query string) (int, map[string]any) {
	t.Helper()
	status, _, body := e.read(t, bearer, query)
	var refusal map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	for _, field := range []string{"code", "message", "details", "hint"} {
		require.Contains(t, refusal, field, body)
	}
	return status, refusal
}

// directIDsAs is what a direct SQL query returns, asked as role with sub as the
// token's subject where there is one: the ids of query's single column.
func directIDsAs(t *testing.T, db *sql.DB, role, sub, query string) []int {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + role)
	require.NoError(t, err)
	if sub != "" {
		_, err = tx.Exec("SELECT set_config('request.jwt.claim.sub', $1, true)", sub)
		require.NoError(t, err)
	}
	rows, err := tx.Query(query)
	require.NoError(t, err)
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

func TestPostgRESTEmbed_eachKindOfRelationshipReturnsItsRows(t *testing.T) {
	rest, _ := newEmbedRest(t)

	// Many to one: a book's author, an object. The two keys to authors need a
	// hint, which the SDK writes as `table!column`. A book with no author
	// holds null.
	rest.ok(t, rest.anon, books+"?select=id,author:"+authors+"!author_id(id,name)&order=id", `[
		{"id": 1, "author": {"id": 1, "name": "Ada"}},
		{"id": 2, "author": {"id": 2, "name": "Grace"}},
		{"id": 3, "author": null}]`)
	rest.ok(t, rest.anon, books+"?select=id,editor:"+authors+"!"+books+"_editor_id_fkey(name)&order=id&id=lt.3", `[
		{"id": 1, "editor": {"name": "Grace"}},
		{"id": 2, "editor": {"name": "Ada"}}]`)

	// One to many: a book's chapters, an array; a book with none holds [].
	rest.ok(t, rest.anon, books+"?select=id,chapters:"+chapters+"(heading)&order=id", `[
		{"id": 1, "chapters": [{"heading": "Intro"}, {"heading": "Middle"}]},
		{"id": 2, "chapters": [{"heading": "One"}]},
		{"id": 3, "chapters": []}]`)

	// One to many, from the table the two keys point at.
	rest.ok(t, rest.anon, authors+"?select=name,written:"+books+"!author_id(title)&id=lt.3&order=id", `[
		{"name": "Ada", "written": [{"title": "Bundles"}]},
		{"name": "Grace", "written": [{"title": "Notes"}]}]`)

	// One to one: the cover's key is its primary key, so it is an object.
	rest.ok(t, rest.anon, books+"?select=id,cover:"+covers+"(color)&order=id", `[
		{"id": 1, "cover": {"color": "red"}},
		{"id": 2, "cover": null},
		{"id": 3, "cover": null}]`)

	// Many to many, through the join table, from either side.
	rest.ok(t, rest.anon, books+"?select=id,tags:"+tags+"(label)&order=id&id=lt.3", `[
		{"id": 1, "tags": [{"label": "sql"}, {"label": "go"}]},
		{"id": 2, "tags": [{"label": "go"}, {"label": "rest"}]}]`)
	rest.ok(t, rest.anon, tags+"?select=label,books:"+books+"(title)&order=id", `[
		{"label": "sql", "books": [{"title": "Bundles"}]},
		{"label": "go", "books": [{"title": "Bundles"}, {"title": "Notes"}]},
		{"label": "rest", "books": [{"title": "Notes"}]}]`)

	// A table pointing at itself: the key's column names the parent, and the
	// table with that column as a hint names the children.
	rest.ok(t, rest.anon, authors+"?select=name,mentor:mentor_id(name),students:"+authors+"!mentor_id(name)&order=id", `[
		{"name": "Ada", "mentor": null, "students": [{"name": "Grace"}, {"name": "Edsger"}]},
		{"name": "Grace", "mentor": {"name": "Ada"}, "students": []},
		{"name": "Edsger", "mentor": {"name": "Ada"}, "students": []}]`)

	// Embedded inside embedded, and the star.
	rest.ok(t, rest.anon, authors+"?select=name,written:"+books+"!author_id(title,chapters:"+chapters+"(heading),tags:"+tags+"(label))&id=eq.1", `[
		{"name": "Ada", "written": [{"title": "Bundles",
			"chapters": [{"heading": "Intro"}, {"heading": "Middle"}],
			"tags": [{"label": "sql"}, {"label": "go"}]}]}]`)
	rest.ok(t, rest.anon, books+"?select=*,"+covers+"(*)&id=eq.1", `[
		{"id": 1, "title": "Bundles", "pages": 300, "author_id": 1, "editor_id": 2,
			"meta": {"kind": "novel", "pub": {"year": 2021}}, "`+covers+`": {"book_id": 1, "color": "red"}}]`)

	// The single object the SDK's single() asks for holds the embedded rows.
	status, _, body := rest.read(t, rest.anon, books+"?select=title,chapters:"+chapters+"(heading)&id=eq.2",
		map[string]string{"Accept": "application/vnd.pgrst.object+json"})
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"title": "Notes", "chapters": [{"heading": "One"}]}`, body)
}

func TestPostgRESTEmbed_aFilterAndAnOrderOnTheEmbeddedTable(t *testing.T) {
	rest, _ := newEmbedRest(t)

	// A filter, an order, a limit and an offset on the embedded table. The
	// service role reads every chapter, the draft included.
	rest.ok(t, rest.admin, books+"?select=id,chapters:"+chapters+"(heading)&chapters.position=gt.1&chapters.order=position.desc&id=eq.1",
		`[{"id": 1, "chapters": [{"heading": "Draft"}, {"heading": "Middle"}]}]`)
	rest.ok(t, rest.admin, books+"?select=id,chapters:"+chapters+"(heading)&chapters.order=position.desc&chapters.limit=1&id=eq.1",
		`[{"id": 1, "chapters": [{"heading": "Draft"}]}]`)
	rest.ok(t, rest.admin, books+"?select=id,chapters:"+chapters+"(heading)&chapters.order=position&chapters.limit=1&chapters.offset=1&id=eq.1",
		`[{"id": 1, "chapters": [{"heading": "Middle"}]}]`)
	rest.ok(t, rest.admin, books+"?select=id,chapters:"+chapters+"(heading)&chapters.or=(heading.eq.Intro,heading.eq.Draft)&chapters.order=position&id=eq.1",
		`[{"id": 1, "chapters": [{"heading": "Intro"}, {"heading": "Draft"}]}]`)

	// Without !inner the parent stays, with nothing related.
	rest.ok(t, rest.anon, books+"?select=id,chapters:"+chapters+"(heading)&chapters.heading=eq.One&order=id",
		`[{"id": 1, "chapters": []}, {"id": 2, "chapters": [{"heading": "One"}]}, {"id": 3, "chapters": []}]`)

	// With !inner the parent goes: only a book with a matching chapter, and
	// the count and the page agree.
	status, headers, body := rest.read(t, rest.anon, books+"?select=id,chapters:"+chapters+"!inner(heading)&chapters.heading=eq.One&order=id",
		map[string]string{"Prefer": "count=exact"})
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"id": 2, "chapters": [{"heading": "One"}]}]`, body)
	require.Equal(t, "0-0/1", headers.Get("Content-Range"))

	// !inner with no filter keeps the books that have any related row, and an
	// embed with no columns filters without answering the key.
	rest.ok(t, rest.anon, books+"?select=id,cover:"+covers+"!inner(color)&order=id", `[{"id": 1, "cover": {"color": "red"}}]`)
	rest.ok(t, rest.anon, books+"?select=id,"+covers+"!inner()&order=id", `[{"id": 1}]`)

	// The count of a read that embeds, with a page smaller than it.
	_, headers, _ = rest.read(t, rest.anon, books+"?select=id,tags:"+tags+"(label)&limit=1&order=id",
		map[string]string{"Prefer": "count=exact"})
	require.Equal(t, "0-0/3", headers.Get("Content-Range"))

	// An embedded limit above the ceiling is refused as the top one is.
	status, refusal := rest.refused(t, rest.anon, books+"?select=id,chapters:"+chapters+"(id)&chapters.limit=100000")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "MS100", refusal["code"])
}

func TestPostgRESTEmbed_aRelatedRowTheRoleMayNotReadIsAbsent(t *testing.T) {
	rest, db := newEmbedRest(t)
	query := books + "?select=id,chapters:" + chapters + "(id,heading)&order=id"

	// The reader reads the published chapters, and the draft is absent.
	status, _, body := rest.read(t, rest.anon, query)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[
		{"id": 1, "chapters": [{"id": 1, "heading": "Intro"}, {"id": 2, "heading": "Middle"}]},
		{"id": 2, "chapters": [{"id": 4, "heading": "One"}]},
		{"id": 3, "chapters": []}]`, body)
	require.Equal(t, []int{1, 2, 4}, directIDsAs(t, db, reader, "", "SELECT id FROM public."+chapters+" ORDER BY id"))

	// Each User reads the chapters the row rule gives them, and the other's
	// are absent from the same embed.
	for sub, want := range map[string][]int{userA: {1, 3}, userB: {2, 4}} {
		_, _, body := rest.read(t, rest.user[sub], books+"?select=chapters:"+chapters+"(id)&order=id")
		var got []struct {
			Chapters []struct {
				ID int `json:"id"`
			} `json:"chapters"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got), body)
		ids := []int{}
		for _, book := range got {
			for _, chapter := range book.Chapters {
				ids = append(ids, chapter.ID)
			}
		}
		require.Equal(t, want, ids, sub)
		require.Equal(t, want, directIDsAs(t, db, signedIn, sub, "SELECT id FROM public."+chapters+" ORDER BY id"), sub)
	}

	// The service role passes the rules: every chapter.
	_, _, body = rest.read(t, rest.admin, books+"?select=id,chapters:"+chapters+"(id)&order=id&id=eq.1")
	require.JSONEq(t, `[{"id": 1, "chapters": [{"id": 1}, {"id": 2}, {"id": 3}]}]`, body)

	// !inner on a table whose rows the role may not read removes the parent
	// too: for the reader a book whose only chapter is a draft is gone.
	exec(t, db, fmt.Sprintf(`INSERT INTO public.%s VALUES (5, 3, 'Only a draft', 1, false, NULL);`, chapters))
	_, _, body = rest.read(t, rest.anon, books+"?select=id,"+chapters+"!inner(id)&order=id")
	require.JSONEq(t, `[{"id": 1, "`+chapters+`": [{"id": 1}, {"id": 2}]}, {"id": 2, "`+chapters+`": [{"id": 4}]}]`, body)

	// A table the role holds no grant on refuses the whole read, in the
	// Database's words, as a read of it would.
	status, refusal := rest.refused(t, rest.anon, books+"?select=id,"+secrets+"(body)")
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, "42501", refusal["code"])
	_, _, body = rest.read(t, rest.admin, books+"?select=id,"+secrets+"(body)&id=eq.1")
	require.JSONEq(t, `[{"id": 1, "`+secrets+`": [{"body": "not for the reader"}]}]`, body)
}

func TestPostgRESTEmbed_aRelationshipThatIsNotThereIsRefusedAsPostgRESTRefuses(t *testing.T) {
	rest, _ := newEmbedRest(t)

	// No relationship between the two tables. Expected: 400, PGRST200.
	status, refusal := rest.refused(t, rest.anon, books+"?select=id,"+secrets+"x(id)")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "PGRST200", refusal["code"])
	require.Equal(t, "Could not find a relationship between '"+books+"' and '"+secrets+"x' in the schema cache", refusal["message"])
	require.Equal(t, "Searched for a foreign key relationship between '"+books+"' and '"+secrets+"x' in the schema 'public', but no matches were found.", refusal["details"])
	require.Equal(t, "Perhaps you meant '"+secrets+"' instead of '"+secrets+"x'.", refusal["hint"])

	// A table that has no foreign key at all, and a hint that names nothing.
	status, refusal = rest.refused(t, rest.anon, books+"?select=id,nothing_like_it(id)")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "PGRST200", refusal["code"])
	require.Nil(t, refusal["hint"])
	status, refusal = rest.refused(t, rest.anon, books+"?select=id,"+chapters+"!nope(id)")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "PGRST200", refusal["code"])
	require.Contains(t, refusal["details"], "using the hint 'nope'")

	// Two foreign keys to the table named. Expected: 300, PGRST201, the two
	// candidates in details, and the hints to write in the hint.
	status, refusal = rest.refused(t, rest.anon, books+"?select=id,"+authors+"(name)")
	require.Equal(t, http.StatusMultipleChoices, status)
	require.Equal(t, "PGRST201", refusal["code"])
	require.Equal(t, "Could not embed because more than one relationship was found for '"+books+"' and '"+authors+"'", refusal["message"])
	require.Equal(t, []any{
		map[string]any{"embedding": books + " with " + authors, "cardinality": "many-to-one",
			"relationship": books + "_author_id_fkey using " + books + "(author_id) and " + authors + "(id)"},
		map[string]any{"embedding": books + " with " + authors, "cardinality": "many-to-one",
			"relationship": books + "_editor_id_fkey using " + books + "(editor_id) and " + authors + "(id)"},
	}, refusal["details"])
	require.Equal(t, "Try changing '"+authors+"' to one of the following: '"+authors+"!"+books+"_author_id_fkey', '"+
		authors+"!"+books+"_editor_id_fkey'. Find the desired relationship in the 'details' key.", refusal["hint"])

	// A table pointing at itself is not ambiguous: its own name is the
	// children and the key's column is the parent, as PostgREST reads them.
	rest.ok(t, rest.anon, authors+"?select=name,"+authors+"(name)&id=eq.1",
		`[{"name": "Ada", "`+authors+`": [{"name": "Grace"}, {"name": "Edsger"}]}]`)

	// The join table named as the hint picks the many-to-many.
	status, _, body := rest.read(t, rest.anon, books+"?select=id,"+tags+"!"+bookTags+"(label)&id=eq.2")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"id": 2, "`+tags+`": [{"label": "go"}, {"label": "rest"}]}]`, body)

	// A spread is not answered, and the refusal says so.
	status, refusal = rest.refused(t, rest.anon, books+"?select=id,..."+covers+"(color)")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "PGRST100", refusal["code"])

	// Tables embedded deeper than the ceiling.
	deep := "id"
	for range pgrDeepEnough {
		deep = chapters + "(" + deep + ")"
	}
	status, refusal = rest.refused(t, rest.anon, books+"?select="+deep)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "PGRST100", refusal["code"])
}

// pgrDeepEnough is more embeds, one inside the next, than a read may nest.
const pgrDeepEnough = 8

func TestPostgRESTEmbed_anAliasACastAndAJSONPath(t *testing.T) {
	rest, _ := newEmbedRest(t)

	// An alias renames a key, a cast changes a value's type, a path reaches
	// into a json column: `->` answers json and `->>` text.
	rest.ok(t, rest.anon, books+"?select=id,name:title,pages::text,kind:meta->>kind,year:meta->pub->>year,pub:meta->pub&id=lt.3&order=id", `[
		{"id": 1, "name": "Bundles", "pages": "300", "kind": "novel", "year": "2021", "pub": {"year": 2021}},
		{"id": 2, "name": "Notes", "pages": "120", "kind": "essay", "year": "2019", "pub": {"year": 2019}}]`)
	// Without an alias a path is answered under its last key.
	rest.ok(t, rest.anon, books+"?select=id,meta->>kind&id=eq.2", `[{"id": 2, "kind": "essay"}]`)
	// An alias and a cast inside an embedded table, and a cast with a size.
	rest.ok(t, rest.anon, books+"?select=id,first:"+chapters+"(n:position::numeric(4,1),title:heading)&id=eq.2",
		`[{"id": 2, "first": [{"n": 1.0, "title": "One"}]}]`)

	// A filter and an order on a path.
	rest.ok(t, rest.anon, books+"?select=id&meta->>kind=eq.essay", `[{"id": 2}]`)
	rest.ok(t, rest.anon, books+"?select=id&meta->pub->>year=gt.2020", `[{"id": 1}]`)
	rest.ok(t, rest.anon, books+"?select=id&meta->pub->>year=not.is.null&order=meta->pub->>year.desc", `[{"id": 1}, {"id": 2}]`)

	// A cast is a type's name and nothing else, a path is words, and ->> ends one.
	for _, query := range []string{
		"select=pages::text%3Bdrop",
		"select=pages::int)--",
		"select=meta->>kind->x",
		"select=meta->'kind'",
		"select=meta->",
	} {
		status, refusal := rest.refused(t, rest.anon, books+"?"+query)
		require.Equal(t, http.StatusBadRequest, status, query)
		require.Equal(t, "PGRST100", refusal["code"], query)
	}
}
