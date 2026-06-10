package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// fakeStore is an in-memory store for driving the login flow without a database.
// It maps lowercased usernames to user rows (with a controllable db_now and
// locked_until so lockout/expiry can be exercised deterministically) and records
// persisted sessions, failure increments, and resets.
type fakeStore struct {
	users       map[string]dbgen.GetUserByUsernameRow
	lookupErr   error // if set, userByUsername returns this (non-ErrNoRows) error
	persistErr  error // if set, persistSession returns this error
	failErr     error // if set, recordLoginFailure returns this error
	resetErr    error // if set, resetLoginFailures returns this error
	persistRows []sessionRows

	// recorded calls, for asserting the lockout lifecycle without a real DB.
	failures []pgtype.UUID // userID per recordLoginFailure call (order = call order)
	resets   []pgtype.UUID // userID per resetLoginFailures call
	touches  []pgtype.UUID // userID per touchUserTiming call (BLOCKER1 timing alignment)
	touchErr error         // if set, touchUserTiming returns this error

	// failCounter tracks the simulated failed_attempts per user across
	// recordLoginFailure calls, so the fake returns a monotonically increasing
	// count and flips locked_until at the threshold like the real atomic UPDATE.
	failCounter map[[16]byte]int32
}

func (s *fakeStore) userByUsername(_ context.Context, lower string) (dbgen.GetUserByUsernameRow, error) {
	if s.lookupErr != nil {
		return dbgen.GetUserByUsernameRow{}, s.lookupErr
	}
	u, ok := s.users[lower]
	if !ok {
		return dbgen.GetUserByUsernameRow{}, pgx.ErrNoRows
	}
	return u, nil
}

func (s *fakeStore) recordLoginFailure(_ context.Context, userID pgtype.UUID, threshold int32, window time.Duration) (dbgen.RecordLoginFailureRow, error) {
	if s.failErr != nil {
		return dbgen.RecordLoginFailureRow{}, s.failErr
	}
	s.failures = append(s.failures, userID)
	if s.failCounter == nil {
		s.failCounter = map[[16]byte]int32{}
	}
	prev := s.failCounter[userID.Bytes]
	s.failCounter[userID.Bytes]++
	n := s.failCounter[userID.Bytes]
	row := dbgen.RecordLoginFailureRow{FailedAttempts: n}
	// Mimic the real CASE + just_locked: set locked_until once the new count reaches
	// threshold, and report JustLocked=true ONLY on the single call that crosses from
	// below the threshold to at/above it (prev < threshold <= n), matching the SQL's
	// FROM self-join that returns just_locked exactly once (MAJOR2). A later call after
	// the lock is already set returns just_locked=false.
	if n >= threshold {
		row.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(window), Valid: true}
		row.JustLocked = prev < threshold
	}
	return row, nil
}

func (s *fakeStore) resetLoginFailures(_ context.Context, userID pgtype.UUID) error {
	if s.resetErr != nil {
		return s.resetErr
	}
	s.resets = append(s.resets, userID)
	if s.failCounter != nil {
		delete(s.failCounter, userID.Bytes)
	}
	return nil
}

func (s *fakeStore) touchUserTiming(_ context.Context, userID pgtype.UUID) error {
	if s.touchErr != nil {
		return s.touchErr
	}
	s.touches = append(s.touches, userID)
	return nil
}

func (s *fakeStore) persistSession(_ context.Context, rows sessionRows) error {
	if s.persistErr != nil {
		return s.persistErr
	}
	s.persistRows = append(s.persistRows, rows)
	return nil
}

// testUserID is the fixed, recognizable UUID every test user carries so userId
// assertions are deterministic.
func testUserID() pgtype.UUID {
	var id pgtype.UUID
	copy(id.Bytes[:], []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})
	id.Valid = true
	return id
}

