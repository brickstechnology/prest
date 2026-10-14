package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lib/pq"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/logsafe"
)

var _ adapters.CallerWriter = (*postgres)(nil)

// WriteAsCallerCtx is ReadAsCallerCtx's transaction without the read-only
// option, and committed: the time limit, the claims, SET LOCAL ROLE, and then
// the write. The role and the claims end with the transaction, so the pooled
// connection goes back holding neither. Nothing is committed unless write
// returns nil.
func (p *postgres) WriteAsCallerCtx(ctx context.Context, caller adapters.Caller, write func(tx *sql.Tx) error) error {
	if caller.Role == "" {
		return adapters.ErrNoAnonymousRole
	}
	db, err := p.dbFromCtx(ctx)
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if ms := p.cfg.PGStatementTimeoutMS; ms > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			slog.Error("could not set the write's time limit", "err", logsafe.Error(err))
			return err
		}
	}
	if err := setClaims(ctx, tx, caller); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+pq.QuoteIdentifier(caller.Role)); err != nil {
		slog.Error("could not become the caller's role", "role", caller.Role, "err", logsafe.Error(err))
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			return fmt.Errorf("%w: %s", adapters.ErrRoleNotEntered, caller.Role)
		}
		return err
	}
	if err := write(tx); err != nil {
		if errors.Is(err, adapters.ErrRollback) {
			return nil
		}
		return err
	}
	return tx.Commit()
}
