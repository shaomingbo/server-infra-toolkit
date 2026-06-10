package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	stdhttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
	"github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/db"
	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// version is injected at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local builds.
var version = "dev"

// Compile-time guard that the runtime pool type satisfies the HTTP layer's DB
// interface. The two interfaces (db.DBTX and apphttp.DB) are declared separately
// by design — the http package must not import the db package (AC11) — so this
// assertion is what catches an accidental signature drift between them at build
// time, here at the wiring seam, instead of only when NewServer(cfg, pool) is
// called.
var _ apphttp.DB = (*db.Pool)(nil)

// Compile-time guard that the runtime pool also satisfies the auth module's
// narrow DB seam (internal/modules/auth.DB). auth declares its own consumer-side
// interface and must not import the concrete db package (its boundary takes the
// interface, not *db.Pool); this assertion lives here at the wiring seam — the
// one place that may import both db and auth — so a signature drift between the
// pool and the auth seam fails the build here rather than at NewHandler(pool).
var _ auth.DB = (*db.Pool)(nil)

// Compile-time guard pinning the Bearer-middleware seam (FR14/AC17). auth.Handler
// exposes BearerMiddleware with the standard func(http.Handler) http.Handler shape
// so a future protected business route mounts it INSIDE the request-id/access-log
// chain — registrars run against the mux that NewServer wraps with that chain, and
// a protected handler wrapped as bearer(handler) nests correctly. T2 has no
// protected business routes yet, so the middleware is not mounted; /livez is never
// wrapped by it (it carries no DB call, AC8). Pinning the method-value type here
// makes a drift in the middleware signature fail the build at the wiring seam.
var _ func(stdhttp.Handler) stdhttp.Handler = (*auth.Handler)(nil).BearerMiddleware

