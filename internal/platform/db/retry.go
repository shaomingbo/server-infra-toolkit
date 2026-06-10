package db

// retry.go adds the FR10 "double scale-to-zero first-request reconnect retry"
// on top of the runtime pool. It introduces a *Pool wrapper that implements the
// same DBTX surface as *pgxpool.Pool, so callers see no signature change
// (FR3/FR10: the retry is an implementation detail, never on the interface and
// never pushed down into business handlers).
//
// WHY THIS EXISTS
// ---------------
// When BOTH tiers scale to zero (Cloud Run instances -> 0 AND Neon compute -> 0)
// the first request after an idle period must wake Neon before any query can
// run. The very first connection acquire from the lazy pool therefore dials a
// compute that is still spinning up and may briefly refuse / reset the
// connection. That is a TRANSIENT, pre-send failure (no query bytes were sent),
// so a short bounded retry turns the first request from a 5xx into a success.
//
// SCOPE (deliberately narrow):
//   - We ONLY retry connection-level failures that pgx tells us happened before
//     any data was sent (see isTransientConnError). A server-reported error
//     (*pgconn.PgError, e.g. a constraint violation or syntax error) is a real
//     application error and is NEVER retried — re-running it could double-apply a
//     non-idempotent statement.
//   - The retry is bounded (attempt cap + backoff + total budget). The budget is
//     sized to comfortably exceed Neon's wake-up latency yet stay two orders of
//     magnitude under the Cloud Run request timeout (see the const block below).
//
// This retry path is invoked only on the request/business path (Exec/Query/
// QueryRow/Begin). It is NEVER on /livez or the startup path (AC11): main.go
// builds the pool lazily and nothing pings it at startup, and /livez carries no
// db dependency at all.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retry budget constants. These are documented, grounded values — not magic
// numbers (NFR7). They were chosen against the CURRENT official docs:
//
//   - Neon Scale to Zero: a compute suspends after 5 minutes of inactivity and,
//     once queried again, "reactivates automatically within a few hundred
//     milliseconds" (the docs document only this typical figure; there is no
//     published worst-case ceiling, and field reports of seconds-long wakes after
//     long idle remain UNCONFIRMED).
//     Source (verified 2026-06): https://neon.com/docs/introduction/scale-to-zero
//   - HTTP WriteTimeout: cmd/api/main.go sets the server WriteTimeout to 15s.
//     The whole retry window must finish with response-write headroom to spare,
//     or the HTTP layer cuts the connection before the retry can succeed.
//   - Cloud Run request timeout: default 300 seconds (5 minutes), the outer
//     ceiling a request handler may run before Cloud Run aborts it.
//     Source (verified 2026-06):
//     https://docs.cloud.google.com/run/docs/configuring/request-timeout
//
// Constraint (FR10): Neon wake latency  <  totalBudget  <  WriteTimeout (15s)  <<  Cloud Run 300s.
const (
	// maxAttempts is the total number of tries (1 initial + 2 retries). Bounded
	// so a genuinely-down Neon does not spin: after the cap we surface the error
	// and the HTTP layer returns a 5xx via WriteError (FR10), never a panic.
	maxAttempts = 3

	// baseBackoff is the first inter-attempt sleep; it doubles each retry
	// (100ms, then 200ms). Short on purpose: Neon wakes in milliseconds, so a
	// long backoff would only waste the request's latency budget.
	baseBackoff = 100 * time.Millisecond
)

// totalBudget caps the wall-clock time the WHOLE retry loop may consume,
// including each attempt's own fn execution (retry derives a context with this
// timeout and passes it to fn — see retry()), not just the inter-attempt
// backoff. It must be:
//   - LARGER than Neon's documented few-hundred-ms wake-up, with generous margin
//     for the UNCONFIRMED seconds-long cold starts after a long idle, so a real
//     wake-up retries to success rather than timing out; and
//   - SMALLER than the 15s HTTP WriteTimeout (main.go), leaving ~5s for the
//     handler to actually write the response after the DB call returns, so the
//     budget — not the transport — governs the slow first request.
//
// 10s satisfies both: 10s > a few seconds of cold start, and 15s - 10s = 5s of
// response headroom under WriteTimeout. The true worst-case wake-up is
// UNCONFIRMED, so this is a conservative pick to be re-tuned after the deferred
// on-line scale-to-zero verification (NFR8).
//
// It is a var (not a const) only so unit tests can shrink it to keep the
// budget-binding regression test fast; production never reassigns it.
var totalBudget = 10 * time.Second

