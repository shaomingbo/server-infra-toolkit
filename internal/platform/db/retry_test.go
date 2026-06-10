package db

// retry_test.go covers the FR10 retry CORE (AC15): the testable retry() function
// exercised against mocked transient failures — no real database is touched
// (E5/NFR8: the local environment has no scale-to-zero, so only the retry logic
// itself is unit-testable here; the real "Neon wakes after scale-to-zero, first
// request succeeds" path is a manual/deferred line item, see AC15/AC16 notes in
// retry.go and the task report).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// safeToRetryErr is a mock connection-level failure that pgx classifies as
// safe-to-retry (the failure is guaranteed to have happened before any data was
// sent). It implements the same `interface{ SafeToRetry() bool }` shape that
// pgconn.SafeToRetry matches via errors.As, so isTransientConnError treats it
// exactly as it would a real pre-send connection failure. We mock this rather
// than *pgconn.ConnectError because ConnectError's wrapped error field is
// unexported (a test cannot construct a non-nil one), and its Error() panics
// when that field is nil.
type safeToRetryErr struct{}

func (safeToRetryErr) Error() string     { return "mock transient connection failure" }
func (safeToRetryErr) SafeToRetry() bool { return true }

// transientErr returns a fresh mock transient (safe-to-retry) connection error,
// the cold-start case isTransientConnError must retry.
func transientErr() error { return safeToRetryErr{} }

