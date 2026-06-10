package db

// pool.go holds the SERVICE-RUNTIME database path: a lazily-constructed
// pgxpool connection pool used by HTTP handlers (T1 onward). It is distinct
// from Smoke (db.go), which is the one-shot bare-connection reachability check
// run only by `-smoke`. Two separate paths on purpose:
//
//   - Smoke   : one bare pgx.Connect, SELECT 1, close. Deploy-time proof only.
//   - NewPool : long-lived pooled connections for request handling. Lazy: it
//               does NOT dial Neon at construction, so service startup never
//               blocks waiting for the database to wake (AC10).
//
// NEON_DSN must never be logged or printed in plaintext. NewPool takes the
// revealed DSN string and passes it straight to pgxpool without echoing it.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the narrow database surface the rest of the service depends on. It is
// deliberately minimal so business code (T1+) takes a small interface, not the
// concrete *pgxpool.Pool, which keeps pgxpool an implementation detail of this
// package (AC3/FR3). *pgxpool.Pool satisfies DBTX natively.
//
// Begin is reserved for future transactional work (T1+ business handlers); it
// is part of the contract now so callers can rely on a stable surface.
//
// Method signatures mirror pgx/pgxpool exactly (verified against
// github.com/jackc/pgx/v5@v5.10.0 pgxpool/pool.go: Exec ->
// (pgconn.CommandTag, error), Query -> (pgx.Rows, error), QueryRow -> pgx.Row,
// Begin -> (pgx.Tx, error)).
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Compile-time assertion that *pgxpool.Pool implements DBTX. If a future pgx
// upgrade changes a signature, this breaks the build instead of silently
// drifting.
var _ DBTX = (*pgxpool.Pool)(nil)

// defaultMaxConns is the conservative per-instance pool ceiling applied when the
// DSN does not specify pool_max_conns. We set it explicitly because pgxpool's
// own default is max(4, runtime.NumCPU()) (verified in pgxpool/pool.go
// ParseConfig: MaxConns defaults to 4 then is raised to NumCPU when larger) —
// on a multi-core Cloud Run instance that silently scales the pool with the CPU
// count, which we do NOT want when fanning out to a shared Neon endpoint.
//
// Sizing rule (FR4/AC4): defaultMaxConns × max Cloud Run instances ≤ Neon
// endpoint connection limit.
//   - max Cloud Run instances = 2 (docs/DEPLOY.md: --max-instances=2).
//   - Neon direct-endpoint connection limit scales with compute size (CU). It
//     must be re-confirmed against current Neon docs before changing this value
//     (NFR7). HOW TO CHECK: Neon Console → Project → Branch → Compute, read the
//     compute size (CU); then Neon docs "Connection limits" /
//     "Compute size and autoscaling" for the max_connections for that CU
//     (the ~0.25 CU tier documents ~100, with a handful reserved for the
//     system, leaving ~97 usable). Source to re-verify:
//     https://neon.tech/docs/connect/connection-pooling and
//     https://neon.tech/docs/manage/computes
//
// 5 × 2 = 10, comfortably under the ~97 usable on the smallest tier, leaving
// ample headroom for the one-shot -smoke connection and any admin sessions.
const defaultMaxConns int32 = 5

// maxConnsCeiling is a hard guardrail: even if the DSN explicitly asks for more
// (pool_max_conns), we clamp down to this so a misconfigured DSN cannot let a
// scale-zero wake-up connection storm exceed the Neon endpoint limit (FR4/E1).
// Chosen so maxConnsCeiling × max instances (2) stays well under the smallest
// Neon tier's usable connections (~97): 20 × 2 = 40. Re-verify the Neon limit
// (see defaultMaxConns) before raising this.
const maxConnsCeiling int32 = 20

