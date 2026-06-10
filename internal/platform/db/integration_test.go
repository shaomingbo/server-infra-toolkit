package db_test

// integration_test.go is the full-chain INFRASTRUCTURE SELF-TEST for T1 (AC8 /
// FR7). It proves the whole data-access chain is wired correctly end to end:
//
//	migration-created table -> sqlc-generated query -> retry connection pool -> assert
//
// Concretely it applies db/migrations/00001_infra_selftest.sql (the same SQL the
// real goose migration runs), then calls the sqlc-generated
// dbgen.New(pool).GetInfraSelftest(ctx, 1) THROUGH db.NewRetryPool (the runtime
// pool, injected via the DBTX interface), and asserts it returns the seeded row
// (id=1, label="ok"). It is NOT a business test: it only touches the
// _infra_selftest carrier table (NG1).
//
// On purpose this does NOT use `SELECT 1` or any system-catalog query as a
// shortcut (FR7): a SELECT-1 would prove a connection works but would NOT prove
// the migration -> sqlc -> pool wiring. The whole point is to exercise a real
// table created by a real migration through a real sqlc query.
//
// GATING (AC8): the test needs a real Postgres and reads its DSN ONLY from
// TEST_DATABASE_URL (the same env var the verify.sh migration gate and the CI
// Postgres service container use). It NEVER touches Neon and hardcodes no DSN.
// When TEST_DATABASE_URL is unset, the test t.Skip()s with an explicit reason,
// so it runs in a dockerized local / CI Postgres and is skipped otherwise.
//
// It lives in the external test package db_test so it can compose the public
// surfaces of both db (NewRetryPool) and dbgen (New/GetInfraSelftest) exactly as
// a real caller would.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/db"
	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// migrationRelPath points at the self-test migration whose Up section creates
// the _infra_selftest carrier and seeds (1, "ok"). `go test` runs with the
// working directory set to this package dir (internal/platform/db); the repo
// root is three levels up (db -> platform -> internal -> root).
const migrationRelPath = "../../../db/migrations/00001_infra_selftest.sql"

// TestFullChain_MigrationToSqlcThroughPool is the AC8/FR7 full-chain proof.
func TestFullChain_MigrationToSqlcThroughPool(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("整链测试需要 Postgres:请设置 TEST_DATABASE_URL(本地起 dockerized Postgres 并导出,CI 用 service container)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	upSQL, downSQL := mustReadGooseSections(t)

	// Build the runtime retry pool against the TEST Postgres (NOT Neon). *db.Pool
	// implements dbgen.DBTX, so it is injected directly into the sqlc Queries
	// below — this is the exact pool -> DBTX -> sqlc wiring the service uses.
	pool, err := db.NewRetryPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewRetryPool: %v", err)
	}
	// Pool close and table teardown both go through t.Cleanup, NOT defer: t.Cleanup
	// runs AFTER the test body's defers, so a `defer pool.Close()` would close the
	// pool before the table-drop cleanup could use it. t.Cleanup is LIFO, so we
	// register Close FIRST and the DROP SECOND — the DROP then runs first (pool
	// still open), Close second.
	t.Cleanup(pool.Close)

	// Defensive isolation: drop any leftover carrier from a previously aborted
	// run before applying the migration up, so a half-cleaned table does not turn
	// the CREATE into a duplicate-table error.
	if _, err := pool.Exec(ctx, downSQL); err != nil {
		t.Logf("pre-up cleanup (ignorable if table absent): %v", err)
	}

	// MIGRATION up: create the _infra_selftest table and seed (1,"ok") using the
	// migration file's own Up SQL, run THROUGH the pool under test.
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("apply migration up: %v", err)
	}
	// Cleanup runs the migration's Down SQL (DROP TABLE) so the test leaves no
	// residue — keeps the test repeatable and isolated (t.Cleanup, AC8). Runs
	// before the pool-close cleanup registered above (t.Cleanup LIFO).
	t.Cleanup(func() {
		// Fresh context: the test's ctx may already be cancelled by the time
		// cleanup runs.
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if _, err := pool.Exec(cctx, downSQL); err != nil {
			t.Logf("cleanup down (DROP TABLE) failed: %v", err)
		}
	})

	// FULL-CHAIN ASSERTION: run the sqlc-GENERATED query through the pool. No
	// SELECT 1 / system-catalog shortcut — this hits the real migration-created
	// table via the real generated GetInfraSelftest (FR7).
	q := dbgen.New(pool)
	got, err := q.GetInfraSelftest(ctx, 1)
	if err != nil {
		t.Fatalf("GetInfraSelftest(1): %v", err)
	}

	if got.ID != 1 {
		t.Errorf("ID = %d, want 1 (self-test seed)", got.ID)
	}
	if got.Label != "ok" {
		t.Errorf("Label = %q, want \"ok\" (self-test seed)", got.Label)
	}
}

// mustReadGooseSections reads the self-test migration file and returns its Up and
// Down SQL bodies. It splits on the goose section markers (-- +goose Up / -- +goose
// Down). This migration is plain DDL with no -- +goose StatementBegin/End blocks,
// so a marker split is sufficient and intentionally minimal; if a future
// statement-block migration were pointed here, this parser would need extending.
func mustReadGooseSections(t *testing.T) (up, down string) {
	t.Helper()

	abs, err := filepath.Abs(migrationRelPath)
	if err != nil {
		t.Fatalf("resolve migration path: %v", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read migration %s: %v", abs, err)
	}

	const upMarker = "-- +goose Up"
	const downMarker = "-- +goose Down"
	content := string(raw)

	upIdx := strings.Index(content, upMarker)
	downIdx := strings.Index(content, downMarker)
	if upIdx < 0 || downIdx < 0 || downIdx < upIdx {
		t.Fatalf("migration %s missing/!ordered goose Up/Down markers", abs)
	}

	up = strings.TrimSpace(content[upIdx+len(upMarker) : downIdx])
	down = strings.TrimSpace(content[downIdx+len(downMarker):])
	if up == "" || down == "" {
		t.Fatalf("migration %s has empty Up or Down section", abs)
	}
	return up, down
}
