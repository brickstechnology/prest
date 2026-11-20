package controllers

import (
	"context"

	"github.com/prest/prest/v2/adapters"
)

// DefaultCheckList returns the default checks for /_health, which is a
// liveness probe. It runs no database check, so a database outage or a
// sleeping database does not fail liveness. /_ready checks the databases, see
// DefaultReadyCheckList. The pinger is kept so callers still compile; add
// CheckDBHealth(pinger) to a list to make /_health check the database.
func DefaultCheckList(pinger adapters.DatabasePinger) CheckList {
	return CheckList{}
}

// CheckDBHealth verifies the default database connection is alive.
func CheckDBHealth(pinger adapters.DatabasePinger) HealthCheckFunc {
	return func(ctx context.Context) error {
		return pinger.Ping(ctx)
	}
}

// DefaultReadyCheckList returns readiness checks for /_ready, which pings
// every configured database.
func DefaultReadyCheckList(checker adapters.ReadinessChecker) CheckList {
	return CheckList{
		func(ctx context.Context) error {
			return checker.PingAll(ctx)
		},
	}
}
