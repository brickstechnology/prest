package anonymousrole_test

// miniship (miniship-cloud#1302): the Supabase SDK's read, GET and HEAD on
// /rest/v1/{table} in PostgREST's language, against a live Postgres. Every
// filter's answer is held to the rows a direct SQL query returns for the same
// question; the roles are held to the row rules; a User's token is checked
// against the key set of the Project the call is for, and its sub reaches a
// row rule as request.jwt.claim.sub.
//
// The public repository's docker/tests/database-sdk.test.ts asks the same
// questions with the real SDK through the gateway; these are the ones a Go
// test can ask of rest alone.

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

const (
	// articles is the table these subjects read.
	articles = "rest_pgr_articles"
	// signedIn is the role a User's token names. It is PostgREST's and
	// Supabase's name, and the one projectkeys admits, so it is not this
	// package's own; it is made if missing and left for the next run.
	signedIn = "authenticated"
	// projectHeader is the header these subjects name the Project in, as a
	// gateway in front of rest does on miniship cloud.
	projectHeader = "x-miniship-project"
)

// The two Users of the Project's pool, and the rows each owns.
const (
	userA = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	userB = "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"
)

// stagePostgREST makes the articles table and its row rules:
//
//   - the reader (the anonymous role) sees published rows;
//   - a signed-in User sees the rows whose owner is their token's sub, read by
//     the rule as request.jwt.claim.sub, as Supabase's auth.uid() reads it;
//   - service_role passes the rules.
func stagePostgREST(t *testing.T, db *sql.DB) {
	t.Helper()
	stageTokenRole(t, db)
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s;`, articles)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[2]s') THEN
				CREATE ROLE %[2]s NOLOGIN NOBYPASSRLS;
			END IF;
		END $$;
		GRANT %[2]s TO %[3]s;
		CREATE TABLE public.%[1]s (
			id int PRIMARY KEY, title text NOT NULL, author_id int, views int NOT NULL,
			published boolean NOT NULL, owner text);
		INSERT INTO public.%[1]s VALUES
			(1, 'how bundles are built', 1, 340, true, '%[6]s'),
			(2, 'Bundles, again', 2, 120, true, '%[6]s'),
			(3, 'a quiet note', 3, 880, true, '%[7]s'),
			(4, 'the long read', 1, 1200, true, '%[7]s'),
			(5, 'no author yet', NULL, 900, true, NULL),
			(6, 'a draft of A', 2, 15, false, '%[6]s'),
			(7, 'a draft of B', 3, 5, false, '%[7]s');
		ALTER TABLE public.%[1]s ENABLE ROW LEVEL SECURITY;
		CREATE POLICY anon_reads_published ON public.%[1]s FOR SELECT TO %[4]s USING (published);
		CREATE POLICY a_user_reads_their_own ON public.%[1]s FOR SELECT TO %[2]s
			USING (owner = current_setting('request.jwt.claim.sub', true));
		GRANT SELECT ON public.%[1]s TO %[4]s, %[2]s, %[5]s;
	`, articles, signedIn, login, reader, service, userA, userB))
	t.Cleanup(func() {
		exec(t, db, fmt.Sprintf(`REVOKE %s FROM %s;`, signedIn, login))
	})
}

// projectPool is the Project's User pool: the key its auth api signs a
// User's token with, and the key-set address rest reads it at.
type projectPool struct {
	private ed25519.PrivateKey
	server  *httptest.Server
}

func newProjectPool(t *testing.T, project string) projectPool {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := jose.JSONWebKeySet{}
		if r.URL.Path == "/auth/v1/projects/"+project+"/.well-known/jwks.json" {
			keys.Keys = []jose.JSONWebKey{{Key: public, KeyID: "k_pool", Algorithm: "EdDSA", Use: "sig"}}
		}
		_ = json.NewEncoder(w).Encode(keys)
	}))
	t.Cleanup(server.Close)
	return projectPool{private: private, server: server}
}

func (p projectPool) keysURL() string {
	return p.server.URL + "/auth/v1/projects/" + config.ProjectPlaceholder + "/.well-known/jwks.json"
}