// testUser builds an ACTIVE user row with a real argon2id hash of password (so
// Verify on the happy path runs the actual KDF as in production) and a db_now of
// the current time with no lock set. Tests that need a lock or a disabled status
// adjust the returned row's fields before storing it.
func testUser(t *testing.T, username, password string) dbgen.GetUserByUsernameRow {
	t.Helper()
	hash, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash seed password: %v", err)
	}
	return dbgen.GetUserByUsernameRow{
		ID:           testUserID(),
		Username:     username,
		PasswordHash: hash,
		Status:       userStatusActive,
		DbNow:        pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

// newTestHandler wires a Handler over the given fake store with the no-op rate
// limiter (so the login flow's facade call site does not nil-panic). db/q are nil
// because the login flow only touches store; nothing in these tests calls the
// refresh or Bearer seams that would use them.
func newTestHandler(s store) *Handler {
	return &Handler{store: s, rateLimiter: noopRateLimiter{}}
}

// doLogin issues a POST /v1/auth/login with rawBody and returns the recorder. The
// X-Request-Id header is pre-set the way the middleware would, so writeError can
// echo it.
func doLogin(h *Handler, rawBody string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(rawBody))
	rec := httptest.NewRecorder()
	rec.Header().Set(requestIDHeader, "req-test-123")
	h.login(rec, req)
	return rec
}

// TestLogin_Success (AC1) asserts a correct credential returns 200 with a
// LoginSession whose four fields are present and camelCase, whose accessToken
// decodes to >= 32 bytes, whose userId matches the user, and whose expiresAt is a
// plausible Unix MILLISECOND value (int64). It also asserts a session row was
// persisted.
func TestLogin_Success(t *testing.T) {
	const username, password = "Alice", "correct horse battery staple"
	user := testUser(t, username, password)
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): user}}
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"Alice","password":"correct horse battery staple"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	// Decode into a map to assert the EXACT camelCase keys the client expects.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"userId", "accessToken", "refreshToken", "expiresAt"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response missing required field %q; got keys %v", key, keysOf(raw))
		}
	}
	if len(raw) != 4 {
		t.Fatalf("response has %d fields, want exactly 4: %v", len(raw), keysOf(raw))
	}

	var sess loginSession
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode LoginSession: %v", err)
	}
	if sess.UserID != uuidToString(user.ID) {
		t.Fatalf("userId = %q, want %q", sess.UserID, uuidToString(user.ID))
	}
	accRaw, err := base64.RawURLEncoding.DecodeString(sess.AccessToken)
	if err != nil {
		t.Fatalf("accessToken not base64url: %v", err)
	}
	if len(accRaw) < 32 {
		t.Fatalf("accessToken decodes to %d bytes, want >= 32", len(accRaw))
	}
	if !strings.Contains(sess.RefreshToken, ".") {
		t.Fatalf("refreshToken %q is not a split token", sess.RefreshToken)
	}
	// expiresAt must be a Unix MILLISECOND value: ~13 digits for 2020s, and far
	// larger than the seconds value would be (a seconds value would be ~10 digits).
	// Lower bound 1e12 ms = 2001-09; any sane now+15min comfortably exceeds it.
	if sess.ExpiresAt < 1_000_000_000_000 {
		t.Fatalf("expiresAt = %d, too small to be Unix milliseconds (looks like seconds?)", sess.ExpiresAt)
	}

	if len(s.persistRows) != 1 {
		t.Fatalf("persisted %d sessions, want 1", len(s.persistRows))
	}
	if !s.persistRows[0].access.UserID.Valid || s.persistRows[0].access.UserID.Bytes != user.ID.Bytes {
		t.Fatal("persisted access token has wrong user id")
	}
}

