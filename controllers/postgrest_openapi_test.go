package controllers

// miniship (miniship-cloud#1890): the OpenAPI document of /rest/v1/, held to
// what PostgREST v14.18 writes (Response/OpenAPI.hs). The live-Postgres half,
// with the roles and the grants, is in integration/postgres/anonymousrole.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }

func TestOpenAPI_refusalIsSupabasesBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAPIRefusal(rec)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"message":"Access to schema is forbidden","hint":"Accessing the schema via the Data API is only allowed using a secret API key."}`, rec.Body.String())
}

func TestOpenAPI_theRootAcceptsOpenAPIJSONJSONAndAnything(t *testing.T) {
	for _, accept := range []string{"", "application/openapi+json", "application/json", "*/*", "text/csv, application/openapi+json", "text/plain;q=0.1, */*;q=0.1"} {
		require.NoError(t, pgrOpenAPIAccepted(accept), accept)
	}
	err := pgrOpenAPIAccepted("text/csv")
	var refused *pgrError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusNotAcceptable, refused.status)
	require.Equal(t, "PGRST107", refused.code)
	require.Equal(t, "None of these media types are available: text/csv", refused.message)
}

func TestOpenAPI_documentFollowsPostgRESTsShape(t *testing.T) {
	src := openAPISource{
		title: str("standard public schema"),
		fks:   map[[2]string]openAPIFK{{"posts", "author_id"}: {"people", "id"}},
		tables: []openAPITable{{
			Name: "posts", Description: str("Posts\n\nWhat people wrote."), Insertable: true, Updatable: true, Deletable: true,
			PKCols: []string{"id"},
			Columns: []openAPIColumn{
				{Name: "id", Nullable: false, Type: "integer", Default: str("nextval('posts_id_seq'::regclass)")},
				{Name: "title", Nullable: false, Type: "character varying", MaxLength: num(40), Default: str("'untitled'::character varying"), Description: str("The headline")},
				{Name: "author_id", Nullable: true, Type: "integer"},
				{Name: "tags", Nullable: true, Type: "text[]"},
				{Name: "meta", Nullable: true, Type: "jsonb"},
				{Name: "mood", Nullable: true, Type: "mood", Enum: []string{"ok", "sad"}},
				{Name: "views", Nullable: false, Type: "bigint", Default: str("0")},
			},
		}, {
			Name: "ro", Columns: []openAPIColumn{{Name: "x", Nullable: true, Type: "boolean", Default: str("false")}},
		}},
		routines: []openAPIRoutine{
			{Name: "add", Volatility: "i", Params: []openAPIParam{{"a", "integer", true, false}, {"b", "integer", false, false}}},
			{Name: "stamp", Volatility: "v", Description: str("Stamps\nit")},
		},
	}
	raw, err := encodeOpenAPI(buildOpenAPI(src))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))

	require.Equal(t, "2.0", doc["swagger"])
	require.Equal(t, map[string]any{"title": "standard public schema", "description": "", "version": "14.18"}, doc["info"])
	require.Equal(t, "0.0.0.0:3000", doc["host"])
	require.Equal(t, "/", doc["basePath"])
	require.Equal(t, []any{"http"}, doc["schemes"])
	require.Equal(t, map[string]any{"description": "PostgREST Documentation", "url": "https://postgrest.org/en/v14/references/api.html"}, doc["externalDocs"])
	require.Equal(t, []any{"application/json", "application/vnd.pgrst.object+json;nulls=stripped", "application/vnd.pgrst.object+json", "text/csv"}, doc["produces"])
	require.NotContains(t, doc, "securityDefinitions")

	defs := doc["definitions"].(map[string]any)
	require.Len(t, defs, 2)
	posts := defs["posts"].(map[string]any)
	require.Equal(t, "object", posts["type"])
	require.Equal(t, "Posts\n\nWhat people wrote.", posts["description"])
	require.Equal(t, []any{"id", "title", "views"}, posts["required"])
	props := posts["properties"].(map[string]any)
	require.Equal(t, map[string]any{"format": "int32", "type": "integer", "description": "Note:\nThis is a Primary Key.<pk/>"}, props["id"])
	require.Equal(t, map[string]any{
		"format": "character varying", "type": "string", "maxLength": float64(40), "default": "untitled", "description": "The headline",
	}, props["title"])
	require.Equal(t, map[string]any{
		"format": "int32", "type": "integer",
		"description": "Note:\nThis is a Foreign Key to `people.id`.<fk table='people' column='id'/>",
	}, props["author_id"])
	require.Equal(t, map[string]any{"format": "text[]", "type": "array", "items": map[string]any{"type": "string"}}, props["tags"])
	require.Equal(t, map[string]any{"format": "jsonb"}, props["meta"])
	require.Equal(t, map[string]any{"format": "mood", "type": "string", "enum": []any{"ok", "sad"}}, props["mood"])
	require.Equal(t, map[string]any{"format": "int64", "type": "integer", "default": float64(0)}, props["views"])
	require.NotContains(t, defs["ro"], "required")

	paths := doc["paths"].(map[string]any)
	require.Equal(t, []string{"/", "/posts", "/ro", "/rpc/add", "/rpc/stamp"}, sortedKeys(paths))
	require.Contains(t, paths["/posts"], "post")
	require.Contains(t, paths["/posts"], "patch")
	require.Contains(t, paths["/posts"], "delete")
	require.NotContains(t, paths["/ro"], "post")
	get := paths["/posts"].(map[string]any)["get"].(map[string]any)
	require.Equal(t, "Posts", get["summary"])
	require.Equal(t, "What people wrote.", get["description"])
	require.Equal(t, []any{map[string]any{"$ref": "#/parameters/rowFilter.posts.id"}, map[string]any{"$ref": "#/parameters/rowFilter.posts.title"}},
		get["parameters"].([]any)[:2])
	require.Equal(t, map[string]any{"description": "Partial Content"}, get["responses"].(map[string]any)["206"])

	params := doc["parameters"].(map[string]any)
	require.Contains(t, params, "body.posts")
	require.Contains(t, params, "rowFilter.posts.title")
	require.Equal(t, map[string]any{"name": "title", "description": "The headline", "required": false, "in": "query", "type": "string"}, params["rowFilter.posts.title"])
	require.Equal(t, "items", params["rangeUnit"].(map[string]any)["default"])

	add := paths["/rpc/add"].(map[string]any)
	require.Contains(t, add, "get")
	body := add["post"].(map[string]any)["parameters"].([]any)[0].(map[string]any)["schema"].(map[string]any)
	require.Equal(t, []any{"a"}, body["required"])
	require.Equal(t, map[string]any{"format": "int32", "type": "integer"}, body["properties"].(map[string]any)["b"])
	require.Equal(t, []any{map[string]any{"name": "a", "required": true, "in": "query", "type": "integer", "format": "int32"},
		map[string]any{"name": "b", "required": false, "in": "query", "type": "integer", "format": "int32"}}, add["get"].(map[string]any)["parameters"])

	stamp := paths["/rpc/stamp"].(map[string]any)
	require.NotContains(t, stamp, "get")
	require.Equal(t, "Stamps", stamp["post"].(map[string]any)["summary"])
	require.Equal(t, "it", stamp["post"].(map[string]any)["description"])
}

func TestOpenAPI_theInfoOfAMultiLineSchemaComment(t *testing.T) {
	doc := buildOpenAPI(openAPISource{title: str("My API\n\nThe long story.")})
	require.Equal(t, oaObj{"title": "My API", "description": "The long story.", "version": "14.18"}, doc["info"])
	doc = buildOpenAPI(openAPISource{})
	require.Equal(t, oaObj{"title": "PostgREST API", "description": "This is a dynamic API generated by PostgREST", "version": "14.18"}, doc["info"])
}

func TestOpenAPI_defaultsAreParsedAsPostgRESTParsesThem(t *testing.T) {
	for _, c := range []struct {
		colType, def string
		want         any
		ok           bool
	}{
		{"text", "'a'::text", "a", true},
		{"character varying", "'b'::character varying", "b", true},
		{"timestamp with time zone", "now()", "now()", true},
		{"integer", "nextval('s'::regclass)", nil, false},
		{"integer", "0", float64(0), true},
		{"boolean", "true", true, true},
		{"jsonb", "'{}'::jsonb", nil, false},
		{"text", "'it\"s'::text", nil, false},
	} {
		got, ok := openAPIDefault(c.colType, c.def)
		require.Equal(t, c.ok, ok, c.def)
		require.Equal(t, c.want, got, c.def)
	}
}

func TestOpenAPI_theBodyIsNotHTMLEscaped(t *testing.T) {
	raw, err := encodeOpenAPI(oaObj{"note": "<pk/>"})
	require.NoError(t, err)
	require.Equal(t, `{"note":"<pk/>"}`, string(raw))
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
