package config

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/prest/prest/v2/internal/ident"
	"github.com/spf13/viper"
)

// AdmissionConf is where rest asks about a Project it was not given, and what
// it will wait and remember while it asks (miniship-cloud#548).
//
// URL empty is rest without a lookup at all: the Projects it was given at
// start-up, and a 404 for every other name. That is the shape #546 and #547
// shipped, and every test of theirs still describes it.
type AdmissionConf struct {
	// URL is the address of the process holding the Database plugin. It is
	// inside the install and is not reachable through the gateway.
	URL string `mapstructure:"url"`
	// Key is the derived key for the lookup route, base64url, which rest
	// mints a short-lived service token with on every call. It is never the
	// install's root secret, and it is never sent.
	Key string `mapstructure:"key"`
	// Timeout is how long rest waits for one answer.
	Timeout time.Duration `mapstructure:"timeout"`
	// Window is how stale rest's view of one Project may be: how long an
	// answer it could not admit is kept, and the least time between two
	// forced lookups for one Project.
	Window time.Duration `mapstructure:"window"`
	// MaxProjects is how many Projects rest keeps one of those for.
	MaxProjects int `mapstructure:"max_projects"`
}

// The three figures, and where each was taken from. None of them was picked
// for feeling about right, and each says below where it came from.
const (
	// DefaultAdmissionTimeout is one sixth of the 30s a read itself is given
	// (#549), so a Project that cannot be admitted is refused well before a
	// read that is merely slow, and a caller gets an answer rather than a
	// socket that never closes.
	DefaultAdmissionTimeout = 5 * time.Second
	// DefaultAdmissionWindow is the 5 seconds the edge already accepts for
	// its own view of the set of Projects — ADR 0017 names its
	// /routing/table poll at that interval, weighing it as the alternative
	// to this lookup. Neon's four minutes is not the figure to copy: that is
	// a *positive* cache, guessing at a five-minute suspend policy, and a
	// negative one of that length would leave a Project created a moment ago
	// unreachable for four minutes.
	DefaultAdmissionWindow = 5 * time.Second
	// DefaultAdmissionMaxProjects is Neon's own per-endpoint cache size,
	// 4,000 (NEON-AS-AN-ENGINE.md §7.2, proxy/src/config.rs L119), taken
	// unchanged: it bounds what a flood of names that have no Project costs
	// in memory.
	DefaultAdmissionMaxProjects = 4000
)

// parseAdmission reads the lookup's settings from the configuration file and
// the environment.
//
// Both spellings of each name are read, as the database registry's are:
// ADMISSION_URL beside PREST_ADMISSION_URL. The unprefixed one is what the
// install's compose file writes, because that file spells rest's Databases
// DATABASE_URL_<n> too, and one service block should not mix two prefixes.
func parseAdmission(v *viper.Viper, cfg *Prest) {
	cfg.Admission = AdmissionConf{
		URL:         v.GetString("admission.url"),
		Key:         v.GetString("admission.key"),
		Timeout:     v.GetDuration("admission.timeout"),
		Window:      v.GetDuration("admission.window"),
		MaxProjects: v.GetInt("admission.max_projects"),
	}
	if url := envFirst("ADMISSION_URL", "PREST_ADMISSION_URL"); url != "" {
		cfg.Admission.URL = url
	}
	if key := envFirst("ADMISSION_KEY", "PREST_ADMISSION_KEY"); key != "" {
		cfg.Admission.Key = key
	}
	// All five, and not only the two an install has to write. A compose file
	// that says ADMISSION_URL and ADMISSION_KEY and then finds ADMISSION_WINDOW
	// silently ignored is a trap, and it caught this fork's own test first.
	if timeout := envDuration("ADMISSION_TIMEOUT", "PREST_ADMISSION_TIMEOUT"); timeout > 0 {
		cfg.Admission.Timeout = timeout
	}
	if window := envDuration("ADMISSION_WINDOW", "PREST_ADMISSION_WINDOW"); window > 0 {
		cfg.Admission.Window = window
	}
	if max := envInt("ADMISSION_MAX_PROJECTS", "PREST_ADMISSION_MAX_PROJECTS"); max > 0 {
		cfg.Admission.MaxProjects = max
	}
	cfg.Admission = cfg.Admission.WithDefaults()
}

// WithDefaults fills the three figures a configuration left out, so a rest
// given only a lookup address is still bounded in all three ways — including
// one composed in a test or by a program rather than read from a file.
func (c AdmissionConf) WithDefaults() AdmissionConf {
	if c.Timeout <= 0 {
		c.Timeout = DefaultAdmissionTimeout
	}
	if c.Window <= 0 {
		c.Window = DefaultAdmissionWindow
	}
	if c.MaxProjects <= 0 {
		c.MaxProjects = DefaultAdmissionMaxProjects
	}
	return c
}

// envDuration is the first of keys that is set and parses as a duration.
// Anything else is left to the default, with a line saying so: a window
// somebody spelled wrong should not silently become five seconds without
// saying it was ignored.
func envDuration(keys ...string) time.Duration {
	raw := envFirst(keys...)
	if raw == "" {
		return 0
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("admission setting ignored: not a duration", "keys", keys, "value", raw)
		return 0
	}
	return value
}

// envInt is envDuration for a count.
func envInt(keys ...string) int {
	raw := envFirst(keys...)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("admission setting ignored: not a number", "keys", keys, "value", raw)
		return 0
	}
	return value
}

// AdmittedDatabaseConf is the registry entry a Project admitted while rest
// runs becomes: exactly the entry DATABASE_ALIAS_<n>, DATABASE_URL_<n> and
// DATABASE_ANON_ROLE_<n> would have produced for it at start-up, pool limits
// included. A Project learned about at ten o'clock is served by the same code
// as one rest was started with.
//
// The alias is held to the same rule a configured one is: a name that is not a
// safe path segment is refused rather than admitted, because it arrived from
// the first segment of somebody's URL.
func (p *Prest) AdmittedDatabaseConf(alias, connURL, anonRole string) (DatabaseConf, error) {
	switch {
	case alias == "" || !ident.IsSafeSegment(alias):
		return DatabaseConf{}, fmt.Errorf("admitted database alias is not a name: %q", alias)
	case connURL == "":
		return DatabaseConf{}, fmt.Errorf("admitted database %q has no address", alias)
	case anonRole == "":
		return DatabaseConf{}, fmt.Errorf("admitted database %q names no anonymous role", alias)
	}
	conf := DatabaseConf{Alias: alias, URL: connURL, AnonRole: anonRole}
	applyURLToDatabaseConf(&conf)
	fillPoolDefaults(&conf, p)
	return conf, nil
}