func main() {
	smoke := flag.Bool("smoke", false, "run a one-shot Neon SELECT 1 reachability check and exit (does not start the HTTP server)")
	seed := flag.Bool("seed", false, "create one user from SEED_USERNAME / SEED_PASSWORD (argon2id hashed) and exit (does not start the HTTP server)")
	unlock := flag.Bool("unlock", false, "clear failed_attempts/locked_until for the user named in UNLOCK_USERNAME and exit (operational account-lockout recovery; does not start the HTTP server)")
	flag.Parse()

	if *smoke {
		if err := runSmoke(); err != nil {
			fmt.Fprintln(os.Stderr, "neon smoke: failed:", err)
			os.Exit(1)
		}
		fmt.Println("neon smoke: ok")
		return
	}

	if *seed {
		if err := runSeed(); err != nil {
			fmt.Fprintln(os.Stderr, "seed: failed:", err)
			os.Exit(1)
		}
		fmt.Println("seed: ok")
		return
	}

	if *unlock {
		if err := runUnlock(); err != nil {
			fmt.Fprintln(os.Stderr, "unlock: failed:", err)
			os.Exit(1)
		}
		fmt.Println("unlock: ok")
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// runSmoke loads config and runs a single Neon reachability check (SELECT 1 via
// a bare connection that is immediately closed). It never starts the HTTP server
// and never builds a connection pool; this is the only DB interaction in T0.
func runSmoke() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return db.Smoke(ctx, cfg.DSN.Reveal())
}

// runSeed creates a single application user from the SEED_USERNAME and
// SEED_PASSWORD environment variables, with the password argon2id-hashed via the
// auth module, then exits. It is a one-shot operational command (run once after a
// migration to bootstrap the first user), not part of the HTTP serving path.
//
// SECURITY (AC19): the seed credentials are read ONLY from the environment at run
// time. The password is never hardcoded, never given a default, and never written
// into a migration or any committed file — it exists only in the operator's
// environment for the duration of this command and reaches the database solely as
// an argon2id hash. The plaintext is never logged.
func runSeed() error {
	username := os.Getenv("SEED_USERNAME")
	password := os.Getenv("SEED_PASSWORD")
	// Refuse to run on missing/empty credentials rather than seeding a guessable
	// default — a defaulted seed password is exactly the foothold this guards.
	if username == "" || password == "" {
		return errors.New("SEED_USERNAME 和 SEED_PASSWORD 必须都设置(不接受空值或默认值)")
	}

	hash, err := auth.Hash(password)
	if err != nil {
		return fmt.Errorf("hash seed password: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.NewRetryPool(ctx, cfg.DSN.Reveal())
	if err != nil {
		return err
	}
	defer pool.Close()

	q := dbgen.New(pool)
	user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
		Username:     username,
		PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("create seed user: %w", err)
	}

	// Print only the non-sensitive username, never the password or the hash.
	fmt.Printf("seed: created user %q\n", user.Username)
	return nil
}

// runUnlock clears a user's account-lockout state (failed_attempts -> 0,
// locked_until -> NULL) for the username in UNLOCK_USERNAME, then exits. It is the
// operational recovery path (W2d FR13/AC14) for a victim locked out by an
// account-lockout DoS before the lock window expires (PRD D1/R1): an operator runs
// `go run ./cmd/api -unlock` with UNLOCK_USERNAME set, and the account can log in
// again on its next correct password.
//
// It is a one-shot command, never on the serving path. It clears the lock by
// reusing SetUserLock (the idempotent whole-value reset the success path uses), so
// it adds no new query and no migration. No credential is read or printed.
func runUnlock() error {
	username := os.Getenv("UNLOCK_USERNAME")
	if username == "" {
		return errors.New("UNLOCK_USERNAME 必须设置(要解锁的用户名,大小写不敏感)")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.NewRetryPool(ctx, cfg.DSN.Reveal())
	if err != nil {
		return err
	}
	defer pool.Close()

	q := dbgen.New(pool)
	user, err := q.GetUserByUsername(ctx, username)
	if err != nil {
		// pgx.ErrNoRows or any DB error: report a generic failure (no row dump).
		return fmt.Errorf("look up user to unlock: %w", err)
	}

	// Reset to the unlocked state: failed_attempts=0, locked_until=NULL (an invalid
	// pgtype.Timestamptz encodes SQL NULL).
	if err := q.SetUserLock(ctx, dbgen.SetUserLockParams{
		ID:             user.ID,
		FailedAttempts: 0,
		LockedUntil:    pgtype.Timestamptz{Valid: false},
	}); err != nil {
		return fmt.Errorf("clear lockout state: %w", err)
	}

	fmt.Printf("unlock: cleared lockout for user %q\n", user.Username)
	return nil
}

func run() error {
	// Resolve the effective version: prefer the ldflags-injected value, falling
	// back to the Cloud Run revision when running an unstamped ("dev") build.
	effectiveVersion := version
	if effectiveVersion == "dev" {
		if rev := os.Getenv("K_REVISION"); rev != "" {
			effectiveVersion = rev
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Version = effectiveVersion

	// Build the runtime connection pool lazily and wrap it with the FR10
	// double-scale-to-zero reconnect retry: NewRetryPool does not dial Neon, so
	// startup never blocks on the database waking up (AC10/AC11). The wrapped
	// pool is passed into NewServer by construction (no global state) and closed
	// only after the HTTP server has drained in-flight requests below (FR9/E6).
	// The retry is transparent to the HTTP layer — *db.Pool satisfies the same
	// narrow DB interface as the bare *pgxpool.Pool.
	pool, err := db.NewRetryPool(context.Background(), cfg.DSN.Reveal())
	if err != nil {
		return err
	}

	// Construct module handlers here, at the top level, and mount them via the
	// NewServer route-registrar seam. This is the ONLY place auth is imported:
	// internal/http never imports internal/modules/auth (frozen dependency
	// direction). The auth handler takes the pool through its narrow DB seam.
	authHandler := auth.NewHandler(pool)

	srv := &stdhttp.Server{
		Addr:    ":" + cfg.Port,
		Handler: apphttp.NewServer(cfg, pool, authHandler.RegisterRoutes),
		// Conservative timeouts: basic hardening for a publicly deployed server
		// (slow-loris / idle-connection protection).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Trigger a graceful shutdown on SIGINT/SIGTERM (NFR1: short grace, no
	// connection draining beyond what http.Server.Shutdown provides).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		fmt.Printf("server-infra-toolkit %s listening on %s\n", effectiveVersion, srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, stdhttp.ErrServerClosed) {
			return nil
		}
		// ListenAndServe failed for some other reason (e.g. the port is taken):
		// close the pool before returning so this error path does not leak it.
		closePoolWithGrace(pool, 5*time.Second)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Drain in-flight HTTP requests first, THEN close the pool, so requests
		// still using pooled connections are not cut off mid-flight (FR9/E6).
		err := srv.Shutdown(shutdownCtx)
		// Close the pool with a grace guard so a connection that refuses to return
		// cannot block past the Cloud Run SIGTERM grace and get the process hard
		// killed. T1 does the minimal pool-close hookup only; full connection
		// draining/orchestration is deferred to T6 (NG9).
		closePoolWithGrace(pool, 2*time.Second)
		return err
	}
}

// closePoolWithGrace runs pool.Close() but waits at most grace for it to finish.
// pool.Close() blocks until every connection is returned and torn down; if a
// connection is stuck, waiting forever could overrun the Cloud Run SIGTERM grace
// period and get the process SIGKILLed mid-teardown. After grace we stop waiting
// and let the process exit — the OS reclaims the sockets. This is the minimal T1
// guardrail; full graceful connection draining is a T6 item (NG9).
func closePoolWithGrace(pool *db.Pool, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// Grace exhausted: abandon the wait and let the process exit.
	}
}