// userToken is a User's token as the auth api mints one under /auth/v1.
func userToken(t *testing.T, key ed25519.PrivateKey, sub string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k_pool"))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(map[string]any{
		"sub": sub, "aud": "authenticated", "role": "authenticated",
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	}).Serialize()
	require.NoError(t, err)
	return token
}

// restForTheSDK is rest as an install composes it for the SDK's address: it
// checks the install's tokens, reads the Project from the gateway's header,
// and reads a User's key set from the Project's pool.
func restForTheSDK(t *testing.T, cfg *config.Prest, install theInstall, pool projectPool, with ...func(*config.Prest)) *httptest.Server {
	t.Helper()
	return restAs(t, cfg, reader, append([]func(*config.Prest){func(c *config.Prest) {
		c.JWTRoleClaim = "role"
		c.JWTAlgo = "ES256"
		c.JWTJWKS = install.jwks
		c.PostgREST = config.PostgRESTConf{ProjectHeader: projectHeader, ProjectKeysURL: pool.keysURL()}
	}}, with...)...)
}

// ask sends one SDK-shaped request for the Project and answers its status,
// headers and body.
func ask(t *testing.T, server *httptest.Server, method, path, bearer string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	header := http.Header{}
	header.Set(projectHeader, alias)
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		header.Set(k, v)
	}
	return call(t, server, method, path, header)
}

// idsIn is the ids of the rows of an answer, in the answer's order.
func idsIn(t *testing.T, body string) []int {
	t.Helper()
	var rows []struct {
		ID int `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rows), body)
	ids := make([]int, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// directIDs is what a direct SQL query returns for the same question, asked
// as the reader with the same row rules applying.
func directIDs(t *testing.T, db *sql.DB, where, order string) []int {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + reader)
	require.NoError(t, err)
	query := "SELECT id FROM public." + articles
	if where != "" {
		query += " WHERE " + where
	}
	if order == "" {
		order = "id"
	}
	rows, err := tx.Query(query + " ORDER BY " + order)
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

func TestPostgREST_eachFilterReturnsTheRowsADirectQueryReturns(t *testing.T) {
	cfg, db := needsPostgres(t)
	stagePostgREST(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)

	for _, c := range []struct {
		query, where, order string
	}{
		{"select=id,title&order=id", "", ""},
		{"select=id&author_id=eq.1&order=id", "author_id = 1", ""},
		{"select=id&author_id=neq.1&order=id", "author_id <> 1", ""},
		{"select=id&views=gt.880&order=id", "views > 880", ""},
		{"select=id&views=gte.880&order=id", "views >= 880", ""},
		{"select=id&views=lt.340&order=id", "views < 340", ""},
		{"select=id&views=lte.340&order=id", "views <= 340", ""},
		{"select=id&title=like.%25bundles%25&order=id", "title LIKE '%bundles%'", ""},
		{"select=id&title=ilike.%25BUNDLES%25&order=id", "title ILIKE '%BUNDLES%'", ""},
		{"select=id&author_id=is.null&order=id", "author_id IS NULL", ""},
		{"select=id&id=in.(1,3,5)&order=id", "id IN (1, 3, 5)", ""},
		{"select=id&or=(views.gt.1000,author_id.eq.3)&order=id", "views > 1000 OR author_id = 3", ""},
		{"select=id&author_id=not.is.null&order=id", "author_id IS NOT NULL", ""},
		{"select=id&order=views.desc", "", "views DESC"},
		{"select=id&order=id&limit=2", "id IN (1, 2)", ""},
		{"select=id&order=id&offset=2&limit=2", "id IN (3, 4)", ""},
	} {
		// The SDK's request for one question.
		// Expected: 200 and the ids a direct query returns, in its order.
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?"+c.query, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s: %s", c.query, body)
		require.Equal(t, directIDs(t, db, c.where, c.order), idsIn(t, body), c.query)
	}

	// The columns named, and only those.
	_, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id,title&id=eq.4", anon, nil)
	require.JSONEq(t, `[{"id": 4, "title": "the long read"}]`, body)
}

func TestPostgREST_oneObjectAndTheCount(t *testing.T) {
	cfg, db := needsPostgres(t)
	stagePostgREST(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)
	object := map[string]string{"Accept": "application/vnd.pgrst.object+json"}

	// single(): one row, as an object.
	status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id,title&id=eq.4", anon, object)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"id": 4, "title": "the long read"}`, body)

	// single() with no row and with several: PGRST116, which the SDK reads.
	for query, rows := range map[string]int{"id=eq.999": 0, "author_id=eq.1": 2} {
		status, _, body = ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&"+query, anon, object)
		require.Equal(t, http.StatusNotAcceptable, status, body)
		var refusal map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &refusal))
		require.Equal(t, "PGRST116", refusal["code"])
		require.Equal(t, fmt.Sprintf("The result contains %d rows", rows), refusal["details"])
	}

	// A count in each of the SDK's three modes, and its Content-Range.
	for _, mode := range []string{"exact", "planned", "estimated"} {
		status, headers, body := ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&views=gt.800&order=id&limit=2", anon,
			map[string]string{"Prefer": "count=" + mode})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, "0-1/3", headers.Get("Content-Range"), mode)
	}

	// The SDK's head: true is HEAD: the count and no body.
	status, headers, body := ask(t, rest, http.MethodHead, "/rest/v1/"+articles+"?select=*", anon,
		map[string]string{"Prefer": "count=exact"})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, body)
	require.Equal(t, "0-4/5", headers.Get("Content-Range"))

	// No count asked: the page, and * for the total.
	_, headers, _ = ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&id=eq.999", anon, nil)
	require.Equal(t, "*/*", headers.Get("Content-Range"))
}