// retryLogger emits the FR10/AC16 reconnect/retry observability via the standard
// library log/slog JSON handler to stdout — the SAME sink and format as the T0
// access log (internal/platform/log also writes slog JSON to os.Stdout). The db
// package must NOT import internal/http or internal/platform/log to keep the
// dependency direction clean (NFR2/NFR3), so it constructs its own JSON handler
// over the same stdout stream rather than reusing that package.
var retryLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// Pool wraps *pgxpool.Pool and adds bounded transient-reconnect retry to the
// request-path methods. It implements DBTX, so *Pool is a drop-in for the bare
// *pgxpool.Pool everywhere the narrow interface is consumed (FR3/FR10).
type Pool struct {
	pool *pgxpool.Pool
}

// Compile-time assertion that *Pool implements DBTX. If a future pgx upgrade
// changes a signature this breaks the build instead of drifting silently.
var _ DBTX = (*Pool)(nil)

// NewRetryPool builds the lazy pgxpool (via NewPool) and wraps it with the FR10
// retry layer. Construction stays lazy: NewPool does not dial Neon, so startup
// never blocks on the database waking (AC10/AC11). dsn is never logged.
func NewRetryPool(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Pool{pool: pool}, nil
}

// Close releases all pool resources. main.go calls this only after the HTTP
// server has drained in-flight requests (FR9/E6).
func (p *Pool) Close() { p.pool.Close() }

// Stat exposes the underlying pgxpool statistics for future observability
// (NFR4). It is a pass-through; no retry semantics apply.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// Exec runs the statement with bounded transient-reconnect retry. An Exec that
// fails at connection acquire (cold start) is safe to re-run because no query
// bytes were sent; an Exec that fails with a server error is returned as-is.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := retry(ctx, "exec", func(ctx context.Context) error {
		var e error
		tag, e = p.pool.Exec(ctx, sql, args...)
		return e
	})
	return tag, err
}

// Query runs the query with bounded transient-reconnect retry. pgxpool.Query
// surfaces a connection-acquire failure as the error returned here (the rows are
// returned in an error state), so a cold-start acquire failure is detected and
// retried before any rows are consumed. A failure after the connection is
// established (a server error) is not a transient connect error and is returned.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	var rows pgx.Rows
	err := retry(ctx, "query", func(ctx context.Context) error {
		var e error
		rows, e = p.pool.Query(ctx, sql, args...)
		return e
	})
	return rows, err
}

// QueryRow runs the single-row query with the SAME bounded transient-reconnect
// retry as Query. pgxpool.QueryRow itself defers its acquire/connect error to
// Scan, so we cannot use it directly under retry. Instead we run the retrying
// Query (whose error — including a cold-start acquire failure — IS observable and
// retried) and return a retryRow that turns the resulting Rows back into a single
// Row with the standard pgx QueryRow semantics. This makes the QueryRow path go
// through the full retry loop atomically (one connection per attempt), rather
// than the old non-atomic probe-acquire-then-release approach (which could have
// its warmed connection stolen by a concurrent request and only surfaced a real
// acquire failure at Scan).
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := p.Query(ctx, sql, args...)
	return &retryRow{rows: rows, err: err}
}

// retryRow adapts a (pgx.Rows, error) pair from the retrying Query into pgx.Row.
// Its Scan reproduces the semantics of pgx's own connRow.Scan (verified against
// pgx/v5@v5.10.0 rows.go connRow.Scan): if the query already errored it returns
// that error; if no row is available it returns pgx.ErrNoRows; otherwise it scans
// the first row, ignores any additional rows, and always closes the Rows.
type retryRow struct {
	rows pgx.Rows
	err  error
}

