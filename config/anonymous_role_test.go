package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// miniship: the role a database's reads become is configuration, per
// database, with no default.

func unsetRegistryEnvForTest(t *testing.T) {
	t.Helper()
	for _, prefix := range []string{"", "PREST_"} {
		for _, key := range []string{"DATABASE_ALIAS_", "DATABASE_URL_", "DATABASE_ANON_ROLE_"} {
			unsetEnvForTest(t, prefix+key+"1")
			unsetEnvForTest(t, prefix+key+"2")
		}
	}
	unsetEnvForTest(t, "PREST_PG_ANON_ROLE")
	unsetEnvForTest(t, "PREST_PG_MAXOPENCONN")
	unsetEnvForTest(t, "PREST_PG_MAXIDLECONN")
}

func TestAnonymousRole_fromTheEnvironmentRegistry(t *testing.T) {
	resetViperForTest(t)
	unsetRegistryEnvForTest(t)
	t.Setenv("DATABASE_ALIAS_1", "project")
	t.Setenv("DATABASE_URL_1", "postgres://authenticator:pw@state-store:5432/postgres?sslmode=disable")
	t.Setenv("DATABASE_ANON_ROLE_1", "app_anon")
	t.Setenv("PREST_DATABASE_ALIAS_2", "no-role")
	t.Setenv("PREST_DATABASE_URL_2", "postgres://authenticator:pw@state-store:5432/other?sslmode=disable")
	t.Setenv("PREST_PG_ANON_ROLE", "a_role_for_the_default_database")

	v, _ := viperCfg()
	cfg := &Prest{}
	parseDBConfig(v, cfg)
	parseDatabaseRegistry(v, cfg)

	// The entry that names a role becomes it.
	role, ok := cfg.AnonymousRole("project")
	require.True(t, ok)
	require.Equal(t, "app_anon", role)

	// An entry that names none has none: pg.anon_role is not a default for a
	// registry entry.
	role, ok = cfg.AnonymousRole("no-role")
	require.False(t, ok)
	require.Empty(t, role)

	// And a name rest was not given has none, the physical database name
	// included.
	for _, name := range []string{"postgres", "other", ""} {
		_, ok = cfg.AnonymousRole(name)
		require.False(t, ok, name)
	}
}

func TestAnonymousRole_fromATOMLRegistry(t *testing.T) {
	resetViperForTest(t)
	unsetRegistryEnvForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rest.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[pg]
anon_role = "not_a_default"

[[databases]]
alias = "tenant-a"
url = "postgres://authenticator:pw@a.example:5432/a"
anon_role = "anon_a"

[[databases]]
alias = "tenant-b"
url = "postgres://authenticator:pw@b.example:5432/b"
`), 0o600))
	t.Setenv("PREST_CONF", path)

	v, configPath := viperCfg()
	cfg := &Prest{}
	Parse(v, cfg, configPath)
	parseDatabaseRegistry(v, cfg)

	role, ok := cfg.AnonymousRole("tenant-a")
	require.True(t, ok)
	require.Equal(t, "anon_a", role)
	_, ok = cfg.AnonymousRole("tenant-b")
	require.False(t, ok)
}

func TestAnonymousRole_withNoRegistry(t *testing.T) {
	// The one configured database, and only it, becomes pg.anon_role.
	cfg := &Prest{PGDatabase: "given", PGAnonRole: "app_anon"}
	role, ok := cfg.AnonymousRole("given")
	require.True(t, ok)
	require.Equal(t, "app_anon", role)
	_, ok = cfg.AnonymousRole("other")
	require.False(t, ok)

	// With no pg.anon_role there is no role, and no default stands in.
	cfg.PGAnonRole = ""
	_, ok = cfg.AnonymousRole("given")
	require.False(t, ok)

	var none *Prest
	_, ok = none.AnonymousRole("given")
	require.False(t, ok)
}

func TestAnonymousRole_hasNoDefault(t *testing.T) {
	resetViperForTest(t)
	unsetRegistryEnvForTest(t)
	unsetEnvForTest(t, "PREST_CONF")
	t.Setenv("PREST_CONF", filepath.Join(t.TempDir(), "absent.toml"))

	v, configPath := viperCfg()
	cfg := &Prest{}
	Parse(v, cfg, configPath)
	require.Empty(t, cfg.PGAnonRole)
	_, ok := cfg.AnonymousRole(cfg.PGDatabase)
	require.False(t, ok)
}

func TestParseDatabaseRegistry_anEnvironmentEntryHasABoundedPool(t *testing.T) {
	resetViperForTest(t)
	unsetRegistryEnvForTest(t)
	t.Setenv("DATABASE_ALIAS_1", "project")
	t.Setenv("DATABASE_URL_1", "postgres://authenticator:pw@state-store:5432/postgres")

	// The defaults: upstream left an environment entry at 0 open connections,
	// which database/sql reads as no limit.
	v, _ := viperCfg()
	cfg := &Prest{}
	parseDBConfig(v, cfg)
	parseDatabaseRegistry(v, cfg)
	require.Len(t, cfg.Databases, 1)
	require.Equal(t, 10, cfg.Databases[0].MaxOpenConn)
	require.Equal(t, 0, cfg.Databases[0].MaxIdleConn)

	// And what pg.* says, when it says something.
	resetViperForTest(t)
	t.Setenv("PREST_PG_MAXOPENCONN", "4")
	t.Setenv("PREST_PG_MAXIDLECONN", "2")
	v, _ = viperCfg()
	cfg = &Prest{}
	parseDBConfig(v, cfg)
	parseDatabaseRegistry(v, cfg)
	require.Equal(t, 4, cfg.Databases[0].MaxOpenConn)
	require.Equal(t, 2, cfg.Databases[0].MaxIdleConn)
}

func TestCORS_defaultsAllowReadsOnlyAndNoCredentials(t *testing.T) {
	resetViperForTest(t)
	for _, key := range []string{"PREST_CORS_ALLOWMETHODS", "PREST_CORS_ALLOWCREDENTIALS", "PREST_CORS_ALLOWORIGIN"} {
		unsetEnvForTest(t, key)
	}
	t.Setenv("PREST_CONF", filepath.Join(t.TempDir(), "absent.toml"))

	v, configPath := viperCfg()
	cfg := &Prest{}
	Parse(v, cfg, configPath)
	require.Equal(t, []string{"GET", "HEAD", "OPTIONS"}, cfg.CORSAllowMethods)
	require.False(t, cfg.CORSAllowCredentials)
	require.Equal(t, []string{"*"}, cfg.CORSAllowOrigin)

	// The stack writes the methods as one variable, space-separated.
	resetViperForTest(t)
	t.Setenv("PREST_CORS_ALLOWMETHODS", "GET HEAD OPTIONS")
	v, configPath = viperCfg()
	cfg = &Prest{}
	Parse(v, cfg, configPath)
	require.Equal(t, []string{"GET", "HEAD", "OPTIONS"}, cfg.CORSAllowMethods)
}