func TestPostgREST_theRowRulesDecideForEachCaller(t *testing.T) {
	cfg, db := needsPostgres(t)
	stagePostgREST(t, db)
	install := newInstall(t)
	pool := newProjectPool(t, alias)
	rest := restForTheSDK(t, cfg, install, pool)
	read := "/rest/v1/" + articles + "?select=id&order=id"

	// The anon token, which the gateway puts in place of the publishable key.
	// Expected: the published rows.
	_, _, body := ask(t, rest, http.MethodGet, read, token(t, install.private, reader), nil)
	require.Equal(t, []int{1, 2, 3, 4, 5}, idsIn(t, body))

	// Each User's own token. Expected: that User's rows, drafts included, and
	// none of the other's.
	_, _, body = ask(t, rest, http.MethodGet, read, userToken(t, pool.private, userA), nil)
	require.Equal(t, []int{1, 2, 6}, idsIn(t, body))
	_, _, body = ask(t, rest, http.MethodGet, read, userToken(t, pool.private, userB), nil)
	require.Equal(t, []int{3, 4, 7}, idsIn(t, body))

	// The api service token, which the gateway puts in place of the secret
	// key. Expected: every row.
	_, _, body = ask(t, rest, http.MethodGet, read, token(t, install.private, service), nil)
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, idsIn(t, body))

	// A User's token signed by a key that is not the Project's pool's.
	// Expected: 401 PGRST301, and no row.
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	status, _, body := ask(t, rest, http.MethodGet, read, userToken(t, stranger, userA), nil)
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.Contains(t, body, `"code":"PGRST301"`)

	// The same User's token on today's address. Expected: refused, as before.
	status, _ = getWith(t, rest, "/"+alias+"/public/"+articles, "Bearer "+userToken(t, pool.private, userA))
	require.Equal(t, http.StatusUnauthorized, status)
}