func (r *retryRow) Scan(dest ...any) error {
	// The retrying Query failed (e.g. transient retries exhausted, or a real
	// server error): surface it. rows may still be a non-nil errRows; closing it
	// is safe and a no-op, but err is the authoritative result.
	if r.err != nil {
		if r.rows != nil {
			r.rows.Close()
		}
		return r.err
	}
	// Mirror connRow.Scan: an error captured on the Rows before any iteration.
	if err := r.rows.Err(); err != nil {
		r.rows.Close()
		return err
	}
	if !r.rows.Next() {
		// No row. Distinguish "empty result" (ErrNoRows) from "error while
		// fetching" exactly as pgx does.
		err := r.rows.Err()
		r.rows.Close()
		if err == nil {
			return pgx.ErrNoRows
		}
		return err
	}
	// Scan the first row. pgx.Rows.Scan auto-closes on error; Close() is safe to
	// call again afterwards (Rows.Close is idempotent), and ignores extra rows.
	_ = r.rows.Scan(dest...)
	r.rows.Close()
	return r.rows.Err()
}

// Begin starts a transaction with bounded transient-reconnect retry on the
// connection acquire. Begin only acquires a connection and issues BEGIN; a
// cold-start acquire failure is pre-send and safe to retry.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	var tx pgx.Tx
	err := retry(ctx, "begin", func(ctx context.Context) error {
		var e error
		tx, e = p.pool.Begin(ctx)
		return e
	})
	return tx, err
}

// retry runs fn up to maxAttempts times, sleeping with exponential backoff
// between attempts, but ONLY when fn's error is a transient connection error
// (isTransientConnError). It stops early on success, on a non-transient error,
// on context cancellation, or when totalBudget is exhausted. It is the testable
// core of the FR10 retry logic (AC15).
//
// op is a short label ("exec"/"query"/"begin"/"query_row") used purely for the
// slog event so operators can see which path reconnected.
func retry(ctx context.Context, op string, fn func(context.Context) error) error {
	// Derive a context bounded by totalBudget and pass it to fn, so the budget
	// constrains the SINGLE attempt's execution (a pool acquire / cold dial that
	// hangs), not merely the inter-attempt backoff (BLOCKER fix). The caller's own
	// context still wins when it is already cancelled or carries a tighter
	// deadline: WithTimeout keeps the earlier of the two deadlines. When the
	// budget fires, fn returns context.DeadlineExceeded, which isTransientConnError
	// classifies as non-transient — so the loop stops naturally instead of
	// retrying a request that has already exhausted its time.
	ctx, cancel := context.WithTimeout(ctx, totalBudget)
	defer cancel()

	// The budget deadline drives the backoff cap too: never sleep past the point
	// where the derived context will fire.
	deadline, _ := ctx.Deadline()
	backoff := baseBackoff

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = fn(ctx)
		if err == nil {
			if attempt > 1 {
				// Succeeded only after one or more reconnect retries: record it
				// so a cold-start wake-up is visible in the logs (AC16).
				retryLogger.LogAttrs(ctx, slog.LevelInfo, "db reconnect succeeded",
					slog.String("event", "db_retry_succeeded"),
					slog.String("op", op),
					slog.Int("attempt", attempt),
					slog.Int("max_attempts", maxAttempts),
				)
			}
			return nil
		}

		// Non-transient (real server/application error, or a query-level failure
		// after the connection was established): do not retry — surface it.
		if !isTransientConnError(err) {
			return err
		}

		// Transient connection failure. If we have attempts left and budget/ctx
		// remaining, back off and try again; otherwise give up.
		if attempt == maxAttempts {
			break
		}

		retryLogger.LogAttrs(ctx, slog.LevelWarn, "db transient connection error, retrying",
			slog.String("event", "db_retry_attempt"),
			slog.String("op", op),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", maxAttempts),
			slog.Duration("backoff", backoff),
			slog.String("error", err.Error()),
		)

		// Respect the tighter of the caller's context deadline and our budget.
		if !sleepWithBudget(ctx, backoff, deadline) {
			break
		}
		backoff *= 2
	}

	// All attempts exhausted (or budget/ctx ran out) and still failing. Record
	// the terminal acquire failure so a genuinely-unreachable Neon is visible,
	// then return the error for the HTTP layer to turn into a 5xx (FR10).
	retryLogger.LogAttrs(ctx, slog.LevelError, "db connection failed after retries",
		slog.String("event", "db_retry_exhausted"),
		slog.String("op", op),
		slog.Int("max_attempts", maxAttempts),
		slog.String("error", err.Error()),
	)
	return err
}

