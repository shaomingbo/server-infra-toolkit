package auth

// ratelimit_test.go pins the RateLimiter facade invariants (FR12/AC13): the no-op
// default always allows, the Handler is wired with it by default, the login flow
// has a call site (a custom limiter that records calls observes one), and — the
// guard that matters most — no rate-limiting LIBRARY is imported anywhere in the
// repo (the seam exists precisely to defer that).

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestNoopRateLimiter_AlwaysAllows asserts the default imposes no limit.
func TestNoopRateLimiter_AlwaysAllows(t *testing.T) {
	allowed, retryAfter := noopRateLimiter{}.Allow(context.Background(), "any-key")
	if !allowed {
		t.Fatal("noopRateLimiter denied a request; the no-op must always allow")
	}
	if retryAfter != 0 {
		t.Fatalf("noopRateLimiter returned retryAfter=%v, want 0", retryAfter)
	}
}

// TestNewHandler_DefaultsToNoopLimiter asserts NewHandler installs a non-nil rate
// limiter so the login call site never nil-panics, and that the default allows.
func TestNewHandler_DefaultsToNoopLimiter(t *testing.T) {
	h := NewHandler(nil)
	if h.rateLimiter == nil {
		t.Fatal("NewHandler left rateLimiter nil; login's call site would nil-panic")
	}
	if allowed, _ := h.rateLimiter.Allow(context.Background(), "k"); !allowed {
		t.Fatal("default rate limiter denied; expected the no-op to allow")
	}
}

// recordingLimiter is a test-only limiter that records the keys it is asked about
// and can be configured to deny, so we can prove (a) the login flow actually calls
// the seam and (b) a denial maps to the generic 401 (D3), not a 429.
type recordingLimiter struct {
	keys  []string
	deny  bool
	after time.Duration
}

func (l *recordingLimiter) Allow(_ context.Context, key string) (bool, time.Duration) {
	l.keys = append(l.keys, key)
	if l.deny {
		return false, l.after
	}
	return true, 0
}

// TestLogin_CallsRateLimiterSeam asserts the login flow invokes the rate limiter
// with the normalized username as the key (the seam is real, not dead code).
func TestLogin_CallsRateLimiterSeam(t *testing.T) {
	const username, password = "Nora", "nora-pw-pqr"
	s := storeOf(username, testUser(t, username, password))
	lim := &recordingLimiter{}
	h := &Handler{store: s, rateLimiter: lim}

	_ = doLogin(h, `{"username":"`+username+`","password":"`+password+`"}`)
	if len(lim.keys) != 1 {
		t.Fatalf("rate limiter consulted %d times, want 1", len(lim.keys))
	}
	if lim.keys[0] != strings.ToLower(username) {
		t.Fatalf("rate-limit key = %q, want normalized username %q", lim.keys[0], strings.ToLower(username))
	}
}

// TestLogin_RateLimiterDenialIs401NotRetryAfter asserts that even when a (future)
// limiter denies, the response is the generic 401 with NO Retry-After and NO 429 —
// the same invisible-throttle invariant as the account lockout (D3).
func TestLogin_RateLimiterDenialIs401NotRetryAfter(t *testing.T) {
	const username, password = "Omar", "omar-pw-stu"
	s := storeOf(username, testUser(t, username, password))
	lim := &recordingLimiter{deny: true, after: 30 * time.Second}
	h := &Handler{store: s, rateLimiter: lim}

	rec := doLogin(h, `{"username":"Omar","password":"omar-pw-stu"}`)
	if rec.Code == 429 {
		t.Fatal("rate-limit denial returned 429; must reuse the generic 401 (D3)")
	}
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("rate-limit denial set Retry-After=%q; must stay invisible (D3)", ra)
	}
	// A denied request must not reach credential verification at all.
	if len(s.failures) != 0 || len(s.resets) != 0 || len(s.persistRows) != 0 {
		t.Fatal("a rate-limited request still touched the credential/session path")
	}
}

// TestNoRateLimitLibraryImported (AC13) asserts the whole-repo build closure of the
// auth package does NOT depend on a rate-limiting library (golang.org/x/time/rate).
// The facade exists to AVOID pulling such a dependency until it is truly needed; a
// future change that imports one fails here.
func TestNoRateLimitLibraryImported(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/shaomingbo/server-infra-toolkit/internal/modules/auth").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "golang.org/x/time/rate" || strings.HasPrefix(dep, "golang.org/x/time/rate") {
			t.Fatalf("auth depends on a rate-limiting library (%s); FR12/D9 keeps the seam library-free until needed", dep)
		}
	}
}
