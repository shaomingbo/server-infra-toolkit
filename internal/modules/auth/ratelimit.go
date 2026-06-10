package auth

// ratelimit.go defines the RateLimiter facade (FR12 / D9): a minimal, replaceable
// seam for a FUTURE rate limiter, plus a no-op default that always allows. It is
// NOT a security control today and does NOT implement any limiting — the ONLY
// brute-force defense in T2 is the DB-side account lockout (D5/D2). This seam
// exists so that, when a real limiter is warranted (an in-process token-bucket, or
// a gateway/Redis limiter behind the same interface), it can be slotted in here
// without reshaping the login flow.
//
// NAMING IS DELIBERATELY NEUTRAL (E10): "RateLimiter" / "Allow" describe a
// capability, not a guarantee. The default noopRateLimiter always returns
// allowed=true, so wiring it changes nothing about request handling. Do not read
// the presence of this seam as "rate limiting is on" — it is not.
//
// NO THIRD-PARTY DEPENDENCY: this file imports only the standard library. The
// facade exists precisely so a limiting LIBRARY (e.g. golang.org/x/time/rate) is
// NOT pulled in until there is a real need; the repo-wide guard test asserts no
// such import exists.

import (
	"context"
	"time"
)

// RateLimiter is the replaceable rate-limiting seam. The signature is shaped to
// fit a future real implementation without a breaking change:
//   - key lets a real limiter bucket by whatever identity is appropriate (a
//     username, an IP, a global "login" key) without this interface committing to
//     one granularity now.
//   - retryAfter lets a future limiter report how long to back off; a caller could
//     surface it (e.g. via a 429 the no-op never produces). The no-op returns 0.
//
// This interface is NOT YET backed by a real limiter, so its exact shape is not a
// frozen contract: when a real limiter is implemented it MAY be reshaped to fit
// (nothing external depends on it yet). Until then it is the documented extension
// point (R6).
type RateLimiter interface {
	// Allow reports whether a request keyed by key may proceed. When allowed is
	// false, retryAfter is a hint for how long to wait before retrying; when
	// allowed is true, retryAfter is 0.
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration)
}

// noopRateLimiter is the default: it imposes NO limit and always allows. It exists
// so the login flow can have a single, unconditional call site for the seam
// without a nil check, while the actual behavior remains "no limiting" until a
// real limiter replaces it.
type noopRateLimiter struct{}

// Allow always permits the request (allowed=true, retryAfter=0). This is the whole
// point of the no-op: the seam is present and called, but it never blocks anything.
func (noopRateLimiter) Allow(_ context.Context, _ string) (bool, time.Duration) {
	return true, 0
}

// Compile-time assertion that the default satisfies the seam.
var _ RateLimiter = noopRateLimiter{}