// keysOf returns the keys of a map for failure messages.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLogin_FailuresIdentical (AC2) asserts the four credential/structural failure
// classes return a consistent, non-leaking response. The two CREDENTIAL failures
// (wrong password, no such user) must be byte-for-byte identical (status + body).
// The two STRUCTURAL failures (malformed JSON, missing field) are 400 and must
// also be byte-for-byte identical to each other. No failure body may reveal which
// username/branch occurred.
func TestLogin_FailuresIdentical(t *testing.T) {
	const username, password = "Bob", "s3cr3t-passphrase-here"
	user := testUser(t, username, password)
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): user}}
	h := newTestHandler(s)

	// Credential failures: must be identical to each other (401).
	wrongPw := doLogin(h, `{"username":"Bob","password":"WRONG"}`)
	noUser := doLogin(h, `{"username":"nobody","password":"whatever"}`)

	if wrongPw.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", wrongPw.Code)
	}
	if noUser.Code != http.StatusUnauthorized {
		t.Fatalf("no-such-user status = %d, want 401", noUser.Code)
	}
	if wrongPw.Body.String() != noUser.Body.String() {
		t.Fatalf("credential-failure bodies differ (enumeration leak):\n  wrong-pw: %s\n  no-user:  %s",
			wrongPw.Body.String(), noUser.Body.String())
	}

	// Structural failures: must be identical to each other (400).
	malformed := doLogin(h, `{"username":"Bob","passwo`) // truncated JSON
	missing := doLogin(h, `{"username":"Bob"}`)          // password absent

	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed-json status = %d, want 400", malformed.Code)
	}
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing-field status = %d, want 400", missing.Code)
	}
	if malformed.Body.String() != missing.Body.String() {
		t.Fatalf("structural-failure bodies differ:\n  malformed: %s\n  missing:   %s",
			malformed.Body.String(), missing.Body.String())
	}

	// No failure body may contain a username or the word "password" value hints.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"wrong-pw": wrongPw, "no-user": noUser, "malformed": malformed, "missing": missing,
	} {
		body := rec.Body.String()
		if strings.Contains(body, "Bob") || strings.Contains(body, "nobody") || strings.Contains(body, "WRONG") {
			t.Fatalf("%s failure body leaks input: %s", name, body)
		}
	}
}

// TestLogin_StrictParsing (AC13/NFR8) asserts the strict-parse guards each yield a
// 400 and never panic: an over-limit body, an unknown field, and a type-mismatched
// field. (Malformed JSON and missing fields are covered above.)
func TestLogin_StrictParsing(t *testing.T) {
	h := newTestHandler(&fakeStore{users: map[string]dbgen.GetUserByUsernameRow{}})

	cases := map[string]string{
		"unknown field":   `{"username":"x","password":"y","extra":true}`,
		"wrong type":      `{"username":123,"password":"y"}`,
		"trailing object": `{"username":"x","password":"y"}{"a":1}`,
		"empty body":      ``,
		"not json":        `not json at all`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doLogin(h, body) // must not panic
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body=%s", name, rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("over limit", func(t *testing.T) {
		// A body larger than maxLoginBodyBytes must be a 400, not a panic or OOM.
		big := `{"username":"x","password":"` + strings.Repeat("a", maxLoginBodyBytes+1) + `"}`
		rec := doLogin(h, big)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("over-limit body: status = %d, want 400", rec.Code)
		}
	})
}

// TestLogin_ServerErrors asserts genuine server failures map to a generic 500 and
// never leak the cause: a non-ErrNoRows DB lookup error, and a persistSession
// failure on an otherwise-valid login.
func TestLogin_ServerErrors(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		s := &fakeStore{lookupErr: errStub("db down")}
		h := newTestHandler(s)
		rec := doLogin(h, `{"username":"x","password":"y"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "db down") {
			t.Fatalf("500 body leaks internal error: %s", rec.Body.String())
		}
	})

	t.Run("persist error", func(t *testing.T) {
		const username, password = "Carol", "another-good-password"
		user := testUser(t, username, password)
		s := &fakeStore{
			users:      map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): user},
			persistErr: errStub("tx failed"),
		}
		h := newTestHandler(s)
		rec := doLogin(h, `{"username":"Carol","password":"another-good-password"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// errStub is a trivial error for fake-store failure injection.
type errStub string

func (e errStub) Error() string { return string(e) }

// TestLogin_UsernameCaseInsensitive asserts the username is lowercased before
// lookup, so a differently-cased login still finds the user (matching the
// lower(username) unique index).
func TestLogin_UsernameCaseInsensitive(t *testing.T) {
	const stored, password = "alice", "case-insensitive-pw-123"
	user := testUser(t, "Alice", password)
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{stored: user}}
	h := newTestHandler(s)

	rec := doLogin(h, `{"username":"ALICE","password":"case-insensitive-pw-123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("uppercase login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUUIDToString asserts the wire userId rendering matches the canonical
// 8-4-4-4-12 hyphenated lowercase-hex form.
func TestUUIDToString(t *testing.T) {
	var u pgtype.UUID
	copy(u.Bytes[:], []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})
	u.Valid = true
	got := uuidToString(u)
	const want = "01234567-89ab-cdef-0123-456789abcdef"
	if got != want {
		t.Fatalf("uuidToString = %q, want %q", got, want)
	}
}
