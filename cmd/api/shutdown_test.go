package main

import (
	"io"
	"net"
	stdhttp "net/http"
	"sync"
	"testing"
	"time"
)

// shutdown_test.go pins the FR2/D5 graceful-shutdown contract: (1) the drain
// sequence lets an in-flight slow request finish with a complete 200 while
// rejecting new connections, and gracefulShutdown returns within its declared
// budget (AC2); (2) the HTTP-drain budget plus the pool-close budget stay within
// the Cloud Run termination grace period, with both sides asserted as literals so
// a constant change is loud (AC3). Neither test touches the database or a real
// signal — gracefulShutdown takes narrow seams so a fake pool is injected.

// fakePool stands in for *db.Pool at the closePoolWithGrace seam. Close blocks for
// blockFor before completing, letting a test exercise both the fast-return and the
// grace-overrun paths without a live connection pool. It also records the wall-clock
// instant Close was entered (closedAt) so a test can assert the pool is closed only
// AFTER the HTTP drain finished — pinning the drain-THEN-close ordering contract,
// not just that Close ran at all.
//
// When release is non-nil, Close blocks on it instead of sleeping for blockFor; this
// lets TestClosePoolWithGrace_AbandonsAfterGrace unblock the goroutine on cleanup
// rather than leaking it for the lifetime of the process. doneCh (when non-nil) is
// closed once Close returns, so the test can wait for the inner goroutine to exit.
type fakePool struct {
	blockFor time.Duration
	release  <-chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	closed   bool
	closedAt time.Time
}

func (p *fakePool) Close() {
	p.mu.Lock()
	p.closedAt = time.Now()
	p.mu.Unlock()
	if p.release != nil {
		<-p.release
	} else {
		time.Sleep(p.blockFor)
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	if p.doneCh != nil {
		close(p.doneCh)
	}
}

func (p *fakePool) wasClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// closedAtTime returns the instant Close was entered, or the zero time if Close has
// not been called yet.
func (p *fakePool) closedAtTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closedAt
}

// TestGracefulShutdown_DrainsInFlightAndRejectsNew drives gracefulShutdown against
// a real http.Server on a loopback listener (AC2). A request whose handler sleeps
// >=3s is in flight when shutdown is triggered; it must still receive a complete
// 200, gracefulShutdown must return within the declared budget, and a connection
// attempted after shutdown began must be refused.
func TestGracefulShutdown_DrainsInFlightAndRejectsNew(t *testing.T) {
	const handlerDelay = 3 * time.Second

	// started closes once the slow handler is running, so the test triggers
	// shutdown only after the request is genuinely in flight. handlerDoneAt records
	// the instant the handler finished writing its response, so the ordering
	// assertion can prove the pool was closed strictly after the drain completed.
	started := make(chan struct{})
	var handlerMu sync.Mutex
	var handlerDoneAt time.Time
	mux := stdhttp.NewServeMux()
	mux.HandleFunc("/slow", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		close(started)
		time.Sleep(handlerDelay)
		w.WriteHeader(stdhttp.StatusOK)
		_, _ = io.WriteString(w, "drained")
		handlerMu.Lock()
		handlerDoneAt = time.Now()
		handlerMu.Unlock()
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srv := &stdhttp.Server{Handler: mux}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// Fire the slow request and capture its outcome from a goroutine.
	type result struct {
		status int
		body   string
		err    error
	}
	reqDone := make(chan result, 1)
	go func() {
		resp, err := stdhttp.Get("http://" + addr + "/slow")
		if err != nil {
			reqDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		reqDone <- result{status: resp.StatusCode, body: string(b)}
	}()

	// Wait until the handler is actually running before shutting down.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow handler never started")
	}

	pool := &fakePool{}

	shutdownReturned := make(chan error, 1)
	start := time.Now()
	go func() { shutdownReturned <- gracefulShutdown(srv, pool) }()

	// New connections must be refused once Shutdown has begun. Shutdown closes the
	// listener synchronously before draining, but Serve's accept loop may take a
	// beat to unwind; retry briefly so this asserts "rejected", not a race.
	rejected := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr != nil {
			rejected = true
			break
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if !rejected {
		t.Error("a connection after shutdown began was accepted, want refused")
	}

	// The in-flight request must complete with a full 200 body.
	select {
	case res := <-reqDone:
		if res.err != nil {
			t.Fatalf("in-flight request errored instead of draining: %v", res.err)
		}
		if res.status != stdhttp.StatusOK {
			t.Fatalf("in-flight request status = %d, want 200", res.status)
		}
		if res.body != "drained" {
			t.Fatalf("in-flight request body = %q, want %q (response was truncated)", res.body, "drained")
		}
	case <-time.After(handlerDelay + 5*time.Second):
		t.Fatal("in-flight request did not complete")
	}

	// gracefulShutdown must return, and within its declared budget. The slow
	// handler takes <handlerDelay (<httpDrainBudget) so drain succeeds well inside
	// the budget; allow a generous scheduling margin so this is not flaky.
	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Fatalf("gracefulShutdown returned error: %v", err)
		}
	case <-time.After(httpDrainBudget + poolCloseBudget + 2*time.Second):
		t.Fatal("gracefulShutdown did not return within budget")
	}
	if elapsed := time.Since(start); elapsed > httpDrainBudget+poolCloseBudget {
		t.Errorf("gracefulShutdown took %s, want <= %s (httpDrainBudget+poolCloseBudget)", elapsed, httpDrainBudget+poolCloseBudget)
	}
	if !pool.wasClosed() {
		t.Error("pool was not closed during graceful shutdown")
	}

	// Ordering contract (the heart of the drain-THEN-close sequence): the pool must
	// be closed only AFTER the in-flight request's response has been fully written.
	// gracefulShutdown calls srv.Shutdown (which returns only once the slow handler
	// completes) BEFORE closePoolWithGrace, so pool.closedAt must be later than the
	// instant the handler finished. Moving closePoolWithGrace ahead of srv.Shutdown
	// would close the pool while the request is still in flight, which this catches.
	handlerMu.Lock()
	doneAt := handlerDoneAt
	handlerMu.Unlock()
	if doneAt.IsZero() {
		t.Fatal("slow handler never recorded a completion time")
	}
	closedAt := pool.closedAtTime()
	if closedAt.IsZero() {
		t.Fatal("pool.Close was never entered")
	}
	if !closedAt.After(doneAt) {
		t.Errorf("pool was closed at %s, before the in-flight response finished at %s; drain-then-close ordering violated (closePoolWithGrace must run after srv.Shutdown)",
			closedAt.Format(time.StampNano), doneAt.Format(time.StampNano))
	}

	// Serve must have returned ErrServerClosed after a clean Shutdown.
	select {
	case err := <-serveErr:
		if err != stdhttp.ErrServerClosed {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Serve did not return after Shutdown")
	}

	// After gracefulShutdown returned, a fresh real HTTP request must NOT be served:
	// the listener is closed, so the request must fail to connect (or, if anything is
	// still listening, return a non-2xx). The earlier mid-shutdown Dial loop only
	// proves the accept loop unwound; this asserts the post-shutdown surface at the
	// HTTP layer with a short timeout so it cannot hang on a half-open socket.
	client := &stdhttp.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/slow")
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Errorf("a request after gracefulShutdown returned %d, want a connection failure or non-2xx", resp.StatusCode)
		}
	}
}

