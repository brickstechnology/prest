package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lib/pq"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/logsafe"
)

var _ adapters.CallerReader = (*postgres)(nil)

// The settings a token's claims are handed to the Database under: PostgREST's
// names, which Supabase-written row rules read (auth.uid() reads the first two).
const (
	claimsSetting    = "request.jwt.claims"
	subClaimSetting  = "request.jwt.claim.sub"
	roleClaimSetting = "request.jwt.claim.role"
)

// ReadAsCallerCtx is asRole's transaction with the caller's claims in it: the
// time limit, SET LOCAL ROLE, and then each claim with set_config(…, true), so
// every one ends with the transaction and the pooled connection goes back
// holding none of them. The transaction is read-only and is rolled back.
func (p *postgres) ReadAsCallerCtx(ctx context.Context, caller adapters.Caller, read func(tx *sql.Tx) error) error {
	if caller.Role == "" {
		return adapters.ErrNoAnonymousRole
	}
	db, err := p.dbFromCtx(ctx)
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		slog.Error("log details", "err", logsafe.Error(err))
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if ms := p.cfg.PGStatementTimeoutMS; ms > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			slog.Error("could not set the read's time limit", "err", logsafe.Error(err))
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
	return read(tx)
}

// setClaims hands the token's payload to the transaction. A read with no token
// still sets the role, as PostgREST does for its anonymous role.
func setClaims(ctx context.Context, tx *sql.Tx, caller adapters.Caller) error {
	claims := caller.Claims
	if len(claims) == 0 {
		claims = []byte(`{"role":` + quoteJSON(caller.Role) + `}`)
	}
	var named map[string]any
	if err := json.Unmarshal(claims, &named); err != nil {
		return fmt.Errorf("the token's claims are not a JSON object: %w", err)
	}
	sub, _ := named["sub"].(string)
	_, err := tx.ExecContext(ctx,
		"SELECT set_config($1, $2, true), set_config($3, $4, true), set_config($5, $6, true)",
		claimsSetting, string(claims), subClaimSetting, sub, roleClaimSetting, caller.Role)
	return err
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
