package postgres

import "github.com/prest/prest/v2/adapters/postgres/internal/connection"

// ApplicationName is what rest's connections call themselves, so that a
// Database can tell which of the sessions in its pg_stat_activity are rest's
// and which belong to something else (miniship-cloud#547).
//
// The name is set where a connection is opened, in internal/connection, which
// nothing outside this package may import. This is the same constant, where a
// caller asking a Database about rest can read it.
const ApplicationName = connection.ApplicationName
