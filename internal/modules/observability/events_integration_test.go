package observability_test

// events_integration_test.go is the TEST_DATABASE_URL-gated, full-chain proof of
// the idempotency and single-transaction batch-landing semantics that cannot be
// exercised without a real Postgres — they hinge on the (source,event_id) UNIQUE
// constraint and ON CONFLICT DO NOTHING:
//
//   - AC3  re-sending a batch with the same (source,event_id) N times lands the
//          event EXACTLY once (rows in events = 1).
//   - E8   a mixed batch (some already-landed events + some new) returns
//          accepted = new count, duplicate = already-present count, and only the
//          new events appear in the table.
//   - FR4  a whole batch lands atomically in one transaction (no per-event
//          connection fan-out).
//
// GATING (AC10): it reads its DSN ONLY from TEST_DATABASE_URL — the same env var
// the verify.sh migration gate and CI Postgres container use — and NEVER touches
// Neon or hardcodes a DSN. When TEST_DATABASE_URL is unset the suite t.Skip()s.
//
// It lives in the external observability_test package and composes the public
// surfaces (NewHandler, RegisterRoutes via HTTP) exactly as the server does, plus
// direct pool SQL to set up and inspect — mirroring auth's integration harness.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
	"github.com/shaomingbo/server-infra-toolkit/internal/modules/observability"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/db"
)

// eventsMigrationRelPath points at the events schema migration; the test applies
// its Up SQL to the TEST Postgres and tears it down with its Down SQL on cleanup.
// The package dir is internal/modules/observability, so the repo root is four
// levels up.
const eventsMigrationRelPath = "../../../db/migrations/00003_events.sql"

// ingestEnv bundles a wired observability Handler over a real pool, the full HTTP
// server that mounts the events route (so ingest is exercised black-box exactly as
// in production), and the pool itself for direct setup/inspection SQL.
type ingestEnv struct {
	pool *db.Pool
	srv  http.Handler
}

// setupIngestEnv applies the events migration to the TEST Postgres, builds the
// runtime pool and a real observability Handler over it, and registers cleanups
// (drop table, close pool). It skips when TEST_DATABASE_URL is unset.
func setupIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("events 集成测试需要 Postgres:请设置 TEST_DATABASE_URL(本地起 dockerized Postgres,CI 用 service container);永不指向 Neon")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	upSQL, downSQL := mustReadEventsGooseSections(t)

	pool, err := db.NewRetryPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewRetryPool: %v", err)
	}
	// Register Close FIRST (runs LAST, LIFO) so the table-drop cleanup below still
	// has an open pool.
	t.Cleanup(pool.Close)

	// Defensive: drop leftovers from an aborted prior run before applying Up, so the
	// CREATE does not fail on a duplicate table.
	if _, err := pool.Exec(ctx, downSQL); err != nil {
		t.Logf("pre-up cleanup (ignorable if table absent): %v", err)
	}
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("apply events migration up: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if _, err := pool.Exec(cctx, downSQL); err != nil {
			t.Logf("cleanup down (DROP TABLE) failed: %v", err)
		}
	})

	h := observability.NewHandler(pool)
	srv := apphttp.NewServer(&config.Config{Version: "it"}, pool, h.RegisterRoutes)
	return &ingestEnv{pool: pool, srv: srv}
}

// ingestResult is the decoded success body (the wire is unstable; the test reads
// the count fields it asserts on).
type ingestResult struct {
	Accepted  int    `json:"accepted"`
	Duplicate int    `json:"duplicate"`
	Rejected  int    `json:"rejected"`
	RequestID string `json:"requestId"`
}

// post sends a batch (already-encoded JSON) through the real server and returns
// the status and decoded body. It sets the X-Request-Id header the middleware
// chain provides upstream.
func (e *ingestEnv) post(t *testing.T, body string) (int, ingestResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "it-events")
	e.srv.ServeHTTP(rec, req)
	var res ingestResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode success body: %v\nbody=%s", err, rec.Body.String())
		}
	}
	return rec.Code, res
}

