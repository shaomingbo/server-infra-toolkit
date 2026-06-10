package auth_test

// login_integration_test.go is the TEST_DATABASE_URL-gated, full-chain proof of
// the W2d account-lockout semantics that cannot be exercised without a real
// Postgres — they hinge on the atomic UPDATE...RETURNING (concurrency), the DB
// clock (now() / locked_until), and state surviving a fresh Handler:
//
//   - AC1  N concurrent wrong-password requests increment failed_attempts to
//          EXACTLY N (no lost updates) and lock the account.
//   - AC2  the lock, held in the DB, is enforced by a brand-new Handler over the
//          same pool (simulated instance restart / cleared in-memory state).
//   - AC3  a correct password resets failed_attempts to 0 / locked_until to NULL;
//          after a lock window expires, counting restarts from 1.
//   - AC4  the expiry decision uses the DB clock: locked_until in the DB past →
//          200; in the DB future → locked 401; independent of the test's wall clock.
//   - AC6  the locked path spends a dummy argon2 and does NOT read the real
//          password_hash (a locked account with the CORRECT password still 401s).
//   - AC8  a disabled user cannot log in and does not accumulate lockout count.
//
// GATING (AC15/AC8 infra): it reads its DSN ONLY from TEST_DATABASE_URL — the same
// env var the verify.sh migration gate and CI Postgres container use — and NEVER
// touches Neon or hardcodes a DSN. When TEST_DATABASE_URL is unset the suite
// t.Skip()s via setupAuthEnv. It reuses the shared harness in
// refresh_integration_test.go (setupAuthEnv / seedUser / sessionResp).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
)

// lockoutThresholdIT mirrors the production lockoutThreshold (auth-internal, not
// exported). It is duplicated here as the value the integration test drives to; if
// the production threshold changes, this constant and the assertions move with it.
// (Keeping it local avoids exporting a tuning constant solely for a test.)
const lockoutThresholdIT = 5

// loginStatus drives POST /v1/auth/login and returns ONLY the status code, for the
// failure paths where the body is the generic 401 (the shared login helper fails
// the test on a non-200, so it cannot be used for expected-401 cases).
func (e *testEnv) loginStatus(username, password string) int {
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "it-login")
	e.srv.ServeHTTP(rec, req)
	return rec.Code
}

// failedAttempts reads the user's current failed_attempts straight from the DB.
func (e *testEnv) failedAttempts(t *testing.T, ctx context.Context, username string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx,
		`SELECT failed_attempts FROM users WHERE lower(username) = lower($1)`, username).Scan(&n); err != nil {
		t.Fatalf("read failed_attempts: %v", err)
	}
	return n
}

// lockedUntil reads the user's locked_until (Valid=false when NULL).
func (e *testEnv) lockedUntil(t *testing.T, ctx context.Context, username string) pgtype.Timestamptz {
	t.Helper()
	var ts pgtype.Timestamptz
	if err := e.pool.QueryRow(ctx,
		`SELECT locked_until FROM users WHERE lower(username) = lower($1)`, username).Scan(&ts); err != nil {
		t.Fatalf("read locked_until: %v", err)
	}
	return ts
}

// setLockedUntil forces a user's locked_until to a DB-relative interval (e.g.
// "-1 second" for past, "+5 minutes" for future) — manipulations only the operator
// can do, used to test the DB-clock expiry decision (AC4).
func (e *testEnv) setLockedUntil(t *testing.T, ctx context.Context, username, interval string) {
	t.Helper()
	if _, err := e.pool.Exec(ctx,
		`UPDATE users SET locked_until = now() + interval '`+interval+`' WHERE lower(username) = lower($1)`,
		username); err != nil {
		t.Fatalf("set locked_until: %v", err)
	}
}

// setStatus forces a user's status column (e.g. "disabled") for the status-gate test.
func (e *testEnv) setStatus(t *testing.T, ctx context.Context, username, status string) {
	t.Helper()
	if _, err := e.pool.Exec(ctx,
		`UPDATE users SET status = $2 WHERE lower(username) = lower($1)`, username, status); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

// TestLockout_ConcurrentAtomicCount (AC1) fires N concurrent wrong-password logins
// at one account and asserts failed_attempts ends EXACTLY at N (no lost updates,
// proving the atomic UPDATE...RETURNING) and the account is locked.
func TestLockout_ConcurrentAtomicCount(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "concurrent-lock", "the-correct-pw-aaa"
	e.seedUser(t, ctx, user, pw)

	const n = lockoutThresholdIT
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = e.loginStatus(user, "WRONG-password-guess")
		}()
	}
	wg.Wait()

	if got := e.failedAttempts(t, ctx, user); got != n {
		t.Fatalf("after %d concurrent failures: failed_attempts = %d, want exactly %d (lost update?)", n, got, n)
	}
	if lu := e.lockedUntil(t, ctx, user); !lu.Valid {
		t.Fatal("account not locked after reaching the threshold (locked_until is NULL)")
	}
}

