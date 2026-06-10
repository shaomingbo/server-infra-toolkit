package observability

// ratelimit.go defines the observability module's RateLimiter facade (FR7 / D7):
// a minimal, replaceable seam for a FUTURE rate limiter, plus a no-op default that
// always allows. It mirrors auth's RateLimiter shape (D7 keeps the shape aligned
// across modules but does NOT promote it to platform — auth lockout vs. T5 event
// storms have different policies, so the rule-of-three threshold is not yet met).
//
// HONEST SCOPE (FR7/E5/D4) — this is NOT a security or billing control:
//   - In-process rate limiting is NOT a safety boundary. Under scale-to-zero the
//     worst case is up to 2x the intended rate (two instances each enforcing
//     independently) and the counter RESETS on cold start, so a burst after an
//     idle period sees a fresh budget. Do not read the presence of this seam as
//     "abuse is bounded" — it is not.
//   - The actual billing ceiling is the DB/request hard limits (FR5): the 1 MiB
//     MaxBytesReader, the per-batch count cap, and the per-field schema lengths.
//     Those are independent of instance count; this limiter only smooths peaks.
//
// The real limiting policy is deferred until the endpoint is actually exposed to
// the public (D2: seam-first, no public exposure yet). Until then noopRateLimiter
// is wired and nothing is throttled.
//
// NO THIRD-PARTY DEPENDENCY: this file imports only the standard library, so a
// limiting LIBRARY (e.g. golang.org/x/time/rate) is NOT pulled into the module's
// build closure until there is a real need; ratelimit_test.go asserts that.

import (
	"context"
	"time"
)

// RateLimiter is the replaceable rate-limiting seam. The signature mirrors auth's
// (D7) so a future real limiter slots in without reshaping the ingest flow:
//   - key lets a real limiter bucket by whatever identity is appropriate (a
//     source, an IP, a global "ingest" key) without committing to one granularity
//     now.
//   - retryAfter lets a future limiter report how long to back off; the ingest
//     handler surfaces it as a 429 Retry-After when a (future) limiter denies. The
//     no-op returns 0 and never denies.
//
// This interface is NOT YET backed by a real limiter, so its exact shape is not a
// frozen contract: when a real limiter is implemented it MAY be reshaped to fit
// (nothing external depends on it yet).
type RateLimiter interface {
	// Allow reports whether a request keyed by key may proceed. When allowed is
	// false, retryAfter is a hint for how long to wait before retrying; when
	// allowed is true, retryAfter is 0.
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration)
}

// noopRateLimiter is the default: it imposes NO limit and always allows. It exists
// so the ingest flow has a single, unconditional call site for the seam without a
// nil check, while the actual behavior remains "no limiting" until a real limiter
// replaces it.
type noopRateLimiter struct{}

// Allow always permits the request (allowed=true, retryAfter=0). This is the whole
// point of the no-op: the seam is present and called, but it never blocks anything.
func (noopRateLimiter) Allow(_ context.Context, _ string) (bool, time.Duration) {
	return true, 0
}

// Compile-time assertion that the default satisfies the seam.
var _ RateLimiter = noopRateLimiter{}
