package connection

import (
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

// miniship: a name the manager was not given is refused before a connection
// is attempted, and is never tried as a database on the default host.
//
// Not parallel: it replaces dbConnect.
func TestAddDatabaseToPool_refusesANameItWasNotGiven(t *testing.T) {
	defaults := config.Prest{
		PGHost:     "default-host",
		PGPort:     5432,
		PGUser:     "authenticator",
		PGDatabase: "given",
		PGSSLMode:  "disable",
	}
	withRegistry := defaults
	withRegistry.Databases = []config.DatabaseConf{{
		Alias:    "tenant-a",
		Host:     "tenant-a-host",
		Port:     5432,
		User:     "authenticator",
		Database: "tenant_a",
		SSL:      config.DatabaseSSLConf{Mode: "disable"},
	}}

	for _, tc := range []struct {
		desc     string
		cfg      config.Prest
		given    string
		givenDSN string
		notGiven []string
	}{
		{
			desc:     "without a registry",
			cfg:      defaults,
			given:    "given",
			givenDSN: "dbname=given host=default-host",
			notGiven: []string{"postgres", "template1", "other"},
		},
		{
			desc:     "with a registry",
			cfg:      withRegistry,
			given:    "tenant-a",
			givenDSN: "dbname=tenant_a host=tenant-a-host",
			notGiven: []string{"tenant_a", "postgres", "other"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var dsns []string
			restore := SetDBConnectForTest(func(_, dsn string) (*sqlx.DB, error) {
				dsns = append(dsns, dsn)
				return nil, errors.New("the test double opens no connection")
			})
			t.Cleanup(restore)
			m := NewManager(&tc.cfg)

			for _, name := range tc.notGiven {
				require.Empty(t, m.GetURI(name), name)
				db, err := m.AddDatabaseToPool(name)
				require.ErrorIs(t, err, ErrDatabaseNotGiven, name)
				require.Nil(t, db)
				_, err = m.GetFromPool(name)
				require.Error(t, err, name)
			}
			require.Empty(t, dsns, "a connection was attempted")

			// The control: the name the manager was given is attempted, on
			// its own host.
			_, err := m.AddDatabaseToPool(tc.given)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrDatabaseNotGiven)
			require.Len(t, dsns, 1)
			require.Contains(t, dsns[0], tc.givenDSN)
		})
	}
}
