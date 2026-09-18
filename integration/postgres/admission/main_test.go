// Package admission_test drives rest, in-process, against a live Postgres, and
// creates a Project's Database while the process runs (miniship-cloud#548).
//
// ADR 0017's amendment decided that rest learns about a Project by asking on a
// miss. What that buys is the thing this package asks for: a Project that did
// not exist when rest started is reachable, without a restart, while the
// Projects rest was given keep answering. What it costs is the thing this
// package also asks for: a Project with no pool cannot be served while the
// answerer is down.
//
// Three Databases, three logins and three anonymous roles — one set per
// Project, so the credential a lookup answers with can be tried against
// another Project's Database and refused. A shared login could not show that.
//
// Every subject here needs that Postgres, and none of them skips without one:
// a subject that cannot reach it fails. TestMain prints one line saying how
// many subjects needed a live Postgres and how many got one, and fails the run
// when the two differ, or when a whole run asked for none, so a green here
// always means the database answered.
package admission_test

import (
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/prest/prest/v2/integration/helpers"
)

var (
	needed atomic.Int32
	got    atomic.Int32
)

func TestMain(m *testing.M) {
	flag.Parse()
	helpers.EnsureTestConfigEnv()
	code := m.Run()
	dropProjects()

	n, g := needed.Load(), got.Load()
	fmt.Printf("rest: %d subjects needed a live Postgres, %d got one\n", n, g)
	filtered := false
	if run := flag.Lookup("test.run"); run != nil && run.Value.String() != "" {
		filtered = true
	}
	if g != n || (n == 0 && !filtered) {
		fmt.Println("rest: a live-Postgres subject did not get its database, so this run is not a pass")
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
