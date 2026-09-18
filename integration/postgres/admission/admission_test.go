package admission_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

// The ticket's first criterion, whole: a third Project, whose Database did not
// exist when rest started, is reachable without a restart — and the two rest
// was given answer throughout, before, during and after.
func TestAdmission_aProjectCreatedWhileRestRunsIsReachableWithoutARestart(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	rest := restOverProjects(t, cfg, answers)

	// Before. The third Project's Database does not exist and nobody has
	// rented it, so rest is told there is no such Project.
	readsItsOwnRow(t, rest, alpha)
	readsItsOwnRow(t, rest, beta)
	status, body := read(t, rest, gamma)
	require.Equal(t, http.StatusNotFound, status, body)

	// The Database is rented and the answerer learns about it, which is the
	// only thing that happens. rest is not restarted, signalled or told.
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	time.Sleep(2 * time.Second) // the window on the refusal above passes

	// During, and after.
	readsItsOwnRow(t, rest, gamma)
	readsItsOwnRow(t, rest, alpha)
	readsItsOwnRow(t, rest, beta)
	readsItsOwnRow(t, rest, gamma)

	// The two Projects rest was given were never asked about: they were
	// given, and a Project rest holds is never looked up.
	require.Zero(t, answers.asksFor(alpha.alias)+answers.asksFor(beta.alias),
		"a Project rest was given was looked up")
}

// A Project's second and later calls cause no lookup: the pool is kept, and
// the answerer is asked once however many rows are read.
func TestAdmission_anAdmittedProjectsLaterCallsAskNothing(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers)

	for range 12 {
		readsItsOwnRow(t, rest, gamma)
	}
	require.Equal(t, 1, answers.asksFor(gamma.alias),
		"twelve reads of one Project were more than one lookup")
}

// A lookup answers for exactly one Project, and the credential it answers with
// opens that Project's Database and no other. Postgres is what refuses it:
// CONNECT on each Database is granted to its own login alone.
func TestAdmission_theCredentialOpensNoOtherProjectsDatabase(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers)

	readsItsOwnRow(t, rest, gamma)
	answer := answers.answerFor(gamma.alias)
	require.Equal(t, gamma.alias, answer.Project, "the lookup answered for another Project")

	// The control: what was answered does open the Database it was answered
	// for, so the refusals below are the grant and not a broken credential.
	mine, err := sql.Open("postgres", answer.URL)
	require.NoError(t, err)
	defer mine.Close()
	require.NoError(t, mine.Ping())

	// And it opens no other Project's.
	for _, other := range []project{alpha, beta} {
		elsewhere := answer.URL
		// The same credential, aimed at another Project's Database.
		elsewhere = replaceDatabase(elsewhere, gamma.database, other.database)
		db, err := sql.Open("postgres", elsewhere)
		require.NoError(t, err)
		err = db.Ping()
		require.Error(t, err, "%s's credential opened %s's Database", gamma.alias, other.alias)
		require.Contains(t, err.Error(), "permission denied for database",
			"%s's credential was refused for the wrong reason", gamma.alias)
		require.NoError(t, db.Close())
	}
}

// With the answerer accepting and never answering, a Project that has a pool
// keeps serving rows, and one that has none is refused inside the bound rest
// set rather than left waiting.
func TestAdmission_withTheAnswererDown_warmProjectsServeRowsAndAColdOneIsRefused(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers, func(c *config.AdmissionConf) {
		c.Timeout = 500 * time.Millisecond
	})

	// gamma is admitted while the answerer still answers; delta never is.
	readsItsOwnRow(t, rest, gamma)

	release := answers.holds()
	defer release()

	// The warm Projects keep answering, with rows, and ask nothing.
	asked := answers.asksFor(gamma.alias)
	for range 4 {
		readsItsOwnRow(t, rest, alpha)
		readsItsOwnRow(t, rest, beta)
		readsItsOwnRow(t, rest, gamma)
	}
	require.Equal(t, asked, answers.asksFor(gamma.alias),
		"a warm Project waited on an answerer that was not answering")

	// A cold one is refused, and inside a bound rest set.
	cold := project{alias: "delta"}
	done := make(chan int, 1)
	go func() {
		status, _ := read(t, rest, cold)
		done <- status
	}()
	select {
	case status := <-done:
		require.Equal(t, http.StatusServiceUnavailable, status)
	case <-time.After(20 * time.Second):
		t.Fatal("a cold Project hung on an answerer that never answered")
	}
}

// The lookup address is internal. An answerer that refuses rest's service
// token admits nothing, and what rest says about it is that it could not find
// out — never that the Project does not exist.
func TestAdmission_anAnswererThatRefusesTheCredentialAdmitsNothing(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	answers.refuses()
	rest := restOverProjects(t, cfg, answers)

	status, body := read(t, rest, gamma)
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.NotContains(t, body, gamma.database)
	require.NotContains(t, body, gamma.login)

	// The control: the same answerer, now accepting what rest proves itself
	// with, admits the Project. So the refusal above was the credential.
	answers.mu.Lock()
	answers.refuse = false
	answers.mu.Unlock()
	time.Sleep(2 * time.Second) // the window on the refusal passes
	readsItsOwnRow(t, rest, gamma)
}

