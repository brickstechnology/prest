package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lib/pq"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/scanner"
	"github.com/prest/prest/v2/internal/logsafe"
)

// miniship: the read half of rest, run as the database's anonymous role.
// adapters/anonymous_role.go says why.

var (
	_ adapters.AnonymousRoles = (*postgres)(nil)
	_ adapters.RoleReader     = (*postgres)(nil)
)

// AnonymousRole returns the role configured for alias.
func (p *postgres) AnonymousRole(alias string) (string, bool) {
	return p.cfg.AnonymousRole(alias)
}

// QueryAsRoleCtx is QueryCtx, run inside a read-only transaction that has
// become role.
func (p *postgres) QueryAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) adapters.Scanner {
	SQL = fmt.Sprintf("SELECT %s(s) FROM (%s) s", p.cfg.JSONAggType, SQL)
	return p.asRole(ctx, role, func(stmt *sql.Stmt) adapters.Scanner {
		var jsonData []byte
		err := stmt.QueryRowContext(ctx, params...).Scan(&jsonData)
		if len(jsonData) == 0 {
			jsonData = []byte("[]")
		}
		return &scanner.PrestScanner{Error: err, Buff: bytes.NewBuffer(jsonData), IsQuery: true}
	}, SQL)
}

// QueryCountAsRoleCtx is QueryCountCtx, run inside a read-only transaction
// that has become role.
func (p *postgres) QueryCountAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) adapters.Scanner {
	return p.asRole(ctx, role, func(stmt *sql.Stmt) adapters.Scanner {
		var result struct {
			Count int64 `json:"count"`
		}
		if err := stmt.QueryRowContext(ctx, params...).Scan(&result.Count); err != nil {
			return &scanner.PrestScanner{Error: err}
		}
		byt, err := json.Marshal(result)
		return &scanner.PrestScanner{Error: err, Buff: bytes.NewBuffer(byt)}
	}, SQL)
}

// asRole opens a read-only transaction on the database named in ctx, becomes
// role with SET LOCAL ROLE, prepares SQL in that transaction and hands the
// statement to read. The transaction is rolled back afterwards, which also
// ends the role: the pooled connection goes back as the login, holding nothing.
//
// Every failure before read returns an error and reads nothing. In particular
// a role that cannot be entered is ErrRoleNotEntered, and the statement is
// never run as the login instead.
//
// The statement is prepared on the transaction, not taken from the pool-wide
// statement cache, so it runs on the connection that became the role. Preparing
// also keeps it on the extended protocol, which carries one statement and no
// more.
func (p *postgres) asRole(ctx context.Context, role string, read func(*sql.Stmt) adapters.Scanner, SQL string) adapters.Scanner {
	if role == "" {
		return &scanner.PrestScanner{Error: adapters.ErrNoAnonymousRole}
	}
	db, err := p.dbFromCtx(ctx)
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return &scanner.PrestScanner{Error: err}
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return &scanner.PrestScanner{Error: err}
	}
	// A read commits nothing, so every path ends in a rollback.
	defer func() { _ = tx.Rollback() }()

	// miniship (#549): the time limit, set on the transaction and ending with
	// it, so the pooled connection goes back without it. It is set as the
	// login, before the role is entered, and the number is rest's own
	// configuration rather than anything a caller sent.
	if ms := p.cfg.PGStatementTimeoutMS; ms > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			slog.Error("could not set the read's time limit", "err", logsafe.Error(err))
			return &scanner.PrestScanner{Error: err}
		}
	}

	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+pq.QuoteIdentifier(role)); err != nil {
		slog.Error("could not become the anonymous role", "role", role, "err", logsafe.Error(err))
		// miniship (#548): only when Postgres itself refused the role. A
		// statement can also fail here because the connection went away
		// underneath it — a Database restarting, a session terminated, a
		// credential replaced — and reporting that as a role failure sends an
		// operator to read the grants, which is the wrong half of the system.
		// #549's readFailure knows what a dead connection is; it only needs to
		// be given the error rather than this one.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			return &scanner.PrestScanner{Error: fmt.Errorf("%w: %s", adapters.ErrRoleNotEntered, role)}
		}
		return &scanner.PrestScanner{Error: err}
	}

	stmt, err := p.PrepareTxContext(ctx, tx, SQL)
	if err != nil {
		slog.Error("log details", "err", err)
		return &scanner.PrestScanner{Error: err}
	}
	defer func() { _ = stmt.Close() }()
	return read(stmt)
}
