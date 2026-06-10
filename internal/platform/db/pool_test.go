package db

// pool_test.go covers NewPool's connection-sizing policy (AC4) and the lazy /
// no-prewarm guarantee (AC10/NG8). NewPool is lazy — pgxpool.NewWithConfig does
// not dial — so these tests construct real pools against unreachable DSNs and
// inspect the resolved config without any database. dsnHasMaxConns is tested
// directly for the DSN-form detection logic.

import (
	"context"
	"testing"
)

// TestDSNHasMaxConns exercises the explicit-pool_max_conns detection across both
// DSN forms pgx accepts and the absent case (which routes NewPool to the
// conservative default).
func TestDSNHasMaxConns(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want bool
	}{
		{"url with pool_max_conns", "postgres://u:p@host:5432/db?pool_max_conns=8", true},
		{"url without pool_max_conns", "postgres://u:p@host:5432/db?sslmode=require", false},
		{"url no query", "postgres://u:p@host:5432/db", false},
		{"keyword form with pool_max_conns", "host=h user=u dbname=d pool_max_conns=8", true},
		{"keyword form without", "host=h user=u dbname=d sslmode=require", false},
		{"keyword form substring not a key", "host=h application_name=pool_max_conns_app", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dsnHasMaxConns(tc.dsn); got != tc.want {
				t.Fatalf("dsnHasMaxConns(%q) = %v, want %v", tc.dsn, got, tc.want)
			}
		})
	}
}

// TestNewPool_MaxConns_DefaultWhenUnspecified: with no pool_max_conns in the DSN,
// MaxConns must be exactly defaultMaxConns regardless of the host CPU count —
// NOT pgxpool's CPU-scaled default (AC4: "unspecified -> 5").
func TestNewPool_MaxConns_DefaultWhenUnspecified(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:5432/db")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != defaultMaxConns {
		t.Fatalf("MaxConns = %d, want defaultMaxConns=%d (CPU-scaled default must be overridden)", got, defaultMaxConns)
	}
}

// TestNewPool_MaxConns_RespectsDSNValue: a pool_max_conns within the ceiling is
// honored as-is (AC4: "specified -> DSN value").
func TestNewPool_MaxConns_RespectsDSNValue(t *testing.T) {
	const want int32 = 8 // > defaultMaxConns (5), < maxConnsCeiling (20)
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:5432/db?pool_max_conns=8")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != want {
		t.Fatalf("MaxConns = %d, want the DSN value %d", got, want)
	}
}

// TestNewPool_MaxConns_BelowDefaultRespectsDSN: a DSN value below defaultMaxConns
// is still the explicit operator intent and is respected (AC4: specified ->
// DSN value), distinguishing this from the unspecified path. This is the precise
// behavior the BLOCKER fix enables: the old floor-clamp would have raised it to 5.
func TestNewPool_MaxConns_BelowDefaultRespectsDSN(t *testing.T) {
	const want int32 = 2
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:5432/db?pool_max_conns=2")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != want {
		t.Fatalf("MaxConns = %d, want the explicit DSN value %d", got, want)
	}
}

// TestNewPool_MaxConns_ClampedToCeiling: an over-eager DSN value is clamped DOWN
// to maxConnsCeiling so a wake-up storm cannot exceed the Neon endpoint limit
// (FR4/E1).
func TestNewPool_MaxConns_ClampedToCeiling(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:5432/db?pool_max_conns=999")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != maxConnsCeiling {
		t.Fatalf("MaxConns = %d, want clamp to maxConnsCeiling=%d", got, maxConnsCeiling)
	}
}

// TestNewPool_MinConnsForcedZero: even when the DSN asks for a minimum / minimum
// idle pool size, NewPool forces both to 0 so no idle connections are pre-dialed
// and Neon is not woken at startup (AC10/NG8).
func TestNewPool_MinConnsForcedZero(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:5432/db?pool_min_conns=3&pool_min_idle_conns=2")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	cfg := pool.Config()
	if cfg.MinConns != 0 {
		t.Fatalf("MinConns = %d, want forced 0 (lazy/no-prewarm, AC10/NG8)", cfg.MinConns)
	}
	if cfg.MinIdleConns != 0 {
		t.Fatalf("MinIdleConns = %d, want forced 0 (lazy/no-prewarm, AC10/NG8)", cfg.MinIdleConns)
	}
}