// A credential that was rotated recovers, and it costs one fresh lookup.
func TestAdmission_aRotatedCredentialRecoversAfterOneFreshLookup(t *testing.T) {
	cfg, admin := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers)

	readsItsOwnRow(t, rest, gamma)
	require.Equal(t, 1, answers.asksFor(gamma.alias))

	// The credential is rotated where it is kept, and the answerer knows the
	// new one. rest is holding the old one and has not been told.
	rotated := "gamma-login-rotated"
	exec(t, admin, "ALTER ROLE "+gamma.login+" PASSWORD '"+rotated+"'")
	answers.teach(cfg, gamma, rotated)
	// Postgres leaves an established session alone when a password changes,
	// so the pool must be made to open a new connection for the stale
	// credential to be used at all.
	hangUpOn(t, admin, gamma)

	readsItsOwnRow(t, rest, gamma)
	require.Equal(t, 2, answers.asksFor(gamma.alias),
		"the rotation cost more than one fresh lookup")

	// And afterwards it is a Project like any other: no further lookups.
	for range 5 {
		readsItsOwnRow(t, rest, gamma)
	}
	require.Equal(t, 2, answers.asksFor(gamma.alias))
}

// A credential the answerer cannot fix is one fresh lookup and then a failure,
// never a loop: the second forced lookup inside the window is refused.
func TestAdmission_aCredentialTheAnswererCannotFixIsNotALoop(t *testing.T) {
	cfg, admin := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers, func(c *config.AdmissionConf) {
		c.Window = time.Minute
	})

	readsItsOwnRow(t, rest, gamma)
	require.Equal(t, 1, answers.asksFor(gamma.alias))

	// Rotated out of band, and the answerer is stale too: every lookup from
	// here answers a credential the Database no longer accepts.
	exec(t, admin, "ALTER ROLE "+gamma.login+" PASSWORD 'gamma-login-rotated-behind-everyones-back'")
	hangUpOn(t, admin, gamma)

	for range 6 {
		status, body := read(t, rest, gamma)
		require.NotEqual(t, http.StatusOK, status,
			"a stale credential read rows: %s", body)
	}
	require.Equal(t, 2, answers.asksFor(gamma.alias),
		"six failing reads were six lookups, which is the loop this must not be")
}

// A read already running when a credential is replaced still finishes.
//
// The pool the rotation replaces is the one that read is holding. Closing it at
// the moment of the swap turns a read that would have succeeded into
// "sql: database is closed", so it is closed after a grace instead — rest's own
// statement time limit, by which point a read still in flight has been
// cancelled by its own bound.
func TestAdmission_aReadInFlightSurvivesTheRotationUnderIt(t *testing.T) {
	cfg, admin := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)
	rest := restOverProjects(t, cfg, answers)

	readsItsOwnRow(t, rest, gamma)

	// Reads of gamma, running while the rotation happens under them. They are
	// on the pool the rotation replaces, which is the whole point.
	var wg sync.WaitGroup
	failures := make(chan string, 32)
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				status, body := read(t, rest, gamma)
				// A 200 is the answer, and a 502 is the Database refusing the
				// stale credential or having hung up, which is this rotation's
				// own doing and is what the fresh lookup then fixes. Anything
				// else is rest having broken a read it was holding — a 500
				// saying the role could not be entered in particular, which is
				// what a connection dying under a read used to be reported as.
				if status != http.StatusOK && status != http.StatusBadGateway &&
					status != http.StatusServiceUnavailable {
					failures <- fmt.Sprintf("%d: %s", status, body)
					return
				}
				if strings.Contains(body, "database is closed") {
					failures <- "a read was answered from a pool that had been closed underneath it"
					return
				}
			}
		}()
	}

	rotated := "gamma-login-rotated-under-a-read"
	exec(t, admin, "ALTER ROLE "+gamma.login+" PASSWORD '"+rotated+"'")
	answers.teach(cfg, gamma, rotated)
	hangUpOn(t, admin, gamma)
	time.Sleep(time.Second)
	close(stop)
	wg.Wait()
	close(failures)

	for why := range failures {
		t.Fatalf("a read did not survive the rotation under it: %s", why)
	}

	// And the control: reads are answered again once the fresh credential is
	// in, so the loop above was reading rather than erroring throughout.
	readsItsOwnRow(t, rest, gamma)
}

// The shape the self-host install runs: rest is given no Project at all and
// learns about every one of them by asking. It is the same path the cloud
// runs, which is why the public suite can exercise it.
func TestAdmission_restGivenNoProjectAtAllLearnsAboutThemAll(t *testing.T) {
	cfg, _ := needsPostgres(t)
	answers := newAnswerer(t, cfg)
	createProject(t, cfg, gamma)
	answers.teach(cfg, gamma, gamma.password)

	for _, p := range atStart {
		answers.teach(cfg, p, p.password)
	}
	rest := restGivenNothing(t, cfg, answers)

	for _, p := range projects {
		readsItsOwnRow(t, rest, p)
	}
	for _, p := range projects {
		require.Equal(t, 1, answers.asksFor(p.alias),
			"%s was looked up more than once", p.alias)
	}
}

// replaceDatabase aims a connection URL at another database on the same host.
func replaceDatabase(dsn, from, to string) string {
	return strings.Replace(dsn, "/"+from+"?", "/"+to+"?", 1)
}
