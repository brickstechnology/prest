// Package twoprojects_test drives rest, in-process, against a live Postgres
// carrying three Databases, and asks it which of them a call reaches
// (miniship-cloud#547).
//
// One process serving two Databases is the whole claim of ADR 0017, and a test
// that only ever uses one cannot see it. The third Database is here to be
// ignored: nothing ever calls it, and the run proves nothing ever reached it.
//
// Every subject here needs that Postgres, and none of them skips without one:
// a subject that cannot reach it fails. TestMain prints one line saying how
// many subjects needed a live Postgres and how many got one, and fails the run
// when the two differ, or when a whole run asked for none, so a green here
// always means the database answered.
package twoprojects_test

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