// TestClosePoolWithGrace_AbandonsAfterGrace asserts the pool-close guard returns
// after at most grace even when Close blocks longer, so a stuck connection cannot
// overrun the termination window (the guard behind poolCloseBudget).
func TestClosePoolWithGrace_AbandonsAfterGrace(t *testing.T) {
	// release blocks the fake pool's Close until the test cleans up, instead of a
	// 1h sleep that would leak the inner Close goroutine until the process exits.
	// doneCh closes when Close finally returns, so cleanup can wait for the goroutine
	// closePoolWithGrace spawned to actually exit.
	release := make(chan struct{})
	pool := &fakePool{release: release, doneCh: make(chan struct{})}
	const grace = 100 * time.Millisecond

	// On cleanup, unblock Close and wait for its goroutine to finish, so the test
	// leaves no goroutine running.
	t.Cleanup(func() {
		close(release)
		<-pool.doneCh
	})

	done := make(chan struct{})
	start := time.Now()
	go func() {
		closePoolWithGrace(pool, grace)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > grace+time.Second {
			t.Errorf("closePoolWithGrace returned after %s, want ~%s (abandoned the wait)", elapsed, grace)
		}
	case <-time.After(grace + 2*time.Second):
		t.Fatal("closePoolWithGrace did not abandon the wait after grace; it would overrun the SIGTERM window")
	}
}

// TestShutdownBudgetFitsGracePeriod pins the FR2/AC3 budget reconciliation: the
// HTTP-drain budget plus the pool-close budget must stay within the Cloud Run
// SIGTERM->SIGKILL grace period. Both sides are asserted as literals so changing
// any of the three constants makes this test red on purpose — when you do, also
// update the grace-period declaration in docs/DEPLOY.md (see the main.go const
// header) so the code and the runbook never drift apart.
func TestShutdownBudgetFitsGracePeriod(t *testing.T) {
	// Pin each constant to its literal value (a drift in any one is caught).
	if httpDrainBudget != 5*time.Second {
		t.Errorf("httpDrainBudget = %s, want 5s — see the main.go shutdown-budget const header before changing", httpDrainBudget)
	}
	if poolCloseBudget != 2*time.Second {
		t.Errorf("poolCloseBudget = %s, want 2s — see the main.go shutdown-budget const header before changing", poolCloseBudget)
	}
	if cloudRunGracePeriod != 10*time.Second {
		t.Errorf("cloudRunGracePeriod = %s, want 10s (Cloud Run fixed SIGTERM->SIGKILL window) — verified against docs.cloud.google.com/run/docs/container-contract", cloudRunGracePeriod)
	}

	// The contract: the two back-to-back drain budgets fit inside the grace period.
	if httpDrainBudget+poolCloseBudget > cloudRunGracePeriod {
		t.Errorf("httpDrainBudget(%s) + poolCloseBudget(%s) = %s > cloudRunGracePeriod(%s); the drain sequence can be SIGKILLed mid-teardown",
			httpDrainBudget, poolCloseBudget, httpDrainBudget+poolCloseBudget, cloudRunGracePeriod)
	}
}