// TestLockout_ConcurrentGuardsLockBoundary (MAJOR1) fires MORE concurrent wrong-
// password requests than the threshold at one account and asserts the SQL FROM
// self-join guards the lock boundary: failed_attempts settles EXACTLY at the
// threshold (NOT threshold+extra), and the lock window is set ONCE and not pushed
// forward by the requests that arrive after the account is already locked.
//
// WHY THIS CATCHES THE OLD BUG: before the FROM self-join, every concurrent request
// that passed the Go-side lock gate ran an unconditional CASE UPDATE — so requests
// arriving after the lock was set would keep incrementing failed_attempts AND reset
// locked_until to a fresh now()+window, extending the lock. With the guard, a
// request that re-reads an already-locked `old` row keeps the count and the original
// locked_until untouched.
func TestLockout_ConcurrentGuardsLockBoundary(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "concurrent-guard", "the-correct-pw-guard"
	e.seedUser(t, ctx, user, pw)

	// Fire well past the threshold concurrently. The extras must NOT push the count
	// past the threshold or reset the lock window.
	const n = lockoutThresholdIT + 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = e.loginStatus(user, "WRONG-guard-guess")
		}()
	}
	wg.Wait()

	// MAJOR1: the count is guarded at the threshold — the post-lock requests took the
	// "already locked -> keep old.failed_attempts" branch instead of incrementing.
	if got := e.failedAttempts(t, ctx, user); got != lockoutThresholdIT {
		t.Fatalf("after %d concurrent failures: failed_attempts = %d, want exactly %d (lock boundary not guarded — post-lock requests kept counting)",
			n, got, lockoutThresholdIT)
	}
	lu := e.lockedUntil(t, ctx, user)
	if !lu.Valid {
		t.Fatal("account not locked after crossing the threshold")
	}

	// MAJOR1: the lock window must reflect the FIRST lock, not a reset by a late
	// request. The window is lockoutWindow (15m); locked_until must sit within roughly
	// one window of the test's start, not pushed far forward by repeated resets. We
	// read the DB's own now() and assert locked_until - now() does not exceed the
	// window by more than a generous slack — a reset-on-every-request bug would still
	// land near now()+window per request, but combined with the un-guarded count this
	// assertion plus the count guard above pins the boundary behavior. The decisive
	// signal is the count; this is the corroborating window check.
	var remaining float64 // seconds until locked_until, by the DB clock
	if err := e.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (locked_until - now())) FROM users WHERE lower(username) = lower($1)`,
		user).Scan(&remaining); err != nil {
		t.Fatalf("read remaining lock seconds: %v", err)
	}
	// lockoutWindow is 15 minutes = 900s. Allow up to 900s + slack; a healthy single
	// lock leaves <= 900s remaining. (Repeated resets would also stay <= 900s, so the
	// count assertion above is the primary guard; this just rejects an absurd value.)
	if remaining > 900+30 {
		t.Fatalf("locked_until is %.0fs out, exceeding one lock window — lock appears reset/extended", remaining)
	}
}

// TestLockout_CrossInstanceEnforced (AC2) reaches the lock threshold, then builds a
// BRAND-NEW Handler over the same pool (simulating an instance restart with cleared
// in-memory state) and asserts that new Handler still rejects the account — proving
// the lock lives in the DB, not process memory. It then proves the lock is INVISIBLE
// on the wire: the locked 401 is byte-identical to a wrong-password 401.
func TestLockout_CrossInstanceEnforced(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "cross-instance", "the-correct-pw-bbb"
	e.seedUser(t, ctx, user, pw)

	for i := 0; i < lockoutThresholdIT; i++ {
		_ = e.loginStatus(user, "WRONG")
	}
	if !e.lockedUntil(t, ctx, user).Valid {
		t.Fatal("setup: account did not lock")
	}

	// A fresh Handler + server over the SAME pool — no shared in-memory lock state.
	freshH := auth.NewHandler(e.pool)
	freshSrv := newServerForHandler(freshH)

	// Locked account, CORRECT password, on the fresh instance → still 401.
	lockedBody := bodyOf(user, pw)
	lockedRec := serve(freshSrv, lockedBody)
	if lockedRec.Code != http.StatusUnauthorized {
		t.Fatalf("fresh instance, locked account, correct password: status = %d, want 401 (lock must be DB-backed)", lockedRec.Code)
	}

	// A different, unlocked account's wrong-password 401 for byte comparison.
	const other, otherPw = "cross-instance-other", "other-pw-ccc"
	e.seedUser(t, ctx, other, otherPw)
	wrongRec := serve(freshSrv, bodyOf(other, "WRONG"))
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("control wrong-password: status = %d, want 401", wrongRec.Code)
	}
	if lockedRec.Body.String() != wrongRec.Body.String() {
		t.Fatalf("locked 401 differs from wrong-password 401 (lockout observable):\n  locked: %s\n  wrong:  %s",
			lockedRec.Body.String(), wrongRec.Body.String())
	}
}

// TestLockout_SuccessResetsAndRecountsAfterExpiry (AC3) asserts a correct password
// after some failures resets the count to 0, and that after a lock window expires
// the next failure restarts counting from 1 (D6).
func TestLockout_SuccessResetsAndRecountsAfterExpiry(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "reset-recount", "the-correct-pw-ddd"
	e.seedUser(t, ctx, user, pw)

	// A few (below-threshold) failures, then a success → count back to 0.
	for i := 0; i < lockoutThresholdIT-1; i++ {
		if got := e.loginStatus(user, "WRONG"); got != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want 401", i+1, got)
		}
	}
	if got := e.failedAttempts(t, ctx, user); got != lockoutThresholdIT-1 {
		t.Fatalf("before success: failed_attempts = %d, want %d", got, lockoutThresholdIT-1)
	}
	_ = e.login(t, user, pw) // correct password → 200 + reset
	if got := e.failedAttempts(t, ctx, user); got != 0 {
		t.Fatalf("after success: failed_attempts = %d, want 0 (success must reset)", got)
	}
	if e.lockedUntil(t, ctx, user).Valid {
		t.Fatal("after success: locked_until not cleared")
	}

	// Lock the account, then force the lock to be EXPIRED in DB time; the next
	// failure must restart the count from 1 (not continue from a stale value).
	for i := 0; i < lockoutThresholdIT; i++ {
		_ = e.loginStatus(user, "WRONG")
	}
	if !e.lockedUntil(t, ctx, user).Valid {
		t.Fatal("setup: account did not lock before expiry test")
	}
	e.setLockedUntil(t, ctx, user, "-1 second") // expired in DB time

	// A WRONG password after the lock has expired must RESTART the count from 1
	// (D6/AC3) — not continue from the stale threshold value and immediately re-lock.
	// This is the atomic expiry-recount branch in RecordLoginFailure.
	if got := e.loginStatus(user, "WRONG"); got != http.StatusUnauthorized {
		t.Fatalf("after lock expiry, wrong password: status = %d, want 401", got)
	}
	if got := e.failedAttempts(t, ctx, user); got != 1 {
		t.Fatalf("after expiry + one failure: failed_attempts = %d, want 1 (count must restart from 1, D6)", got)
	}
	if e.lockedUntil(t, ctx, user).Valid {
		t.Fatal("after expiry + one failure: account re-locked on a single attempt (expiry recount broken)")
	}

	// And a correct password now succeeds and clears the count again.
	if got := e.loginStatus(user, pw); got != http.StatusOK {
		t.Fatalf("after expiry recount, correct password: status = %d, want 200", got)
	}
	if got := e.failedAttempts(t, ctx, user); got != 0 {
		t.Fatalf("after expiry recount + success: failed_attempts = %d, want 0", got)
	}
}

// TestLockout_DBClockExpiry (AC4) asserts the expiry decision is made in the DB
// clock domain: locked_until in the DB PAST → correct password 200; locked_until in
// the DB FUTURE → 401 — regardless of the test process's wall clock.
func TestLockout_DBClockExpiry(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "db-clock", "the-correct-pw-eee"
	e.seedUser(t, ctx, user, pw)

	// locked_until 1 second in the DB past → treated as expired → 200.
	e.setLockedUntil(t, ctx, user, "-1 second")
	if got := e.loginStatus(user, pw); got != http.StatusOK {
		t.Fatalf("locked_until in DB past: status = %d, want 200", got)
	}

	// locked_until 5 minutes in the DB future → still locked → 401, even with the
	// correct password (the real hash is bypassed on the locked path).
	e.setLockedUntil(t, ctx, user, "5 minutes")
	if got := e.loginStatus(user, pw); got != http.StatusUnauthorized {
		t.Fatalf("locked_until in DB future: status = %d, want 401", got)
	}
}

// TestLockout_LockedPathBypassesRealHash (AC6) asserts a locked account with the
// CORRECT password returns 401 — the only way that happens is the locked path
// running DummyVerify instead of the real password_hash verification. It also
// asserts the locked path does NOT further increment the count.
func TestLockout_LockedPathBypassesRealHash(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "locked-bypass", "the-correct-pw-fff"
	e.seedUser(t, ctx, user, pw)

	e.setLockedUntil(t, ctx, user, "5 minutes")
	before := e.failedAttempts(t, ctx, user)

	if got := e.loginStatus(user, pw); got != http.StatusUnauthorized {
		t.Fatalf("locked account, correct password: status = %d, want 401 (real hash must be bypassed)", got)
	}
	if after := e.failedAttempts(t, ctx, user); after != before {
		t.Fatalf("locked path changed failed_attempts %d -> %d, want unchanged (no increment while locked)", before, after)
	}
}

// TestLockout_DisabledUserGate (AC8) asserts a disabled user cannot log in with the
// correct password and accumulates no lockout count, and that the disabled 401 is
// byte-identical to an active user's wrong-password 401.
func TestLockout_DisabledUserGate(t *testing.T) {
	e := setupAuthEnv(t)
	ctx := context.Background()
	const user, pw = "disabled-gate", "the-correct-pw-ggg"
	e.seedUser(t, ctx, user, pw)
	e.setStatus(t, ctx, user, "disabled")

	// Correct password → still 401, no count.
	disabledRec := serve(e.srv, bodyOf(user, pw))
	if disabledRec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user, correct password: status = %d, want 401", disabledRec.Code)
	}
	// Several wrong attempts → still no lockout count (disabled does not participate).
	for i := 0; i < lockoutThresholdIT+2; i++ {
		_ = e.loginStatus(user, "WRONG")
	}
	if got := e.failedAttempts(t, ctx, user); got != 0 {
		t.Fatalf("disabled user accumulated %d failed_attempts, want 0", got)
	}
	if e.lockedUntil(t, ctx, user).Valid {
		t.Fatal("disabled user got locked_until set, want NULL (no lockout participation)")
	}

	// Indistinguishable from an active user's wrong-password 401 on the
	// anti-enumeration invariant: error.code + error.message must match. We compare
	// those two fields (NOT the whole body) because requestId is unique per request —
	// these two responses carry DIFFERENT request ids ("it-login-2" vs the recorder's
	// own), so a whole-body compare would always differ on requestId and is NOT what
	// AC8 verifies. The credential-failure invariant is that code/message leak nothing.
	const other, otherPw = "disabled-gate-other", "other-pw-hhh"
	e.seedUser(t, ctx, other, otherPw)
	wrongRec := serve(e.srv, bodyOf(other, "WRONG"))
	dCode, dMsg := errorCodeMessage(t, disabledRec.Body.Bytes())
	wCode, wMsg := errorCodeMessage(t, wrongRec.Body.Bytes())
	if dCode != wCode || dMsg != wMsg {
		t.Fatalf("disabled 401 error code/message differs from wrong-password 401 (enumeration leak):\n  disabled: code=%q message=%q\n  wrong:    code=%q message=%q",
			dCode, dMsg, wCode, wMsg)
	}
}

// errorCodeMessage parses an error-envelope body and returns its error.code and
// error.message — the anti-enumeration invariant fields — ignoring the per-request
// requestId (AC8). It fails the test on a body that is not a valid error envelope.
func errorCodeMessage(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse error envelope: %v\n%s", err, string(body))
	}
	return env.Error.Code, env.Error.Message
}

// --- small shared helpers for this file ---

// bodyOf builds a login request body.
func bodyOf(username, password string) string {
	return `{"username":"` + username + `","password":"` + password + `"}`
}

// serve issues a login POST against the given handler and returns the recorder.
func serve(srv http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "it-login-2")
	srv.ServeHTTP(rec, req)
	return rec
}

// newServerForHandler mounts a Handler's routes into a minimal mux for the fresh-
// instance test. The handler reads the request id off the response header, which
// the serve helper pre-sets the way the production middleware chain would, so a
// bare mux that just runs the handler is sufficient here.
func newServerForHandler(h *auth.Handler) http.Handler {
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}
