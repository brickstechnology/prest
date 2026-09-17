package connection

import (
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

// miniship: rest's connections say who they are, so a Database can tell its
// rest sessions from every other one in pg_stat_activity
// (miniship-cloud#547). integration/postgres/twoprojects reads the name back
// out of a live Postgres.

func TestWithApplicationName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		uri  string
		want string
	}{
		{
			desc: "a keyword DSN, as the manager builds one from a profile",
			uri:  "user=authenticator dbname=tenant_a host=db-a port=5432 sslmode=disable connect_timeout=10",
			want: "user=authenticator dbname=tenant_a host=db-a port=5432 sslmode=disable connect_timeout=10 fallback_application_name=rest",
		},
		{
			desc: "a URL with no query, as DATABASE_URL_1 may be written",
			uri:  "postgres://authenticator@db-a:5432/tenant_a",
			want: "postgres://authenticator@db-a:5432/tenant_a?fallback_application_name=rest",
		},
		{
			desc: "a URL that already carries a query",
			uri:  "postgres://authenticator@db-a:5432/tenant_a?sslmode=disable",
			want: "postgres://authenticator@db-a:5432/tenant_a?sslmode=disable&fallback_application_name=rest",
		},
		{
			desc: "postgresql:// is the same scheme under its other spelling",
			uri:  "postgresql://authenticator@db-a:5432/tenant_a",
			want: "postgresql://authenticator@db-a:5432/tenant_a?fallback_application_name=rest",
		},
		{
			desc: "an operator who named their own keeps it",
			uri:  "postgres://authenticator@db-a:5432/tenant_a?application_name=theirs",
			want: "postgres://authenticator@db-a:5432/tenant_a?application_name=theirs",
		},
		{
			desc: "and so does one who named their own fallback",
			uri:  "user=authenticator dbname=tenant_a fallback_application_name=theirs",
			want: "user=authenticator dbname=tenant_a fallback_application_name=theirs",
		},
		{
			desc: "a URI the manager refused to build is left alone",
			uri:  "",
			want: "",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, WithApplicationName(tc.uri))
		})
	}
}

// Not parallel: it replaces dbConnect.
func TestAddDatabaseToPool_namesRestToTheDatabase(t *testing.T) {
	cfg := config.Prest{
		PGHost:     "default-host",
		PGPort:     5432,
		PGUser:     "authenticator",
		PGDatabase: "given",
		PGSSLMode:  "disable",
		Databases: []config.DatabaseConf{{
			Alias:    "tenant-a",
			URL:      "postgres://authenticator@tenant-a-host:5432/tenant_a?sslmode=disable",
			Database: "tenant_a",
		}},
	}

	var dsns []string
	restore := SetDBConnectForTest(func(_, dsn string) (*sqlx.DB, error) {
		dsns = append(dsns, dsn)
		return nil, errors.New("the test double opens no connection")
	})
	t.Cleanup(restore)
	m := NewManager(&cfg)

	_, err := m.AddDatabaseToPool("tenant-a")
	require.Error(t, err)
	require.Len(t, dsns, 1)
	require.Contains(t, dsns[0], "fallback_application_name="+ApplicationName)

	// And the pool is still keyed by the Database, not by what rest calls
	// itself: GetURI answers the address, and nothing about the caller.
	require.Equal(t, cfg.Databases[0].URL, m.GetURI("tenant-a"))
}
