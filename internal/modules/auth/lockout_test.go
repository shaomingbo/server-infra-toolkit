package auth

// lockout_test.go covers the account-lockout lifecycle at the unit level, over the
// fakeStore (no real DB): the atomic-failure increment is CALLED on a wrong
// password (FR1), a locked account is rejected before its real hash with NO
// increment (FR2/FR5), a successful login resets the count (FR3), the expiry
// decision uses the DB clock not the app clock (FR4/D5), the status gate rejects a
// disabled user without counting (FR6/AC8), and no failure path ever emits a 429 /
// Retry-After (FR5/D3/AC5). The DB-backed concurrency/cross-instance proofs
// (AC1/AC2) live in the TEST_DATABASE_URL-gated integration test.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// storeOf builds a fakeStore holding a single user row keyed by its lowercased
// username, ready to be tweaked (lock, status) by the caller before use.
func storeOf(username string, row dbgen.GetUserByUsernameRow) *fakeStore {
	return &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): row}}
}

// TestLockout_WrongPasswordIncrements (FR1) asserts a wrong password on an active,
// not-locked user drives exactly one atomic failure increment and no reset.
func TestLockout_WrongPasswordIncrements(t *testing.T) {
	const username, password = "Dave", "right-password-value"
	s := storeOf(username, testUser(t, username, password))
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Dave","password":"WRONG"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(s.failures) != 1 {
		t.Fatalf("recordLoginFailure called %d times, want 1", len(s.failures))
	}
	if len(s.resets) != 0 {
		t.Fatalf("resetLoginFailures called %d times on a failure, want 0", len(s.resets))
	}
}

// TestLockout_SuccessResets (FR3/AC3) asserts a correct password clears the failure
// count (resetLoginFailures called once) and does NOT increment.
func TestLockout_SuccessResets(t *testing.T) {
	const username, password = "Erin", "correct-pw-erin-123"
	s := storeOf(username, testUser(t, username, password))
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Erin","password":"correct-pw-erin-123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(s.resets) != 1 {
		t.Fatalf("resetLoginFailures called %d times on success, want 1", len(s.resets))
	}
	if len(s.failures) != 0 {
		t.Fatalf("recordLoginFailure called %d times on success, want 0", len(s.failures))
	}
}

// TestLockout_LockedRejectsEvenCorrectPassword (FR2/FR5/AC5) asserts a locked
// account returns 401 even with the CORRECT password — proving the real hash is
// bypassed (a correct password would otherwise succeed) — and that the locked path
// does NOT increment the failure count (it is already locked).
func TestLockout_LockedRejectsEvenCorrectPassword(t *testing.T) {
	const username, password = "Frank", "frank-correct-pw-456"
	row := testUser(t, username, password)
	// Lock the account: locked_until is 5 minutes in the FUTURE relative to db_now.
	row.DbNow = pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
	row.LockedUntil = pgtype.Timestamptz{Time: time.Unix(1_700_000_300, 0), Valid: true}
	s := storeOf(username, row)
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Frank","password":"frank-correct-pw-456"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked account with correct password: status = %d, want 401 (real hash must be bypassed)", rec.Code)
	}
	if len(s.failures) != 0 {
		t.Fatalf("locked path incremented failure count %d times, want 0 (already locked)", len(s.failures))
	}
	if len(s.persistRows) != 0 {
		t.Fatal("locked account issued a session — must never authenticate while locked")
	}
}

// TestLockout_ExpiredLockUsesDBClock (FR4/D5/AC4) asserts the expiry decision
// compares locked_until against the DB clock (db_now), not the test process's
// time.Now(). A locked_until that is in the PAST relative to db_now — even if it is
// in the FUTURE relative to the real wall clock — is treated as expired, so a
// correct password succeeds.
func TestLockout_ExpiredLockUsesDBClock(t *testing.T) {
	const username, password = "Gail", "gail-correct-pw-789"
	row := testUser(t, username, password)
	// db_now is FAR in the future (year ~2033); locked_until is "now-ish" in real
	// wall-clock terms but PAST relative to db_now. App time.Now() would wrongly see
	// the lock as still active; db_now correctly sees it expired.
	row.DbNow = pgtype.Timestamptz{Time: time.Now().Add(200 * 365 * 24 * time.Hour), Valid: true}
	row.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true}
	s := storeOf(username, row)
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Gail","password":"gail-correct-pw-789"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expired-by-DB-clock lock: status = %d, want 200 (expiry must use db_now, not app time.Now)", rec.Code)
	}
}