// NewPool builds a lazy pgxpool from dsn. Pool parameters may be tuned via the
// DSN query string (pool_max_conns / pool_min_conns / pool_max_conn_lifetime /
// pool_max_conn_idle_time / pool_health_check_period — parameter names verified
// against pgxpool/pool.go ParseConfig in pgx/v5@v5.10.0), so no new env vars are
// introduced (AC2). When pool_max_conns is absent we apply defaultMaxConns
// instead of pgxpool's CPU-scaled default; in all cases MaxConns is clamped to
// maxConnsCeiling.
//
// Construction is lazy: pgxpool.NewWithConfig does not dial the database, and we
// force MinConns/MinIdleConns to 0 so no idle connections are pre-created, so
// this returns without any network round trip (AC10/NG8). The pool dials Neon on
// first acquire, which is the request path, not startup.
//
// dsn is never logged here.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	// Detect whether the DSN explicitly set pool_max_conns BEFORE ParseConfig
	// consumes (deletes) the runtime param. We only read whether the KEY is
	// present — never the DSN value or any credential — so nothing sensitive is
	// touched. dsnHasMaxConns swallows parse errors and reports false; a malformed
	// DSN is surfaced by ParseConfig below, and on the false path we fall back to
	// the conservative defaultMaxConns, which is the safe direction.
	explicitMaxConns := dsnHasMaxConns(dsn)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Wrap without including dsn: ParseConfig errors are crafted by pgx to
		// redact credentials, but we add no context that could leak the DSN.
		return nil, fmt.Errorf("db: parse pool config: %w", err)
	}

	// Apply our own MaxConns policy rather than trusting the library default.
	// ParseConfig sets MaxConns to its CPU-scaled default (max(4, NumCPU)) when
	// the DSN omits pool_max_conns, which we must not rely on (AC4): on a
	// multi-core Cloud Run instance that silently scales the pool with the CPU
	// count. We distinguish the two cases precisely via explicitMaxConns:
	//   - DSN omitted pool_max_conns: overwrite the CPU-scaled default with the
	//     explicit conservative defaultMaxConns floor (AC4: "unspecified -> 5").
	//   - DSN set pool_max_conns: respect the parsed value (AC4: "specified ->
	//     DSN value").
	// In BOTH cases we then clamp DOWN to maxConnsCeiling so an over-eager DSN
	// value cannot exceed the Neon endpoint limit during a scale-zero wake-up
	// connection storm (FR4/E1) — implementing "unspecified -> 5; specified ->
	// DSN value; both <= ceiling".
	if !explicitMaxConns {
		cfg.MaxConns = defaultMaxConns
	}
	if cfg.MaxConns > maxConnsCeiling {
		cfg.MaxConns = maxConnsCeiling
	}

	// Force the pool to keep NO idle/minimum connections, even if the DSN tries
	// to set pool_min_conns / pool_min_idle_conns. A non-zero minimum makes
	// NewWithConfig spawn a background goroutine that pre-dials Neon to satisfy
	// the minimum, which would wake the compute at startup and break the lazy /
	// no-prewarm guarantee (AC10/NG8). Lazy-not-prewarm is a hard constraint, so
	// we override any DSN-provided minimum unconditionally. (Both fields verified
	// to exist on pgxpool.Config in pgx/v5@v5.10.0.)
	cfg.MinConns = 0
	cfg.MinIdleConns = 0

	// Lazy construction: no ping, no eager dial (AC10).
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: build pool: %w", err)
	}
	return pool, nil
}

// dsnHasMaxConns reports whether dsn explicitly carries a pool_max_conns
// parameter, so NewPool can distinguish "DSN set the pool size" from "pgxpool
// applied its CPU-scaled default" (AC4). pgxpool's ParseConfig deletes the param
// while parsing, so this must run on the raw dsn first.
//
// It only inspects whether the KEY is present; it never reads the value, never
// logs, and never returns any part of the DSN. It handles both DSN forms pgx
// accepts: the URL form (postgres://...?pool_max_conns=N) and the keyword/value
// form (host=... pool_max_conns=N). Any parse difficulty returns false, which
// routes NewPool to the conservative defaultMaxConns floor.
func dsnHasMaxConns(dsn string) bool {
	const key = "pool_max_conns"

	// URL form: parse the query string and check for the key.
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		return u.Query().Has(key)
	}

	// Keyword/value form (space-separated "k=v" pairs). We do a minimal token
	// scan for the key= prefix rather than a full DSN parser: we only need key
	// presence, not the value, and must not echo anything sensitive.
	for _, field := range strings.Fields(dsn) {
		if k, _, ok := strings.Cut(field, "="); ok && k == key {
			return true
		}
	}
	return false
}
