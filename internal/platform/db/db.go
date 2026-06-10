// Package db holds the two distinct database paths:
//
//   - Smoke (this file): a one-shot bare-connection SELECT 1 used only by the
//     deploy `-smoke` flow to prove the service can reach Neon. Unchanged since
//     T0: it opens a single bare pgx connection and closes it immediately. It
//     does NOT use the connection pool.
//   - NewPool / DBTX (pool.go): the service-runtime pooled path (T1+) for HTTP
//     request handling.
//
// Smoke is deliberately NOT called by /livez, which stays a pure,
// dependency-free liveness signal.
package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Smoke opens a single bare connection to dsn, runs SELECT 1, and closes it.
// It returns the first error encountered, or nil if the round trip succeeds.
//
// This is the bare-connection path (deploy `-smoke` only) and is intentionally
// kept separate from the runtime connection pool (NewPool); do not route the
// pool through here or vice versa.
func Smoke(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}
