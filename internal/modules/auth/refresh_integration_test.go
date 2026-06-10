package auth_test

// refresh_integration_test.go is the TEST_DATABASE_URL-gated, full-chain proof of
// the refresh-rotation and Bearer-verification security semantics that cannot be
// exercised without a real Postgres (they hinge on FOR UPDATE row locks, real
// transactions, and now()-relative timestamps):
//
//   - AC5  replay of a consumed refresh token (used long ago) revokes the family.
//   - AC6  two concurrent refreshes of the same token: exactly one 200, one 401,
//          family NOT revoked, the winner's new token still usable.
//   - AC7  a revoked access token is rejected by BearerMiddleware immediately; a
//          revoked refresh token (or family) is rejected on refresh.
//   - AC15 the rotation is atomic: a failure mid-flight leaves the old token
//          unconsumed and no orphan new token (asserted via the revoke-then-replay
//          path, which must not also rotate).
//   - AC20 a disabled user cannot refresh and cannot use its access token.
//
// GATING (AC8): it reads its DSN ONLY from TEST_DATABASE_URL — the same env var
// the verify.sh migration gate and CI Postgres container use — and NEVER touches
// Neon or hardcodes a DSN. When TEST_DATABASE_URL is unset the test t.Skip()s.
//
// It lives in the external auth_test package and composes the public surfaces
// (NewHandler, RegisterRoutes/refresh via HTTP, BearerMiddleware) exactly as the
// server does, plus direct pool SQL to set up and to simulate the passage of time
// (rewinding used_at) and a disabled user — manipulations a client cannot perform.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
	"github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/db"
	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// authMigrationRelPath points at the auth schema migration; the test applies its
// Up SQL to the TEST Postgres and tears it down with its Down SQL on cleanup. The
// package dir is internal/modules/auth, so the repo root is four levels up.
const authMigrationRelPath = "../../../db/migrations/00002_auth.sql"

// testEnv bundles a wired auth Handler over a real pool, the full HTTP server that
// mounts the auth routes (so login/refresh are exercised black-box exactly as in
// production — no test-only exported handler methods), and the pool itself for
// direct setup/inspection SQL.
type testEnv struct {
	pool *db.Pool
	h    *auth.Handler
	srv  http.Handler
}

// setupAuthEnv applies the auth migration to the TEST Postgres, builds the runtime
// pool and a real auth Handler over it, and registers cleanups (drop tables, close
// pool). It skips when TEST_DATABASE_URL is unset.
func setupAuthEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("refresh/Bearer 集成测试需要 Postgres:请设置 TEST_DATABASE_URL(本地起 dockerized Postgres,CI 用 service container);永不指向 Neon")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	upSQL, downSQL := mustReadAuthGooseSections(t)

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
		t.Logf("pre-up cleanup (ignorable if tables absent): %v", err)
	}
	if _, err := pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("apply auth migration up: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if _, err := pool.Exec(cctx, downSQL); err != nil {
			t.Logf("cleanup down (DROP TABLE) failed: %v", err)
		}
	})

	h := auth.NewHandler(pool)
	srv := apphttp.NewServer(&config.Config{Version: "it"}, pool, h.RegisterRoutes)
	return &testEnv{pool: pool, h: h, srv: srv}
}

// seedUser creates one active user with a real argon2 hash of password and returns
// its id. It uses the generated CreateUser through the pool, matching how the seed
// command creates users.
func (e *testEnv) seedUser(t *testing.T, ctx context.Context, username, password string) pgtype.UUID {
	t.Helper()
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	u, err := dbgen.New(e.pool).CreateUser(ctx, dbgen.CreateUserParams{
		Username:     username,
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

// login drives POST /v1/auth/login and returns the decoded session. It fails the
// test on a non-200.
func (e *testEnv) login(t *testing.T, username, password string) sessionResp {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var s sessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode login session: %v", err)
	}
	return s
}

// refresh drives POST /v1/auth/refresh with the given refresh token and returns
// the recorder plus the decoded session (only valid when status is 200).
func (e *testEnv) refresh(t *testing.T, refreshToken string) (*httptest.ResponseRecorder, sessionResp) {
	t.Helper()
	body := `{"refreshToken":"` + refreshToken + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	var s sessionResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("decode refresh session: %v", err)
		}
	}
	return rec, s
}

