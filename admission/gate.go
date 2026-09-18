package admission

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/config"
)

// Pool is how the Gate makes and discards the connection pool for a Project it
// admitted. app supplies both: admission decides when a Project is learned
// about, and has no opinion about which driver holds it.
type Pool struct {
	// Open builds an unconnected adapter for one registry entry. Its pool
	// fills on the first read, as every other Project's does.
	Open func(config.DatabaseConf) (adapters.Adapter, error)
	// Close discards a pool, which is what a credential that was rotated
	// leaves behind.
	Close func(adapters.Adapter)
}

// Gate is the lookup, and the three things in front of it: the registry that
// makes a second call cost nothing, the permit that makes concurrent first
// calls cost one lookup, and the window that makes a burst for a name with no
// Project cost one lookup too.
//
// It holds nothing on disk. Every Project it knows about is in the adapter
// registry it was given, and a restart starts again from none.
type Gate struct {
	answerer Answerer
	registry adapters.Registry
	pool     Pool
	conf     func(Answer) (config.DatabaseConf, error)

	// window is how stale rest's view of one Project may be: how long an
	// answer rest could not admit is kept before it is asked again, and the
	// least time between two forced lookups for one Project.
	window time.Duration
	// max is how many Projects rest keeps one of these for.
	max int
	// now is the clock, replaced in tests.
	now func() time.Time

	mu      sync.Mutex
	known   map[string]*entry
	lookups int64
}

// entry is what rest remembers about one Project between calls. It is three
// timestamps and an error: no address, no credential and no row.
type entry struct {
	// permit is the Project's own, holding one lookup at a time. Concurrent
	// first calls wait on it and find the Project admitted.
	permit chan struct{}
	// lookedUp is when the last lookup for this Project returned, and err is
	// what it returned when rest could not admit the Project.
	lookedUp time.Time
	err      error
	// refreshed is when the last *forced* lookup returned. It is separate
	// from lookedUp because a rotated credential is refreshed straight after
	// a successful admission, and must not be refused for being too soon
	// after it.
	refreshed time.Time
	// admitted says the Project has a pool, so this entry is not evicted:
	// losing it would lose the floor under forced lookups.
	admitted bool
	// inFlight says a lookup is running on this entry, so it is not evicted
	// out from under the goroutine holding its permit.
	inFlight bool
}

// NewGate builds the gate. answerer is the process holding the Database
// plugin, registry is where an admitted Project is kept, and pool is how its
// connections are opened and discarded.
func NewGate(answerer Answerer, registry adapters.Registry, pool Pool, conf config.AdmissionConf, base *config.Prest) *Gate {
	return &Gate{
		answerer: answerer,
		registry: registry,
		pool:     pool,
		conf: func(a Answer) (config.DatabaseConf, error) {
			return base.AdmittedDatabaseConf(a.Project, a.URL, a.AnonRole)
		},
		window: conf.Window,
		max:    conf.MaxProjects,
		now:    time.Now,
		known:  map[string]*entry{},
	}
}

// Admit answers with the adapter for project, looking it up when rest has not
// seen it. A Project already admitted costs no lookup at all.
func (g *Gate) Admit(ctx context.Context, project string) (adapters.Adapter, error) {
	return g.admit(ctx, project, false)
}

// Readmit looks project up again although rest already has a pool for it,
// which is what a credential that was rotated needs. It is refused with
// ErrTooSoon inside the window, so an authentication failure that the answerer
// cannot fix is one fresh lookup and not a loop.
func (g *Gate) Readmit(ctx context.Context, project string) (adapters.Adapter, error) {
	return g.admit(ctx, project, true)
}

// Lookups is how many times the answerer has been asked. It is for rest's own
// tests and its operator's log line, and nothing serves from it.
func (g *Gate) Lookups() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookups
}

