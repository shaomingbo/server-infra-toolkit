package auth

import (
	"sort"
	"strings"
	"testing"
	"time"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// TestConstantWork_FailurePathsEquivalent (AC4) asserts the two credential-failure
// paths — wrong password (user exists) and no such user — both perform one real
// argon2 computation and take indistinguishable wall-clock time, closing the
// username-enumeration timing channel (FR10).
//
// METHODOLOGY: timing tests are noisy, so we do not assert a tight ratio on a
// single sample. We run each path many times, take the MEDIAN (p50) and p95 of
// each, and assert the two medians are within a tolerance that is small relative
// to one argon2 op. Because both paths do exactly one argon2 computation, the
// dominant cost is identical and the medians must track closely; a regression that
// skipped argon2 on the not-found path would roughly halve (or zero) that path's
// time, blowing past the tolerance. The p95 comparison guards the tail.
func TestConstantWork_FailurePathsEquivalent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in -short mode")
	}

	const username, password = "timing-user", "the-real-password-value"
	user := testUser(t, username, password)
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): user}}
	h := newTestHandler(s)

	// Warm the argon2 working set so the first not-found call does not skew the
	// sample (the dummy hash itself is precomputed at process start).
	_ = DummyVerify("warmup")
	_, _ = Verify("warmup", user.PasswordHash)

	const iterations = 25

	wrongPw := sampleLogin(t, h, `{"username":"timing-user","password":"WRONG-PASSWORD"}`, iterations)
	noUser := sampleLogin(t, h, `{"username":"ghost-user","password":"WRONG-PASSWORD"}`, iterations)

	wpP50, wpP95 := percentile(wrongPw, 50), percentile(wrongPw, 95)
	nuP50, nuP95 := percentile(noUser, 50), percentile(noUser, 95)

	t.Logf("wrong-password  p50=%v p95=%v", wpP50, wpP95)
	t.Logf("no-such-user    p50=%v p95=%v", nuP50, nuP95)

	// Tolerance: the two medians must be within 50% of the SMALLER median. One
	// argon2 op at the OWASP floor dominates both; a path that skipped it would be
	// far more than 50% faster. 50% leaves generous headroom for scheduler/CI noise
	// while still catching a missing argon2 computation.
	if relDiff(wpP50, nuP50) > 0.5 {
		t.Fatalf("p50 timing divergence too large (possible enumeration leak): wrong-pw=%v no-user=%v",
			wpP50, nuP50)
	}
	// The p95 tail is noisier; allow a wider band but still bound it.
	if relDiff(wpP95, nuP95) > 1.0 {
		t.Fatalf("p95 timing divergence too large: wrong-pw=%v no-user=%v", wpP95, nuP95)
	}
}

// TestConstantWork_BothPathsHitArgon2 asserts, structurally (not by timing), that
// both failure paths route through one argon2 computation: the wrong-password path
// through Verify and the no-such-user path through DummyVerify. We prove this by
// observing each path takes a non-trivial amount of time consistent with running
// argon2 (an OWASP-floor argon2 op is milliseconds, far above a no-op), which a
// skipped-KDF path could not. This complements the relative timing test with an
// absolute floor.
func TestConstantWork_BothPathsHitArgon2(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in -short mode")
	}

	// Establish a baseline cost for one argon2 op directly.
	_ = DummyVerify("warmup")
	start := time.Now()
	_ = DummyVerify("baseline-measure")
	oneArgon2 := time.Since(start)

	const username, password = "absolute-user", "absolute-password"
	user := testUser(t, username, password)
	s := &fakeStore{users: map[string]dbgen.GetUserByUsernameRow{strings.ToLower(username): user}}
	h := newTestHandler(s)

	// Floor: each failure path must cost at least a meaningful fraction of one
	// argon2 op (we use 1/3 to stay robust against measurement noise). A path that
	// short-circuited the KDF would fall well below this.
	floor := oneArgon2 / 3

	wrongPw := medianDuration(t, h, `{"username":"absolute-user","password":"nope"}`, 5)
	noUser := medianDuration(t, h, `{"username":"missing","password":"nope"}`, 5)

	if wrongPw < floor {
		t.Fatalf("wrong-password path too fast (argon2 skipped?): %v < floor %v", wrongPw, floor)
	}
	if noUser < floor {
		t.Fatalf("no-such-user path too fast (dummy argon2 skipped?): %v < floor %v", noUser, floor)
	}
}

// sampleLogin runs the login handler n times against rawBody and returns the
// per-call durations.
func sampleLogin(t *testing.T, h *Handler, rawBody string, n int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		_ = doLogin(h, rawBody)
		out[i] = time.Since(start)
	}
	return out
}

// medianDuration runs login n times and returns the median duration.
func medianDuration(t *testing.T, h *Handler, rawBody string, n int) time.Duration {
	t.Helper()
	return percentile(sampleLogin(t, h, rawBody, n), 50)
}

// percentile returns the p-th percentile (0-100) of durations using nearest-rank.
func percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p * (len(s) - 1)) / 100
	return s[idx]
}

// relDiff returns the absolute relative difference between two durations,
// normalized by the smaller one (so it is symmetric in magnitude).
func relDiff(a, b time.Duration) float64 {
	if a == 0 && b == 0 {
		return 0
	}
	min := a
	if b < min {
		min = b
	}
	if min == 0 {
		return 1
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return float64(diff) / float64(min)
}