// TestRetry_TransientThenSucceeds is the headline AC15 path: the first acquire
// fails transiently, then a retry succeeds. It asserts (a) the call ultimately
// succeeds, and (b) it retried (was called more than once).
func TestRetry_TransientThenSucceeds(t *testing.T) {
	calls := 0
	err := retry(context.Background(), "test", func(context.Context) error {
		calls++
		if calls < 2 {
			return transientErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry returned error, want success: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fn called %d times, want exactly 2 (1 fail + 1 retry success)", calls)
	}
}

// TestRetry_AttemptCap asserts the bounded-retry guarantee: a persistently
// transient failure is retried at most maxAttempts times and then surfaces the
// error (so the HTTP layer can WriteError a 5xx, never spin forever).
func TestRetry_AttemptCap(t *testing.T) {
	calls := 0
	err := retry(context.Background(), "test", func(context.Context) error {
		calls++
		return transientErr()
	})
	if err == nil {
		t.Fatalf("retry returned nil, want the transient error after the cap")
	}
	if calls != maxAttempts {
		t.Fatalf("fn called %d times, want exactly maxAttempts=%d", calls, maxAttempts)
	}
}

// TestRetry_BackoffExists asserts a real backoff sleep happens between attempts:
// with maxAttempts=3 there are 2 inter-attempt sleeps (baseBackoff +
// 2×baseBackoff). The elapsed wall-clock must be at least the first backoff, and
// strictly greater than zero, proving the retry is not a tight spin loop.
func TestRetry_BackoffExists(t *testing.T) {
	start := time.Now()
	_ = retry(context.Background(), "test", func(context.Context) error {
		return transientErr()
	})
	elapsed := time.Since(start)

	// Lower bound: at least the sum of the two backoffs (100ms + 200ms) must
	// have elapsed. We assert >= the first backoff to stay robust against timer
	// granularity while still proving a non-trivial delay occurred.
	if elapsed < baseBackoff {
		t.Fatalf("elapsed %v < baseBackoff %v: no backoff sleep happened", elapsed, baseBackoff)
	}
	// Sanity upper bound: must stay well under totalBudget (no runaway).
	if elapsed > totalBudget {
		t.Fatalf("elapsed %v exceeded totalBudget %v", elapsed, totalBudget)
	}
}

// TestRetry_NonTransientNotRetried asserts a server-reported error
// (*pgconn.PgError, e.g. a constraint violation) is NOT retried: re-running a
// possibly-non-idempotent statement is unsafe. It must fail on the first call.
func TestRetry_NonTransientNotRetried(t *testing.T) {
	calls := 0
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	err := retry(context.Background(), "test", func(context.Context) error {
		calls++
		return pgErr
	})
	if !errors.Is(err, pgErr) {
		t.Fatalf("retry returned %v, want the original pg error unchanged", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want exactly 1 (server errors are never retried)", calls)
	}
}

// TestRetry_ContextCancelStops asserts an already-cancelled context aborts the
// retry promptly rather than burning the full backoff budget.
func TestRetry_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the loop sleeps

	start := time.Now()
	calls := 0
	err := retry(ctx, "test", func(context.Context) error {
		calls++
		return transientErr()
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("retry returned nil, want the transient error after ctx cancel")
	}
	// With the context cancelled, sleepWithBudget returns immediately, so the
	// loop must finish far under a single full backoff.
	if elapsed >= baseBackoff {
		t.Fatalf("elapsed %v >= baseBackoff %v: cancel did not short-circuit the backoff", elapsed, baseBackoff)
	}
	// It still made at least the first attempt before bailing.
	if calls < 1 {
		t.Fatalf("fn called %d times, want at least 1", calls)
	}
}

// TestIsTransientConnError_Classification pins the boundary between transient
// connect failures (retried) and everything else (not retried), guarding
// against misclassifying business errors as retryable. It covers the SQLSTATE
// whitelist: connect/wake-time server codes (e.g. 57P03 cannot_connect_now, the
// Neon wake case) ARE transient, while ordinary business PgErrors (constraint
// violation, syntax error) are NOT.
func TestIsTransientConnError_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"safe-to-retry (pre-send conn failure)", transientErr(), true},
		{"connect error (cold start dial)", &pgconn.ConnectError{Config: &pgconn.Config{}}, true},
		{"context deadline", context.DeadlineExceeded, false},
		{"context canceled", context.Canceled, false},
		{"plain error", errors.New("boom"), false},

		// SQLSTATE whitelist: connect/wake-time server errors are retried.
		{"pg 57P03 cannot_connect_now (Neon waking)", &pgconn.PgError{Code: "57P03"}, true},
		{"pg 57P01 admin_shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"pg 08006 connection_failure", &pgconn.PgError{Code: "08006"}, true},
		{"pg 08001 unable_to_establish", &pgconn.PgError{Code: "08001"}, true},
		{"pg 53300 too_many_connections", &pgconn.PgError{Code: "53300"}, true},

		// Ordinary business PgErrors are never retried.
		{"pg 23505 unique_violation", &pgconn.PgError{Code: "23505"}, false},
		{"pg 42601 syntax_error", &pgconn.PgError{Code: "42601"}, false},
		{"pg 23503 foreign_key_violation", &pgconn.PgError{Code: "23503"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientConnError(tc.err); got != tc.want {
				t.Fatalf("isTransientConnError(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRetry_BudgetBindsFnExecution is the BLOCKER regression: the total budget
// must constrain a SINGLE attempt's fn execution (a hung acquire / cold dial),
// not only the inter-attempt backoff. fn here blocks until the context it is
// handed is done; before the fix retry passed the caller's (never-cancelled)
// context straight through, so this would block forever. After the fix retry
// derives a budget-bounded context, so fn unblocks at the budget and retry
// returns within it. We shrink totalBudget so the test is fast.
func TestRetry_BudgetBindsFnExecution(t *testing.T) {
	orig := totalBudget
	totalBudget = 150 * time.Millisecond
	defer func() { totalBudget = orig }()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- retry(context.Background(), "test", func(ctx context.Context) error {
			// Simulate a single acquire that hangs: block until the budget-derived
			// context fires. If retry did not bound fn with the budget, this never
			// returns and the test times out.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("retry returned nil, want a deadline error after the budget")
		}
		// Must return shortly after the budget, never hang. Generous upper bound
		// (10×) to stay robust on a loaded CI box while still proving the bound.
		if elapsed > 10*totalBudget {
			t.Fatalf("retry took %v, far over budget %v: fn execution is not budget-bound", elapsed, totalBudget)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not return: fn execution is not bounded by totalBudget (BLOCKER regression)")
	}
}

// TestRetry_CallerDeadlineWins asserts the caller's tighter deadline still wins
// over the (larger) budget: WithTimeout keeps the earlier deadline. A fn that
// blocks on its context must unblock at the caller's deadline, well before the
// budget.
func TestRetry_CallerDeadlineWins(t *testing.T) {
	orig := totalBudget
	totalBudget = 5 * time.Second
	defer func() { totalBudget = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := retry(ctx, "test", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("retry returned nil, want a deadline error")
	}
	if elapsed >= totalBudget {
		t.Fatalf("elapsed %v >= budget %v: the caller's tighter deadline did not win", elapsed, totalBudget)
	}
}