// countRows reads the current row count for a given source from the events table.
func (e *ingestEnv) countRows(t *testing.T, ctx context.Context, source string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source = $1`, source).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// event builds one schema-valid event JSON object string with the given source
// and eventId (the idempotency key components).
func event(source, eventID string) string {
	return `{"eventId":"` + eventID + `","kind":"log","traceId":"trace-x","timestampMs":1748563200123,"source":"` + source + `","name":"app.launch","attributes":{"ok":true}}`
}

// batchOf joins event JSON strings into a bare JSON array body.
func batchOf(events ...string) string {
	return "[" + strings.Join(events, ",") + "]"
}

// TestIngest_IdempotentResend (AC3) sends a batch with the same (source,event_id)
// N times and asserts the event lands EXACTLY once: the first send accepts it,
// every later send reports it as a duplicate, and the table holds exactly 1 row.
func TestIngest_IdempotentResend(t *testing.T) {
	e := setupIngestEnv(t)
	ctx := context.Background()
	const source = "resend-src"

	body := batchOf(event(source, "evt-resend-1"))

	const n = 4
	for i := 0; i < n; i++ {
		code, res := e.post(t, body)
		if code != http.StatusOK {
			t.Fatalf("send %d: status = %d, want 200", i+1, code)
		}
		if i == 0 {
			if res.Accepted != 1 || res.Duplicate != 0 {
				t.Fatalf("first send: counts = %+v, want accepted=1 duplicate=0", res)
			}
		} else {
			if res.Accepted != 0 || res.Duplicate != 1 {
				t.Fatalf("resend %d: counts = %+v, want accepted=0 duplicate=1 (hold-and-retry must not re-insert)", i+1, res)
			}
		}
	}

	if got := e.countRows(t, ctx, source); got != 1 {
		t.Fatalf("after %d sends of the same (source,event_id): rows = %d, want exactly 1 (AC3)", n, got)
	}
}

// TestIngest_MixedBatchPartialDuplicate (E8) first lands a batch, then sends a
// SECOND batch that mixes the already-landed events with brand-new ones, and
// asserts the response reports accepted = new count and duplicate = already-present
// count, with only the new events added to the table.
func TestIngest_MixedBatchPartialDuplicate(t *testing.T) {
	e := setupIngestEnv(t)
	ctx := context.Background()
	const source = "mixed-src"

	// First batch: 3 events land fresh.
	first := batchOf(
		event(source, "m-1"),
		event(source, "m-2"),
		event(source, "m-3"),
	)
	if code, res := e.post(t, first); code != http.StatusOK || res.Accepted != 3 || res.Duplicate != 0 {
		t.Fatalf("first batch: status=%d counts=%+v, want 200 accepted=3 duplicate=0", code, res)
	}
	if got := e.countRows(t, ctx, source); got != 3 {
		t.Fatalf("after first batch: rows = %d, want 3", got)
	}

	// Second batch: 2 already-present (m-2, m-3) + 2 new (m-4, m-5).
	second := batchOf(
		event(source, "m-2"), // dup
		event(source, "m-3"), // dup
		event(source, "m-4"), // new
		event(source, "m-5"), // new
	)
	code, res := e.post(t, second)
	if code != http.StatusOK {
		t.Fatalf("second batch: status = %d, want 200", code)
	}
	if res.Accepted != 2 || res.Duplicate != 2 || res.Rejected != 0 {
		t.Fatalf("mixed batch: counts = %+v, want accepted=2 duplicate=2 rejected=0 (E8)", res)
	}
	if got := e.countRows(t, ctx, source); got != 5 {
		t.Fatalf("after mixed batch: rows = %d, want 5 (3 original + 2 new, duplicates skipped)", got)
	}
}

// TestIngest_IntraBatchDuplicate sends a SINGLE batch carrying two events with the
// same (source,event_id) and asserts 200 accepted=1 duplicate=1: within one
// transaction, the per-event INSERT ... ON CONFLICT DO NOTHING lands the first and
// skips the second, so the idempotency key dedupes inside a batch, not just across
// batches. Exactly 1 row reaches the table.
func TestIngest_IntraBatchDuplicate(t *testing.T) {
	e := setupIngestEnv(t)
	ctx := context.Background()
	const source = "intra-dup-src"

	body := batchOf(
		event(source, "intra-1"),
		event(source, "intra-1"), // same (source,event_id) inside the same batch
	)
	code, res := e.post(t, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if res.Accepted != 1 || res.Duplicate != 1 || res.Rejected != 0 {
		t.Fatalf("intra-batch dup: counts = %+v, want accepted=1 duplicate=1 rejected=0", res)
	}
	if got := e.countRows(t, ctx, source); got != 1 {
		t.Fatalf("after intra-batch dup: rows = %d, want exactly 1 (ON CONFLICT skips the second in-batch copy)", got)
	}
}

// TestIngest_BatchLandsAtomically (FR4) sends a larger batch in one request and
// asserts every row landed — a single transaction carried the whole batch (the
// row count equals the batch size, no partial landing).
func TestIngest_BatchLandsAtomically(t *testing.T) {
	e := setupIngestEnv(t)
	ctx := context.Background()
	const source = "atomic-src"

	const n = 50
	events := make([]string, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, event(source, "a-"+strconv.Itoa(i)))
	}
	code, res := e.post(t, batchOf(events...))
	if code != http.StatusOK {
		t.Fatalf("batch: status = %d, want 200", code)
	}
	if res.Accepted != n || res.Duplicate != 0 {
		t.Fatalf("batch: counts = %+v, want accepted=%d duplicate=0", res, n)
	}
	if got := e.countRows(t, ctx, source); got != n {
		t.Fatalf("after batch: rows = %d, want %d (whole batch must land in one transaction)", got, n)
	}
}

// mustReadEventsGooseSections reads the events migration and returns its Up and
// Down SQL bodies, splitting on the goose section markers. The events migration is
// plain DDL (no -- +goose StatementBegin/End blocks), so a marker split suffices.
// Mirrors auth's mustReadAuthGooseSections.
func mustReadEventsGooseSections(t *testing.T) (up, down string) {
	t.Helper()
	abs, err := filepath.Abs(eventsMigrationRelPath)
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