// bearerStatus runs the access token through BearerMiddleware in front of a 200
// handler and returns the resulting status code.
func (e *testEnv) bearerStatus(accessToken string) int {
	guarded := e.h.BearerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/protected", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "it-bearer")
	guarded.ServeHTTP(rec, req)
	return rec.Code
}

// countRows runs a `SELECT count(*) ...` query through the pool and returns the
// scalar count, used to assert atomic row-state after a rotation (AC15).
func (e *testEnv) countRows(t *testing.T, ctx context.Context, query string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// sessionResp mirrors the LoginSession wire shape for decoding in tests.
type sessionResp struct {
	UserID       string `json:"userId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// TestRefresh_HappyRotation asserts a single refresh returns a new pair and the new
// access token authenticates, while the OLD refresh token, when reused immediately,
// is rejected as the concurrency-loser (within the leeway) WITHOUT revoking the
// family — so the new token still works.
func TestRefresh_HappyRotation(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "rotate", "rotate-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)
	rec, newSess := e.refresh(t, sess.RefreshToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if newSess.RefreshToken == sess.RefreshToken {
		t.Fatal("refresh did not rotate the refresh token (same value returned)")
	}
	if e.bearerStatus(newSess.AccessToken) != http.StatusOK {
		t.Fatal("new access token did not authenticate after rotation")
	}

	// Atomicity (AC15): a successful rotation committed ALL three writes together —
	// the old refresh token is now marked used, exactly one new refresh row exists
	// on the same family, and exactly one new access row exists. There is no
	// half-applied state (old marked used but no new row, or a new row with the old
	// not marked). login wrote 1 refresh + 1 access; rotation adds 1 each → 2 + 2.
	if got := e.countRows(t, ctx, `SELECT count(*) FROM refresh_tokens WHERE used_at IS NOT NULL`); got != 1 {
		t.Fatalf("after one rotation: %d refresh tokens marked used, want exactly 1 (atomic mark-used)", got)
	}
	if got := e.countRows(t, ctx, `SELECT count(*) FROM refresh_tokens`); got != 2 {
		t.Fatalf("after one rotation: %d refresh rows, want 2 (1 login + 1 rotated, no orphan/missing)", got)
	}
	if got := e.countRows(t, ctx, `SELECT count(*) FROM access_tokens`); got != 2 {
		t.Fatalf("after one rotation: %d access rows, want 2 (1 login + 1 rotated)", got)
	}

	// Reuse the OLD refresh token immediately: within the leeway this is a
	// concurrency loser → 401, family NOT revoked, so the NEW token still refreshes.
	rec2, _ := e.refresh(t, sess.RefreshToken)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("immediate reuse of old token: status = %d, want 401", rec2.Code)
	}
	rec3, _ := e.refresh(t, newSess.RefreshToken)
	if rec3.Code != http.StatusOK {
		t.Fatalf("new token after immediate-reuse 401: status = %d, want 200 (family must NOT be revoked)", rec3.Code)
	}
}

// TestRefresh_ReplayRevokesFamily (AC5/E3) asserts that reusing a CONSUMED refresh
// token after the leeway window (simulated by rewinding used_at to an hour ago)
// is treated as a replay: it 401s AND revokes the whole family, so other tokens on
// that family can no longer refresh.
func TestRefresh_ReplayRevokesFamily(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "replay", "replay-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)
	rec, newSess := e.refresh(t, sess.RefreshToken) // consumes sess's token
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation status = %d, want 200", rec.Code)
	}

	// Rewind the consumed token's used_at to an hour ago so reuse falls OUTSIDE the
	// leeway window and is judged a genuine replay (a manipulation only the server
	// can do — a real attacker just presents a stolen token long after rotation).
	oldSelector, _, _ := strings.Cut(sess.RefreshToken, ".")
	if _, err := e.pool.Exec(ctx,
		`UPDATE refresh_tokens SET used_at = now() - interval '1 hour' WHERE selector = $1`,
		oldSelector,
	); err != nil {
		t.Fatalf("rewind used_at: %v", err)
	}

	// Replay the old token → 401 AND family revoked.
	recReplay, _ := e.refresh(t, sess.RefreshToken)
	if recReplay.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", recReplay.Code)
	}

	// The new token (same family) must now be revoked too → refresh with it 401s.
	recNew, _ := e.refresh(t, newSess.RefreshToken)
	if recNew.Code != http.StatusUnauthorized {
		t.Fatalf("post-replay refresh with sibling token: status = %d, want 401 (family must be revoked, AC5)", recNew.Code)
	}
}

// TestRefresh_Concurrent (AC6) fires two simultaneous refreshes of the SAME token
// and asserts exactly one 200 and one 401, the family is NOT revoked, and the
// winner's new token still refreshes. The FOR UPDATE row lock is what serializes
// the two so there is no double-spend and no double-failure.
func TestRefresh_Concurrent(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "concurrent", "concurrent-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	sessions := make([]sessionResp, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec, s := e.refresh(t, sess.RefreshToken)
			codes[idx] = rec.Code
			sessions[idx] = s
		}(i)
	}
	wg.Wait()

	got200, got401, winner := 0, 0, -1
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			got200++
			winner = i
		case http.StatusUnauthorized:
			got401++
		default:
			t.Fatalf("concurrent refresh %d: unexpected status %d", i, c)
		}
	}
	if got200 != 1 || got401 != 1 {
		t.Fatalf("concurrent refresh: got %d×200 and %d×401, want exactly one of each (no double-spend/double-fail)", got200, got401)
	}

	// Family must NOT have been revoked: the winner's new token still refreshes.
	recWin, _ := e.refresh(t, sessions[winner].RefreshToken)
	if recWin.Code != http.StatusOK {
		t.Fatalf("winner's new token after concurrent race: status = %d, want 200 (family must NOT be revoked, AC6)", recWin.Code)
	}
}

// TestRefresh_ConcurrentN5 (AC6, N=5) widens the concurrency race beyond the 2-way
// case: five goroutines fire the SAME valid refresh token simultaneously. The
// FOR UPDATE row lock must serialize all five so EXACTLY one wins (200) and the
// other four lose cleanly (401) WITHOUT revoking the family — and the single
// winner's new token must still refresh afterward. This guards the db_now-based
// grace-window judgement under a fan-out wider than two, where any clock-domain
// mismatch or a misclassified loser would surface as a wrong count or a revoked
// family.
func TestRefresh_ConcurrentN5(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "concurrent5", "concurrent5-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)

	const n = 5
	var wg sync.WaitGroup
	codes := make([]int, n)
	sessions := make([]sessionResp, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec, s := e.refresh(t, sess.RefreshToken)
			codes[idx] = rec.Code
			sessions[idx] = s
		}(i)
	}
	wg.Wait()

	got200, got401, winner := 0, 0, -1
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			got200++
			winner = i
		case http.StatusUnauthorized:
			got401++
		default:
			t.Fatalf("concurrent5 refresh %d: unexpected status %d", i, c)
		}
	}
	if got200 != 1 || got401 != n-1 {
		t.Fatalf("concurrent5 refresh: got %d×200 and %d×401, want exactly 1×200 and %d×401 (no double-spend, all losers clean)", got200, got401, n-1)
	}

	// Family must NOT have been revoked by any of the four losers: the single
	// winner's new token still refreshes.
	recWin, _ := e.refresh(t, sessions[winner].RefreshToken)
	if recWin.Code != http.StatusOK {
		t.Fatalf("winner's new token after 5-way concurrent race: status = %d, want 200 (family must NOT be revoked, AC6)", recWin.Code)
	}
}

// selectorOf splits a "selector.verifier" refresh token and returns its selector,
// the plaintext lookup key used to target a single row in setup SQL.
func selectorOf(t *testing.T, refreshToken string) string {
	t.Helper()
	sel, _, ok := strings.Cut(refreshToken, ".")
	if !ok || sel == "" {
		t.Fatalf("refresh token %q has no selector", refreshToken)
	}
	return sel
}

// rewindUsedAt moves the named token's used_at far into the past so reuse falls
// OUTSIDE refreshReuseLeeway and is judged a genuine replay (a manipulation only
// the server can do — a thief just presents a stolen token long after rotation).
func (e *testEnv) rewindUsedAt(t *testing.T, ctx context.Context, selector string) {
	t.Helper()
	tag, err := e.pool.Exec(ctx,
		`UPDATE refresh_tokens SET used_at = now() - interval '1 hour' WHERE selector = $1`,
		selector,
	)
	if err != nil {
		t.Fatalf("rewind used_at for %s: %v", selector, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("rewind used_at for %s: affected %d rows, want 1", selector, tag.RowsAffected())
	}
}

// TestRefresh_ConcurrentReplayNoEscape is the BLOCKER fix's escape invariant. A
// family F holds a current valid token C plus an OLD used token O whose used_at is
// outside the grace window (a replay candidate). refresh(C) (a legitimate rotation
// that inserts a NEW row) and refresh(O) (a replay that revokes the family) fire
// concurrently. Without the family advisory lock, the replay's family-wide revoke
// can run on a snapshot that predates C's rotation insert, so the freshly minted
// token ESCAPES the revoke (AC5 broken under concurrency). With the lock the two
// are serialized either way, so AFTER both complete NO token on F is usable and
// neither request is a 500.
func TestRefresh_ConcurrentReplayNoEscape(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "noescape", "noescape-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	// Build family F: login mints O, one rotation consumes O and mints current C
	// (same family). Rewind O outside the grace window so reusing it is a replay.
	o := e.login(t, user, pw)
	recRot, c := e.refresh(t, o.RefreshToken)
	if recRot.Code != http.StatusOK {
		t.Fatalf("setup rotation status = %d, want 200", recRot.Code)
	}
	e.rewindUsedAt(t, ctx, selectorOf(t, o.RefreshToken))

	// Fire the rotation of the current token and the replay of the old token
	// concurrently.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	newRefresh := make([]string, 2) // any new token a 200 rotation produced
	wg.Add(2)
	go func() {
		defer wg.Done()
		rec, s := e.refresh(t, c.RefreshToken)
		codes[0] = rec.Code
		newRefresh[0] = s.RefreshToken
	}()
	go func() {
		defer wg.Done()
		rec, s := e.refresh(t, o.RefreshToken)
		codes[1] = rec.Code
		newRefresh[1] = s.RefreshToken
	}()
	wg.Wait()

	// Neither request may be a 500: the advisory lock must have serialized them, not
	// deadlocked or errored.
	for i, code := range codes {
		if code == http.StatusInternalServerError {
			t.Fatalf("concurrent request %d returned 500 (advisory lock should serialize, not error)", i)
		}
	}

	// Escape invariant: every token that could exist on F must now be unusable.
	// Candidates are C, O, and any new token a winning rotation minted. All must 401.
	candidates := []string{c.RefreshToken, o.RefreshToken}
	for _, nt := range newRefresh {
		if nt != "" {
			candidates = append(candidates, nt)
		}
	}
	for _, tok := range candidates {
		rec, _ := e.refresh(t, tok)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-race refresh of a family-F token: status = %d, want 401 (no token may escape the family revoke, AC5 under concurrency)", rec.Code)
		}
	}
}

// TestRefresh_ConcurrentDoubleReplayNoDeadlock is the BLOCKER fix's deadlock
// invariant. Two OLD used tokens O1, O2 on the SAME family, both rewound outside
// the grace window, are replayed concurrently. Each replay path runs
// RevokeTokenFamily over the same batch of family rows; without the family
// advisory lock serializing them, the two family-wide UPDATEs can lock the same
// rows in opposite order and DEADLOCK, surfacing as a 500. With the lock both
// serialize to a clean 401 and neither is a 500.
func TestRefresh_ConcurrentDoubleReplayNoDeadlock(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "doublereplay", "doublereplay-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	// Chain two rotations off one login so O1 and O2 are two consumed tokens on the
	// SAME family: login→O1, rotate O1→O2, rotate O2→current. Rewind O1 and O2 out
	// of grace so both reuse attempts are genuine replays.
	o1 := e.login(t, user, pw)
	rec1, o2 := e.refresh(t, o1.RefreshToken)
	if rec1.Code != http.StatusOK {
		t.Fatalf("setup rotation 1 status = %d, want 200", rec1.Code)
	}
	rec2, _ := e.refresh(t, o2.RefreshToken)
	if rec2.Code != http.StatusOK {
		t.Fatalf("setup rotation 2 status = %d, want 200", rec2.Code)
	}
	e.rewindUsedAt(t, ctx, selectorOf(t, o1.RefreshToken))
	e.rewindUsedAt(t, ctx, selectorOf(t, o2.RefreshToken))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	tokens := []string{o1.RefreshToken, o2.RefreshToken}
	wg.Add(2)
	for i := range tokens {
		go func(idx int) {
			defer wg.Done()
			rec, _ := e.refresh(t, tokens[idx])
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusUnauthorized {
			t.Fatalf("concurrent double-replay request %d: status = %d, want 401 (no DB deadlock/500 — advisory lock serializes the two family revokes)", i, code)
		}
	}
}

// TestRefresh_ExpiredReplayRevokesFamily is the MAJOR fix: the used_at (replay)
// check must run BEFORE the expired check. A token that is BOTH used (outside the
// grace window) AND past its expiry is still a replay signal; if expiry were judged
// first it would 401 WITHOUT revoking the family, letting a replayed-but-expired
// token slip past detection. Replaying such a token must 401 AND revoke the family,
// so a same-family sibling can no longer refresh.
func TestRefresh_ExpiredReplayRevokesFamily(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "expiredreplay", "expiredreplay-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	// login→O, rotate O→current sibling C (same family). Make O a used-and-expired
	// replay candidate: rewind used_at out of grace AND push expires_at into the past.
	o := e.login(t, user, pw)
	recRot, c := e.refresh(t, o.RefreshToken)
	if recRot.Code != http.StatusOK {
		t.Fatalf("setup rotation status = %d, want 200", recRot.Code)
	}
	oSelector := selectorOf(t, o.RefreshToken)
	e.rewindUsedAt(t, ctx, oSelector)
	if _, err := e.pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 hour' WHERE selector = $1`,
		oSelector,
	); err != nil {
		t.Fatalf("expire old token: %v", err)
	}

	// Replaying the used-and-expired token must revoke the family (used_at判定 fires
	// before the expired check), so the sibling C can no longer refresh.
	recReplay, _ := e.refresh(t, o.RefreshToken)
	if recReplay.Code != http.StatusUnauthorized {
		t.Fatalf("expired-replay status = %d, want 401", recReplay.Code)
	}
	recSibling, _ := e.refresh(t, c.RefreshToken)
	if recSibling.Code != http.StatusUnauthorized {
		t.Fatalf("post-expired-replay sibling refresh: status = %d, want 401 (family must be revoked — used_at judged before expired)", recSibling.Code)
	}
}