func (g *Gate) admit(ctx context.Context, project string, force bool) (adapters.Adapter, error) {
	e := g.entryFor(project)

	// The Project's own permit. A caller whose own deadline passes while it
	// waits gives up here rather than queueing behind an answerer that is not
	// answering: the refusal is bounded whether rest is waiting on the
	// answerer or on the call in front of it.
	select {
	case e.permit <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrUnavailable
	}
	defer func() { <-e.permit }()

	// Rechecked now the permit is held, as Neon's proxy rechecks its cache
	// after acquiring its own: the call this one queued behind has usually
	// admitted the Project already.
	if !force {
		if adapter, err := g.registry.Get(project); err == nil {
			return adapter, nil
		}
	}
	if err := g.kept(e, force); err != nil {
		return nil, err
	}

	g.begin(e)
	answer, err := g.answerer.Lookup(ctx, project)
	if err != nil {
		g.finished(e, force, err)
		slog.Warn("a Project was not admitted", "project", project, "err", err.Error())
		return nil, err
	}

	adapter, err := g.open(project, answer, force)
	g.finished(e, force, err)
	if err != nil {
		slog.Warn("a Project was not admitted", "project", project, "err", err.Error())
		return nil, err
	}
	slog.Info("a Project was admitted while rest ran", "project", project)
	return adapter, nil
}

// kept is the answer rest already has for this Project, when it is inside the
// window: the refusal it was given, or, for a forced lookup, the refusal to
// ask again so soon.
func (g *Gate) kept(e *entry, force bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if force {
		if !e.refreshed.IsZero() && now.Sub(e.refreshed) < g.window {
			return ErrTooSoon
		}
		return nil
	}
	if e.err != nil && now.Sub(e.lookedUp) < g.window {
		return e.err
	}
	return nil
}

func (g *Gate) begin(e *entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e.inFlight = true
	g.lookups++
}

func (g *Gate) finished(e *entry, force bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	e.inFlight = false
	e.lookedUp = now
	e.err = err
	if force {
		e.refreshed = now
	}
	if err == nil {
		e.admitted = true
	}
}

// open turns an answer into the Project's pool and keeps it. A Project being
// admitted again — a credential that was rotated — has its old pool discarded,
// so the connections opened with the credential that no longer works go.
func (g *Gate) open(project string, answer Answer, force bool) (adapters.Adapter, error) {
	conf, err := g.conf(answer)
	if err != nil {
		return nil, err
	}
	adapter, err := g.pool.Open(conf)
	if err != nil {
		return nil, err
	}
	var stale adapters.Adapter
	if force {
		stale, _ = g.registry.Get(project)
	}
	if err := g.registry.Register(project, adapter); err != nil {
		return nil, err
	}
	if stale != nil && g.pool.Close != nil {
		g.pool.Close(stale)
	}
	return adapter, nil
}

// entryFor is this Project's entry, made when rest first hears the name.
//
// The map is bounded, as Neon's own per-endpoint cache is at 4,000 entries
// (NEON-AS-AN-ENGINE.md §7.2, proxy/src/config.rs L119): a flood of names that
// have no Project must not be a flood of memory either. What is dropped when
// it is full is an entry that has expired, and failing that the oldest one
// that is neither admitted nor being looked up — so a Project with a pool
// keeps its floor, and a lookup in flight keeps the permit its caller holds.
func (g *Gate) entryFor(project string) *entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.known[project]; ok {
		return e
	}
	if g.max > 0 && len(g.known) >= g.max {
		g.evict()
	}
	e := &entry{permit: make(chan struct{}, 1)}
	g.known[project] = e
	return e
}

// evict runs with g.mu held.
func (g *Gate) evict() {
	now := g.now()
	var oldestName string
	var oldest time.Time
	for name, e := range g.known {
		if e.admitted || e.inFlight {
			continue
		}
		if now.Sub(e.lookedUp) >= g.window {
			delete(g.known, name)
			continue
		}
		if oldestName == "" || e.lookedUp.Before(oldest) {
			oldestName, oldest = name, e.lookedUp
		}
	}
	if g.max > 0 && len(g.known) >= g.max && oldestName != "" {
		delete(g.known, oldestName)
	}
}