// TestLockout_FutureLockByDBClock is the mirror of the above: a locked_until in the
// PAST relative to real wall-clock but in the FUTURE relative to db_now must still
// be treated as LOCKED — again proving db_now drives the decision, not app time.
func TestLockout_FutureLockByDBClock(t *testing.T) {
	const username, password = "Hank", "hank-correct-pw-abc"
	row := testUser(t, username, password)
	// db_now is FAR in the past; locked_until is "a minute ago" in real terms but in
	// the FUTURE relative to db_now. App time.Now() would wrongly see it expired.
	row.DbNow = pgtype.Timestamptz{Time: time.Now().Add(-200 * 365 * 24 * time.Hour), Valid: true}
	row.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}
	s := storeOf(username, row)
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Hank","password":"hank-correct-pw-abc"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("future-by-DB-clock lock: status = %d, want 401 (expiry must use db_now, not app time.Now)", rec.Code)
	}
}

// TestStatusGate_DisabledUserRejected (FR6/AC8) asserts a disabled user cannot log
// in with the CORRECT password (returns 401), and a disabled user's failures do NOT
// increment the lockout count (a disabled account does not participate in lockout).
func TestStatusGate_DisabledUserRejected(t *testing.T) {
	const username, password = "Iris", "iris-correct-pw-def"

	t.Run("correct password still 401", func(t *testing.T) {
		row := testUser(t, username, password)
		row.Status = "disabled"
		s := storeOf(username, row)
		h := newTestHandler(s)

		rec := doLogin(h, `{"username":"Iris","password":"iris-correct-pw-def"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("disabled user with correct password: status = %d, want 401", rec.Code)
		}
		if len(s.persistRows) != 0 {
			t.Fatal("disabled user issued a session")
		}
		if len(s.failures) != 0 {
			t.Fatalf("disabled user incremented lockout count %d times, want 0", len(s.failures))
		}
	})

	t.Run("wrong password does not count", func(t *testing.T) {
		row := testUser(t, username, password)
		row.Status = "disabled"
		s := storeOf(username, row)
		h := newTestHandler(s)

		rec := doLogin(h, `{"username":"Iris","password":"WRONG"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("disabled user with wrong password: status = %d, want 401", rec.Code)
		}
		if len(s.failures) != 0 {
			t.Fatalf("disabled user (wrong pw) incremented lockout count %d times, want 0", len(s.failures))
		}
	})
}

// TestLockout_DisabledByteIdenticalToWrongPassword (AC8) asserts a disabled user
// with a correct password returns a response BYTE-IDENTICAL to an active user with
// a wrong password — the disabled state is not observable on the wire.
func TestLockout_DisabledByteIdenticalToWrongPassword(t *testing.T) {
	const password = "shared-correct-pw-ghi"

	// Active user, wrong password.
	activeRow := testUser(t, "Jack", password)
	activeStore := storeOf("Jack", activeRow)
	activeRec := doLogin(newTestHandler(activeStore), `{"username":"Jack","password":"WRONG"}`)

	// Disabled user, correct password.
	disabledRow := testUser(t, "Jill", password)
	disabledRow.Status = "disabled"
	disabledStore := storeOf("Jill", disabledRow)
	disabledRec := doLogin(newTestHandler(disabledStore), `{"username":"Jill","password":"shared-correct-pw-ghi"}`)

	if activeRec.Code != http.StatusUnauthorized || disabledRec.Code != http.StatusUnauthorized {
		t.Fatalf("statuses: active-wrong=%d disabled-correct=%d, want both 401", activeRec.Code, disabledRec.Code)
	}
	if activeRec.Body.String() != disabledRec.Body.String() {
		t.Fatalf("disabled state observable on the wire:\n  active-wrong:    %s\n  disabled-correct:%s",
			activeRec.Body.String(), disabledRec.Body.String())
	}
}

// TestLockout_NoRetryAfterOn401 (FR5/D3/AC5) asserts NO failure path — locked,
// wrong password, no such user, disabled — ever sets a Retry-After header or
// returns 429. The lockout must be invisible on the wire.
func TestLockout_NoRetryAfterOn401(t *testing.T) {
	const username, password = "Kira", "kira-correct-pw-jkl"

	// Locked account.
	lockedRow := testUser(t, username, password)
	lockedRow.DbNow = pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
	lockedRow.LockedUntil = pgtype.Timestamptz{Time: time.Unix(1_700_000_300, 0), Valid: true}

	cases := map[string]struct {
		store *fakeStore
		body  string
	}{
		"locked":       {storeOf(username, lockedRow), `{"username":"Kira","password":"kira-correct-pw-jkl"}`},
		"wrong-pw":     {storeOf(username, testUser(t, username, password)), `{"username":"Kira","password":"WRONG"}`},
		"no-such-user": {&fakeStore{users: map[string]dbgen.GetUserByUsernameRow{}}, `{"username":"ghost","password":"whatever"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doLogin(newTestHandler(c.store), c.body)
			if rec.Code == http.StatusTooManyRequests {
				t.Fatalf("%s returned 429 — lockout must reuse 401, never 429 (D3)", name)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want 401", name, rec.Code)
			}
			if ra := rec.Header().Get("Retry-After"); ra != "" {
				t.Fatalf("%s set Retry-After=%q — must never reveal a lockout/backoff (D3)", name, ra)
			}
			if strings.Contains(rec.Body.String(), "rate_limited") || strings.Contains(rec.Body.String(), "locked") {
				t.Fatalf("%s body leaks lockout state: %s", name, rec.Body.String())
			}
		})
	}
}

// TestLockout_FailureCounterErrorIs500 asserts a DB error from the atomic failure
// counter surfaces as a generic 500 (the failure that should have counted toward
// lockout is not silently dropped), with no leak of the cause.
func TestLockout_FailureCounterErrorIs500(t *testing.T) {
	const username, password = "Liam", "liam-correct-pw-mno"
	s := storeOf(username, testUser(t, username, password))
	s.failErr = errStub("counter update failed")
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Liam","password":"WRONG"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "counter update failed") {
		t.Fatalf("500 body leaks internal error: %s", rec.Body.String())
	}
}

// TestTimingAlignment_EveryFailurePathDoesOneEquivalentDBWrite (BLOCKER1) asserts the
// anti-enumeration DB-write alignment: each of the four failure paths performs EXACTLY
// one equivalent primary-key UPDATE so the no-user / locked / disabled paths are not
// faster (no DB write) than the wrong-password path. no-user / locked / disabled go
// through touchUserTiming; wrong-password goes through recordLoginFailure. Without the
// touchUserTiming writes, three of the four paths would skip the DB entirely and leak
// username existence by timing.
func TestTimingAlignment_EveryFailurePathDoesOneEquivalentDBWrite(t *testing.T) {
	const username, password = "Mona", "mona-correct-pw-pqr"

	t.Run("no-such-user touches once", func(t *testing.T) {
		s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{}}
		rec := doLogin(newTestHandler(s), `{"username":"ghost","password":"whatever"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if len(s.touches) != 1 {
			t.Fatalf("no-user path did %d timing UPDATEs, want exactly 1 (BLOCKER1)", len(s.touches))
		}
		if len(s.failures) != 0 {
			t.Fatalf("no-user path recorded %d failures, want 0 (no account to count)", len(s.failures))
		}
		// The no-user touch targets a zero-value (all-zero) UUID, matching 0 rows.
		if s.touches[0].Bytes != ([16]byte{}) || !s.touches[0].Valid {
			t.Fatalf("no-user touch UUID = %v (valid=%v), want the Valid all-zero UUID", s.touches[0].Bytes, s.touches[0].Valid)
		}
	})

	t.Run("locked touches once, no count", func(t *testing.T) {
		row := testUser(t, username, password)
		row.DbNow = pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
		row.LockedUntil = pgtype.Timestamptz{Time: time.Unix(1_700_000_300, 0), Valid: true}
		s := storeOf(username, row)
		rec := doLogin(newTestHandler(s), `{"username":"Mona","password":"mona-correct-pw-pqr"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if len(s.touches) != 1 {
			t.Fatalf("locked path did %d timing UPDATEs, want exactly 1 (BLOCKER1)", len(s.touches))
		}
		if len(s.failures) != 0 {
			t.Fatalf("locked path recorded %d failures, want 0 (already locked)", len(s.failures))
		}
		if s.touches[0].Bytes != row.ID.Bytes {
			t.Fatal("locked touch did not target the real user id")
		}
	})

	t.Run("disabled touches once, no count", func(t *testing.T) {
		row := testUser(t, username, password)
		row.Status = "disabled"
		s := storeOf(username, row)
		rec := doLogin(newTestHandler(s), `{"username":"Mona","password":"mona-correct-pw-pqr"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if len(s.touches) != 1 {
			t.Fatalf("disabled path did %d timing UPDATEs, want exactly 1 (BLOCKER1)", len(s.touches))
		}
		if len(s.failures) != 0 {
			t.Fatalf("disabled path recorded %d failures, want 0 (does not participate in lockout)", len(s.failures))
		}
	})

	t.Run("wrong-password records one failure, no extra touch", func(t *testing.T) {
		s := storeOf(username, testUser(t, username, password))
		rec := doLogin(newTestHandler(s), `{"username":"Mona","password":"WRONG"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if len(s.failures) != 1 {
			t.Fatalf("wrong-password path recorded %d failures, want exactly 1 (its equivalent DB write)", len(s.failures))
		}
		if len(s.touches) != 0 {
			t.Fatalf("wrong-password path did %d timing UPDATEs, want 0 (recordLoginFailure is its write)", len(s.touches))
		}
	})

	t.Run("success resets once, no touch", func(t *testing.T) {
		s := storeOf(username, testUser(t, username, password))
		rec := doLogin(newTestHandler(s), `{"username":"Mona","password":"mona-correct-pw-pqr"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(s.resets) != 1 {
			t.Fatalf("success path did %d resets, want exactly 1 (its equivalent DB write)", len(s.resets))
		}
		if len(s.touches) != 0 {
			t.Fatalf("success path did %d timing UPDATEs, want 0 (resetLoginFailures is its write)", len(s.touches))
		}
	})
}

// TestTimingAlignment_NoUserTouchErrorIs500 asserts a DB error from the no-user
// timing UPDATE surfaces as a generic 500 rather than letting the no-user path become
// a faster, observable channel (BLOCKER1): if the touch is dropped, the no-user path
// would skip its DB write and answer faster than wrong-password.
func TestTimingAlignment_NoUserTouchErrorIs500(t *testing.T) {
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{}, touchErr: errStub("touch failed")}
	rec := doLogin(newTestHandler(s), `{"username":"ghost","password":"whatever"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "touch failed") {
		t.Fatalf("500 body leaks internal error: %s", rec.Body.String())
	}
}

// TestAccountLocked_OnlyJustLockedRequestLogs (MAJOR2) asserts the account_locked
// event is driven by the query's just_locked flag, emitted EXACTLY once: the single
// request that crosses the threshold logs, and additional wrong-password requests
// after the lock is set do NOT re-log (their recordLoginFailure returns
// just_locked=false). This pins the Go-side `if res.JustLocked` wiring; the SQL FROM
// self-join that produces just_locked exactly once under real concurrency is proven
// by the DB-backed integration tests.
func TestAccountLocked_OnlyJustLockedRequestLogs(t *testing.T) {
	buf := captureLockoutEvents(t)

	const username, password = "Nora", "nora-correct-pw-stu"
	s := storeOf(username, testUser(t, username, password))
	h := newTestHandler(s)

	// Drive MORE than the threshold of wrong-password attempts. The fakeStore models
	// just_locked=true only on the crossing call; every later call returns
	// just_locked=false, so only one event must be emitted regardless of count.
	for i := 0; i < lockoutThreshold+3; i++ {
		_ = doLogin(h, `{"username":"Nora","password":"WRONG"}`)
	}

	if lines := nonEmptyLines(buf.String()); len(lines) != 1 {
		t.Fatalf("emitted %d account_locked lines across %d over-threshold failures, want exactly 1 (only the just_locked request logs, MAJOR2):\n%s",
			len(lines), lockoutThreshold+3, buf.String())
	}
}