// TestBearer_RevocationImmediate (AC7) asserts revoking an access token makes the
// very next Bearer request 401, and revoking a refresh token's family makes the
// next refresh with it 401.
func TestBearer_RevocationImmediate(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "revoke", "revoke-pw-correct-horse"
	e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)

	// Access token works, then is revoked → next request 401 (AC7).
	if e.bearerStatus(sess.AccessToken) != http.StatusOK {
		t.Fatal("fresh access token should authenticate")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE access_tokens SET revoked_at = now()`); err != nil {
		t.Fatalf("revoke access token: %v", err)
	}
	if got := e.bearerStatus(sess.AccessToken); got != http.StatusUnauthorized {
		t.Fatalf("revoked access token: status = %d, want 401 (AC7 immediate)", got)
	}

	// Revoke the refresh family → refresh with it 401s.
	if _, err := e.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()`); err != nil {
		t.Fatalf("revoke refresh tokens: %v", err)
	}
	rec, _ := e.refresh(t, sess.RefreshToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked refresh token: status = %d, want 401", rec.Code)
	}
}

// TestDisabledUser_Rejected (AC20) asserts a disabled user's access token is
// rejected by Bearer and its refresh token is rejected on refresh, even though
// both credentials are otherwise valid and unexpired.
func TestDisabledUser_Rejected(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "disabled", "disabled-pw-correct-horse"
	uid := e.seedUser(t, ctx, user, pw)

	sess := e.login(t, user, pw)
	if e.bearerStatus(sess.AccessToken) != http.StatusOK {
		t.Fatal("active user's access token should authenticate before disabling")
	}

	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, uid); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if got := e.bearerStatus(sess.AccessToken); got != http.StatusUnauthorized {
		t.Fatalf("disabled user access token: status = %d, want 401 (AC20)", got)
	}
	rec, _ := e.refresh(t, sess.RefreshToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user refresh: status = %d, want 401 (AC20)", rec.Code)
	}
}

// mustReadAuthGooseSections reads the auth migration and returns its Up and Down
// SQL bodies, splitting on the goose section markers. The auth migration is plain
// DDL (no -- +goose StatementBegin/End blocks), so a marker split suffices.
func mustReadAuthGooseSections(t *testing.T) (up, down string) {
	t.Helper()
	abs, err := filepath.Abs(authMigrationRelPath)
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
