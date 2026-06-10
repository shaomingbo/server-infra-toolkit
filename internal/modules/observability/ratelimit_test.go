package observability

// ratelimit_test.go pins the RateLimiter facade invariants (FR7): the no-op
// default always allows, NewHandler wires it by default, the ingest flow has a
// real call site (a recording limiter observes one), a denial maps to 429
// rate_limited with the limiter's Retry-After hint, and — the guard that matters
// most — no rate-limiting LIBRARY is imported anywhere in the module's build
// closure (the seam exists precisely to defer that). Mirrors auth's
// ratelimit_test.go.

import (
	"context"
	"net/http"
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
// limiter so the ingest call site never nil-panics, and that the default allows.
func TestNewHandler_DefaultsToNoopLimiter(t *testing.T) {
	h := NewHandler(nil)
	if h.rateLimiter == nil {
		t.Fatal("NewHandler left rateLimiter nil; ingest's call site would nil-panic")
	}
	if allowed, _ := h.rateLimiter.Allow(context.Background(), "k"); !allowed {
		t.Fatal("default rate limiter denied; expected the no-op to allow")
	}
}

// recordingLimiter is a test-only limiter that records the keys it is asked about
// and can be configured to deny, so we can prove (a) the ingest flow actually calls
// the seam and (b) a denial maps to a 429 with Retry-After.
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

// TestIngest_CallsRateLimiterSeam asserts the ingest flow invokes the rate limiter
// (the seam is real, not dead code) and that a permitted request still reaches the
// store.
func TestIngest_CallsRateLimiterSeam(t *testing.T) {
	fs := &fakeStore{}
	lim := &recordingLimiter{}
	h := &Handler{store: fs, rateLimiter: lim}

	rec := postBatch(t, h, []any{validEvent()})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody=%s", rec.Code, rec.Body.String())
	}
	if len(lim.keys) != 1 {
		t.Fatalf("rate limiter consulted %d times, want 1", len(lim.keys))
	}
	if fs.calls != 1 {
		t.Fatalf("store insertBatch called %d times, want 1 (a permitted request must reach the store)", fs.calls)
	}
}

// TestIngest_RateLimiterDenialIs429 asserts that when a (future) limiter denies,
// the response is 429 rate_limited with the Retry-After hint and ZERO store writes
// — the request is shed before persistence.
func TestIngest_RateLimiterDenialIs429(t *testing.T) {
	fs := &fakeStore{}
	lim := &recordingLimiter{deny: true, after: 30 * time.Second}
	h := &Handler{store: fs, rateLimiter: lim}

	rec := postBatch(t, h, []any{validEvent()})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codeRateLimited)
	if ra := rec.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want %q", ra, "30")
	}
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on a rate-limited request, want 0", fs.calls)
	}
}

// TestNoRateLimitLibraryImported asserts the whole build closure of the
// observability package does NOT depend on a rate-limiting library
// (golang.org/x/time/rate). The facade exists to AVOID pulling such a dependency
// until it is truly needed; a future change that imports one fails here.
func TestNoRateLimitLibraryImported(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/shaomingbo/server-infra-toolkit/internal/modules/observability").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "golang.org/x/time/rate" || strings.HasPrefix(dep, "golang.org/x/time/rate") {
			t.Fatalf("observability depends on a rate-limiting library (%s); FR7/D7 keeps the seam library-free until needed", dep)
		}
	}
}