// sleepWithBudget sleeps for d, but never past the totalBudget deadline and
// never past the caller's context. It returns false if the context was
// cancelled or the budget is exhausted (caller should stop retrying), true if it
// slept and there is budget left to make another attempt.
func sleepWithBudget(ctx context.Context, d time.Duration, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if d > remaining {
		d = remaining
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		// Still have budget for at least one more attempt only if time remains.
		return time.Now().Before(deadline)
	}
}

// isTransientConnError reports whether err is a connection-level failure that
// occurred BEFORE any query data was sent — i.e. safe to retry. Identification
// uses pgx's own error model (verified against github.com/jackc/pgx/v5@v5.10.0):
//
//   - *pgconn.ConnectError: returned when a connection attempt (dial) fails.
//     This is exactly the cold-start case: the lazy pool dials a Neon compute
//     that is still waking and the TCP/TLS connect is refused/reset/times out.
//     No query was sent, so re-dialing is safe.
//   - pgconn.SafeToRetry(err): pgx marks certain errors (e.g. a connection
//     closed before use) as guaranteed-pre-send; honor that signal too.
//   - context.DeadlineExceeded / Canceled: NOT transient here — the caller's
//     deadline is up, retrying only wastes more of the request budget.
//
// A *pgconn.PgError (a server-reported error such as a constraint violation or
// syntax error) is generally NOT transient: the statement reached the server, and
// re-running a possibly-non-idempotent statement is unsafe. The ONE exception is a
// small whitelist of connect/wake-time SQLSTATEs (retryableSQLSTATEs): when Neon's
// compute is still spinning up it may answer with a server-reported "try again"
// rather than refusing the dial outright, and those are pre-query, safe-to-retry
// conditions. Every other PgError stays non-transient.
func isTransientConnError(err error) bool {
	if err == nil {
		return false
	}
	// Server-reported error: retry ONLY the connect/wake-time SQLSTATE whitelist;
	// every other PgError (constraint violation, syntax error, etc.) is a real
	// application error and is never retried.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return retryableSQLSTATEs[pgErr.Code]
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	return pgconn.SafeToRetry(err)
}

// retryableSQLSTATEs is the whitelist of server-reported SQLSTATE codes treated
// as transient connect/wake-time conditions safe to retry. These are the standard
// PostgreSQL Class 08 (connection exception) and operator-intervention codes that
// occur BEFORE a query is processed, so re-issuing the statement cannot
// double-apply it (pgErr.Code is the 5-char SQLSTATE; field verified against
// pgconn.PgError in pgx/v5@v5.10.0).
//
// The exact set Neon returns while a scale-to-zero compute is waking is
// UNCONFIRMED — this covers the standard PostgreSQL retryable connect-time codes;
// extend it once on-line scale-to-zero observation (NFR8) shows which SQLSTATEs
// Neon actually emits during a wake-up.
var retryableSQLSTATEs = map[string]bool{
	"08000": true, // connection_exception
	"08001": true, // sqlclient_unable_to_establish_sqlconnection
	"08003": true, // connection_does_not_exist
	"08004": true, // sqlserver_rejected_establishment_of_sqlconnection
	"08006": true, // connection_failure
	"57P01": true, // admin_shutdown (server is shutting down / restarting)
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now (server is starting up — the Neon wake case)
	"53300": true, // too_many_connections (transient saturation during a wake storm)
}
