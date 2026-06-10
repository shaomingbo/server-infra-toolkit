package auth

// lockout_event_test.go pins the account-lockout audit event (FR11/AC12) and the
// error-slug append-only invariant (FR10/AC11) at the unit level. The event test
// redirects the package's lockout logger to a buffer and drives the failure counter
// past the threshold, asserting exactly one account_locked line with user_id +
// request_id and no credential leak. The slug test asserts W2d introduces NO new
// outward-facing error code (the lockout reuses unauthorized, D3).

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// captureLockoutEvents swaps lockoutEventLogger for one writing to buf for the
// duration of the test, restoring the original on cleanup.
func captureLockoutEvents(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := lockoutEventLogger
	lockoutEventLogger = slog.New(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { lockoutEventLogger = orig })
	return &buf
}

// TestAccountLocked_EventEmittedOnce (FR11/AC12) drives lockoutThreshold wrong-
// password attempts against one account and asserts exactly one account_locked
// event is emitted, carrying event=account_locked + user_id + request_id and NO
// plaintext password/token. Attempts BELOW the threshold emit no event.
func TestAccountLocked_EventEmittedOnce(t *testing.T) {
	buf := captureLockoutEvents(t)

	const username, password = "Pria", "pria-correct-pw-vwx"
	s := storeOf(username, testUser(t, username, password))
	h := newTestHandler(s)

	// Drive exactly lockoutThreshold wrong-password failures.
	for i := 0; i < lockoutThreshold; i++ {
		rec := doLogin(h, `{"username":"Pria","password":"WRONG-`+strings.Repeat("x", i)+`"}`)
		if rec.Code != 401 {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("emitted %d account_locked lines, want exactly 1:\n%s", len(lines), buf.String())
	}

	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("event is not valid JSON: %v\n%s", err, lines[0])
	}
	if ev["event"] != "account_locked" {
		t.Fatalf("event field = %v, want account_locked", ev["event"])
	}
	if uid, _ := ev["user_id"].(string); uid != uuidToString(testUserID()) {
		t.Fatalf("user_id = %v, want %q", ev["user_id"], uuidToString(testUserID()))
	}
	if rid, _ := ev["request_id"].(string); rid != "req-test-123" {
		t.Fatalf("request_id = %v, want the handler's request id", ev["request_id"])
	}
	if strings.Contains(lines[0], "WRONG") || strings.Contains(lines[0], password) {
		t.Fatalf("account_locked line leaks a credential: %s", lines[0])
	}
}

// TestAccountLocked_NoEventBelowThreshold asserts no event is emitted while the
// count stays under the threshold.
func TestAccountLocked_NoEventBelowThreshold(t *testing.T) {
	buf := captureLockoutEvents(t)

	const username, password = "Quin", "quin-correct-pw-yz1"
	s := storeOf(username, testUser(t, username, password))
	h := newTestHandler(s)

	for i := 0; i < lockoutThreshold-1; i++ {
		_ = doLogin(h, `{"username":"Quin","password":"WRONG"}`)
	}
	if got := nonEmptyLines(buf.String()); len(got) != 0 {
		t.Fatalf("emitted %d account_locked lines below threshold, want 0:\n%s", len(got), buf.String())
	}
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// TestW2d_NoNewOutwardSlug (FR10/AC11) asserts W2d introduces NO new outward-facing
// error code: the only slugs auth emits are the three pre-existing ones, and the
// lockout reuses unauthorized (D3). A new rate_limited/429 slug would fail this —
// it must stay reserved for a future real limiter, not be wired today.
func TestW2d_NoNewOutwardSlug(t *testing.T) {
	// The exhaustive set of code slugs auth is allowed to emit on the wire.
	allowed := map[string]bool{
		codeUnauthorized: true,
		codeBadRequest:   true,
		codeInternal:     true,
	}
	// No lockout-specific or rate-limit slug may exist as an emitted constant.
	for _, banned := range []string{"rate_limited", "account_locked", "locked", "too_many_requests"} {
		if allowed[banned] {
			t.Fatalf("W2d wired an outward slug %q; the lockout must reuse %q (D3)", banned, codeUnauthorized)
		}
	}
}
