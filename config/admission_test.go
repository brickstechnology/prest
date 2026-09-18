package config

// The lookup's own settings, and the registry entry a Project admitted while
// rest runs becomes (miniship-cloud#548).

import (
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

// A Project admitted while rest runs becomes exactly the entry it would have
// been given at start-up: the same fields off the same URL, and the same pool
// limits. One code path serves a Project rest started with and one it learned
// about at ten o'clock.
func TestAdmittedDatabaseConf_isTheEntryTheEnvironmentWouldHaveMade(t *testing.T) {
	cfg := &Prest{PGMaxOpenConn: 7, PGMaxIdleConn: 3}
	url := "postgres://authenticator:a-password@db.example:6543/tenant?sslmode=require"

	admitted, err := cfg.AdmittedDatabaseConf("prj_0123456789abcdef", url, "app_anon")
	require.NoError(t, err)

	// What DATABASE_ALIAS_1, DATABASE_URL_1 and DATABASE_ANON_ROLE_1 make of
	// the same three values.
	t.Setenv("DATABASE_ALIAS_1", "prj_0123456789abcdef")
	t.Setenv("DATABASE_URL_1", url)
	t.Setenv("DATABASE_ANON_ROLE_1", "app_anon")
	given := &Prest{PGMaxOpenConn: 7, PGMaxIdleConn: 3}
	parseDatabaseRegistry(viper.New(), given)
	require.Len(t, given.Databases, 1)

	require.Equal(t, given.Databases[0], admitted,
		"a Project admitted while rest runs is not the entry it would have been given")
}

// The alias arrived in the first segment of somebody's URL, so it is held to
// the rule a configured one is held to, and an answer missing either half of
// what a read needs is refused rather than served.
func TestAdmittedDatabaseConf_refusesWhatItCannotServe(t *testing.T) {
	cfg := &Prest{}
	url := "postgres://authenticator:a-password@db.example:6543/tenant?sslmode=require"

	for what, call := range map[string]func() error{
		"a name that is not a path segment": func() error {
			_, err := cfg.AdmittedDatabaseConf("../../etc", url, "app_anon")
			return err
		},
		"no name at all": func() error {
			_, err := cfg.AdmittedDatabaseConf("", url, "app_anon")
			return err
		},
		"no address": func() error {
			_, err := cfg.AdmittedDatabaseConf("prj_a", "", "app_anon")
			return err
		},
		"no anonymous role": func() error {
			_, err := cfg.AdmittedDatabaseConf("prj_a", url, "")
			return err
		},
	} {
		require.Error(t, call(), "admitted a Project with %s", what)
	}
}

// The three bounds are always set, so a rest given only a lookup address still
// has a timeout, a window and a ceiling.
func TestAdmissionConf_isBoundedEvenWhenOnlyAnAddressIsGiven(t *testing.T) {
	conf := AdmissionConf{URL: "http://api:4000/database"}.WithDefaults()
	require.Equal(t, DefaultAdmissionTimeout, conf.Timeout)
	require.Equal(t, DefaultAdmissionWindow, conf.Window)
	require.Equal(t, DefaultAdmissionMaxProjects, conf.MaxProjects)

	// And what an operator set is kept.
	tuned := AdmissionConf{URL: "http://api:4000/database", Timeout: time.Second}.WithDefaults()
	require.Equal(t, time.Second, tuned.Timeout)
}

// Both spellings are read, as the database registry's are. The install's
// compose file writes the unprefixed one beside DATABASE_URL_<n>, and one
// service block should not mix two prefixes.
func TestParseAdmission_readsBothSpellings(t *testing.T) {
	v := viper.New()
	v.SetDefault("admission.url", "")
	v.SetDefault("admission.key", "")

	t.Setenv("ADMISSION_URL", "http://api:4000/database")
	t.Setenv("ADMISSION_KEY", "a-derived-key")
	unprefixed := &Prest{}
	parseAdmission(v, unprefixed)
	require.Equal(t, "http://api:4000/database", unprefixed.Admission.URL)
	require.Equal(t, "a-derived-key", unprefixed.Admission.Key)

	t.Setenv("ADMISSION_URL", "")
	t.Setenv("ADMISSION_KEY", "")
	t.Setenv("PREST_ADMISSION_URL", "http://api:4000/database")
	t.Setenv("PREST_ADMISSION_KEY", "a-derived-key")
	prefixed := &Prest{}
	parseAdmission(v, prefixed)
	require.Equal(t, "http://api:4000/database", prefixed.Admission.URL)
	require.Equal(t, "a-derived-key", prefixed.Admission.Key)
}

// No address is rest without a lookup at all, which is the shape #547 shipped:
// the Databases it was given, and a name that is not one of them refused.
func TestParseAdmission_noAddressIsRestWithoutALookup(t *testing.T) {
	cfg := &Prest{}
	parseAdmission(viper.New(), cfg)
	require.Empty(t, cfg.Admission.URL)
}