func TestPostgREST_theBoundsAreRefusedInTheSDKsShape(t *testing.T) {
	cfg, db := needsPostgres(t)
	stagePostgREST(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias), func(c *config.Prest) {
		c.PGMaxPageSize = 3
		c.PGStatementTimeoutMS = 250
	})
	anon := token(t, install.private, reader)
	refusal := func(body string) map[string]any {
		var answer map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &answer), body)
		for _, field := range []string{"code", "message", "details", "hint"} {
			require.Contains(t, answer, field, body)
		}
		return answer
	}

	// More rows than a read answers. Expected: refused, MS100.
	status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&limit=4", anon, nil)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "MS100", refusal(body)["code"])

	// No limit at all. Expected: the ceiling's page, as the table read caps.
	_, _, body = ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&order=id", anon, nil)
	require.Equal(t, []int{1, 2, 3}, idsIn(t, body))

	// A query string longer than rest reads. Expected: 414, MS101.
	long := "/rest/v1/" + articles + "?select=id&title=in.(" + strings.Repeat("a,", 9000) + "a)"
	status, _, body = ask(t, rest, http.MethodGet, long, anon, nil)
	require.Equal(t, http.StatusRequestURITooLong, status)
	require.Equal(t, "MS101", refusal(body)["code"])

	// A read the Database cancels at the time limit. Expected: 504, 57014.
	exec(t, db, fmt.Sprintf(`CREATE OR REPLACE VIEW public.rest_pgr_slow AS SELECT 1 AS id, pg_sleep(5) IS NULL AS slept;
		GRANT SELECT ON public.rest_pgr_slow TO %s;`, reader))
	t.Cleanup(func() { exec(t, db, `DROP VIEW IF EXISTS public.rest_pgr_slow;`) })
	status, _, body = ask(t, rest, http.MethodGet, "/rest/v1/rest_pgr_slow", anon, nil)
	require.Equal(t, http.StatusGatewayTimeout, status, body)
	require.Equal(t, "57014", refusal(body)["code"])

	// Another schema. Expected: 406, PGRST106.
	status, _, body = ask(t, rest, http.MethodGet, "/rest/v1/"+articles, anon, map[string]string{"Accept-Profile": "private"})
	require.Equal(t, http.StatusNotAcceptable, status)
	require.Equal(t, "PGRST106", refusal(body)["code"])

	// A filter's value of the wrong type, and a table that is not there.
	status, _, body = ask(t, rest, http.MethodGet, "/rest/v1/"+articles+"?id=eq.one", anon, nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "22P02", refusal(body)["code"])
	status, _, body = ask(t, rest, http.MethodGet, "/rest/v1/rest_pgr_absent", anon, nil)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "42P01", refusal(body)["code"])

	// A write, which this address does not take yet. Expected: 404.
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		status, _, _ = ask(t, rest, method, "/rest/v1/"+articles+"?id=eq.1", anon, nil)
		require.Equal(t, http.StatusNotFound, status, method)
	}
}

func TestPostgREST_aSelfHostInstallServesItsOneProject(t *testing.T) {
	cfg, db := needsPostgres(t)
	stagePostgREST(t, db)
	install := newInstall(t)
	const project = "prj_the_one"
	pool := newProjectPool(t, project)

	// The lookup, as the api answers it: asked for no Project, it names the
	// install's one.
	var asked []string
	answerer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"project":  project,
			"url":      dsn(cfg, login, password),
			"anonRole": reader,
		})
	}))
	t.Cleanup(answerer.Close)

	rest := restAs(t, cfg, reader, func(c *config.Prest) {
		c.Databases = nil
		c.JWTRoleClaim = "role"
		c.JWTAlgo = "ES256"
		c.JWTJWKS = install.jwks
		c.Admission = config.AdmissionConf{URL: answerer.URL + "/database", Key: "the api secret key"}
		c.PostgREST = config.PostgRESTConf{ProjectKeysURL: pool.keysURL()}
	})

	// A read with no Project named, twice, and a User's read.
	// Expected: the one Project's rows, one lookup, and the User's own rows
	// checked against that Project's key set.
	for range 2 {
		status, _, body := call(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&order=id", http.Header{
			"Authorization": {"Bearer " + token(t, install.private, reader)},
			projectHeader:   {"prj_typed_by_a_caller"},
		})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, []int{1, 2, 3, 4, 5}, idsIn(t, body))
	}
	require.Equal(t, []string{"/database/"}, asked)

	_, _, body := call(t, rest, http.MethodGet, "/rest/v1/"+articles+"?select=id&order=id", http.Header{
		"Authorization": {"Bearer " + userToken(t, pool.private, userA)},
	})
	ids := idsIn(t, body)
	sort.Ints(ids)
	require.Equal(t, []int{1, 2, 6}, ids)
}
