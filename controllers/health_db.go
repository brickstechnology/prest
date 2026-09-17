package controllers

import (
	"context"

	"github.com/prest/prest/v2/adapters"
)

// DefaultCheckList returns the default liveness checks for /_health.
//
// miniship: there are none, so the health answer touches no Database. Upstream
// pinged the default database here, and a platform that probes /_health every
// few seconds would keep a suspended compute awake that nobody is calling.
// The pinger stays in the signature for the call site upstream owns.
func DefaultCheckList(_ adapters.DatabasePinger) CheckList {
	return CheckList{}
}

// DefaultReadyCheckList returns readiness checks for /_ready.
func DefaultReadyCheckList(checker adapters.ReadinessChecker) CheckList {
	return CheckList{
		func(ctx context.Context) error {
			return checker.PingAll(ctx)
		},
	}
}
