package auth

// store.go is the narrow persistence seam the login flow depends on. login.go
// talks to this interface, not to the generated queries or the pool directly, for
// two reasons:
//   - it expresses EXACTLY the two operations a login needs (look a user up; write
//     a token pair atomically), keeping the security logic in login.go readable;
//   - it makes the login flow unit-testable without a real database — tests
//     substitute a fake store, which is the only way to exercise the constant-work
//     and success paths deterministically (the concrete pool / pgx.Tx cannot be
//     faked, as noted in internal/platform/db/row_test.go).
//
// The production implementation (dbStore) wraps the module's narrow DB seam and
// the generated queries; it owns the db.Begin -> dbgen.New(tx).WithTx transaction
// that the refresh-rotation seam (handler.go's rotateInTx) demonstrates.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// sessionRows carries the two rows a successful login writes: one access token
// and one refresh token. Bundling them lets persistSession take a single argument
// and write both inside one transaction.
type sessionRows struct {
	access  dbgen.InsertAccessTokenParams
	refresh dbgen.InsertRefreshTokenParams
}

// store is the persistence surface the login flow consumes. It is intentionally
// tiny: a login reads the user, atomically counts a failure (or resets on
// success), and writes a token pair.
type store interface {
	// userByUsername looks a user up by an already-normalized (lowercased)
	// username. It returns the user row PLUS the DB's now() (db_now) read in the
	// same statement, so the lockout-expiry decision compares locked_until against
	// the DB clock, not the app clock (FR4/D5). It returns pgx.ErrNoRows when no
	// user matches; the caller relies on that specific sentinel to drive the
	// constant-work no-such-user path.
	userByUsername(ctx context.Context, lowerUsername string) (dbgen.GetUserByUsernameRow, error)
	// recordLoginFailure atomically increments the user's failed_attempts by one
	// and, when the new value reaches threshold, sets locked_until = DB now() +
	// window — all in ONE UPDATE...FROM self-join...RETURNING (D2/FR1 + MAJOR1/MAJOR2):
	// no read-modify-write, so concurrent failures across instances never lose a count;
	// the FROM users AS old self-join reads the PRE-update row so an already-locked
	// account is not re-counted and its lock window is not reset by a concurrent
	// over-threshold request (MAJOR1). It returns the new failed_attempts, locked_until,
	// and just_locked — true ONLY on the single request that transitions the account
	// from unlocked/expired to locked — so the caller emits account_locked exactly once
	// (FR11/MAJOR2).
	recordLoginFailure(ctx context.Context, userID pgtype.UUID, threshold int32, window time.Duration) (dbgen.RecordLoginFailureRow, error)
	// resetLoginFailures clears failed_attempts (to 0) and locked_until (to NULL)
	// on a successful login (FR3/D6). It is an idempotent whole-value overwrite —
	// safe here because success is not a concurrency contention point: a successful
	// login means the user holds the CORRECT password, which an attacker by definition
	// does not, so success is never an attacker-controlled race. In the worst case a
	// reset races a concurrent wrong-password failure (e.g. the legitimate user logs in
	// while an attacker guesses); last-writer-wins leaves the count at 1 rather than 0,
	// which only fails to clear a single attempt — it never over-locks and is no
	// security downgrade (D6). It reuses SetUserLock; failure counting must NOT (a
	// read-modify-write there would lose counts across instances, D2).
	resetLoginFailures(ctx context.Context, userID pgtype.UUID) error
	// touchUserTiming performs ONE primary-key UPDATE (updated_at = now()) purely to
	// equalize DB-write cost across the no-user / locked / disabled failure paths with
	// the wrong-password path's recordLoginFailure write (anti-enumeration timing
	// alignment, BLOCKER1). Without it those paths skip the DB write that wrong-password
	// performs, and the response-time difference leaks whether a username exists. The
	// no-user path passes a zero-value UUID (matches 0 rows); see login.go for the
	// WAL-equivalence reasoning.
	touchUserTiming(ctx context.Context, userID pgtype.UUID) error
	// persistSession writes the access and refresh token rows atomically. A login
	// must never leave a half-written session, so both inserts share one
	// transaction that either fully commits or fully rolls back.
	persistSession(ctx context.Context, rows sessionRows) error
}

// dbStore is the production store: it wraps the narrow DB seam and the generated
// queries. The non-transactional read goes through q; the write opens a
// transaction via the DB seam's Begin and binds the queries to it.
type dbStore struct {
	db DB
	q  *dbgen.Queries
}

// newDBStore builds the production store from the module's DB seam.
func newDBStore(db DB) *dbStore {
	return &dbStore{db: db, q: dbgen.New(db)}
}

// userByUsername reads a single user (plus the DB's now() as db_now) by normalized
// username via the generated case-insensitive query, surfacing pgx.ErrNoRows
// unchanged for the not-found branch.
func (s *dbStore) userByUsername(ctx context.Context, lowerUsername string) (dbgen.GetUserByUsernameRow, error) {
	return s.q.GetUserByUsername(ctx, lowerUsername)
}

// recordLoginFailure runs the atomic failure-count UPDATE (D2/FR1). The window is
// converted to a pgtype.Interval in microseconds — pgtype.Interval carries
// months/days/microseconds, and a lockout window is a plain duration, so all of it
// goes in the microseconds field (no month/day component) for an exact now()+window.
func (s *dbStore) recordLoginFailure(ctx context.Context, userID pgtype.UUID, threshold int32, window time.Duration) (dbgen.RecordLoginFailureRow, error) {
	return s.q.RecordLoginFailure(ctx, dbgen.RecordLoginFailureParams{
		ID:            userID,
		Threshold:     threshold,
		LockoutWindow: pgtype.Interval{Microseconds: window.Microseconds(), Valid: true},
	})
}

// resetLoginFailures clears the failure count and lock on success (FR3/D6) by
// reusing SetUserLock as an idempotent whole-value overwrite: failed_attempts=0,
// locked_until=NULL (an invalid pgtype.Timestamptz encodes SQL NULL).
//
// CONCURRENCY (MAJOR3, intentionally left as a whole-value overwrite): unlike
// recordLoginFailure (which MUST be a single atomic CASE UPDATE to avoid lost
// counts across instances), an unconditional overwrite is safe on the SUCCESS path
// because success is not an attacker-controlled contention point — only a holder of
// the correct password reaches here. If this reset races a concurrent wrong-password
// recordLoginFailure, last-writer-wins; the worst case is the legitimate user's
// count settling at 1 instead of 0, which neither over-locks nor weakens the
// lockout. The whole-value overwrite is also what gives the success path one
// DB-write of equivalent cost to every failure path (BLOCKER1 timing alignment).
func (s *dbStore) resetLoginFailures(ctx context.Context, userID pgtype.UUID) error {
	return s.q.SetUserLock(ctx, dbgen.SetUserLockParams{
		ID:             userID,
		FailedAttempts: 0,
		LockedUntil:    pgtype.Timestamptz{Valid: false},
	})
}

// touchUserTiming runs the equivalent-cost primary-key UPDATE that aligns the
// no-user / locked / disabled failure paths with the wrong-password path's DB write
// (BLOCKER1). It is a thin pass-through to the generated TouchUserTiming query; the
// caller supplies the real user id (locked/disabled) or a zero-value UUID (no-user,
// matches 0 rows).
func (s *dbStore) touchUserTiming(ctx context.Context, userID pgtype.UUID) error {
	return s.q.TouchUserTiming(ctx, userID)
}

// persistSession inserts the access and refresh token rows inside one
// transaction, mirroring handler.go's rotateInTx seam: begin via the narrow DB
// surface, bind the generated queries to the tx with dbgen.New(tx).WithTx(tx),
// run both writes, then commit. Any error rolls the whole thing back (the rollback
// after a successful commit is a documented pgx no-op).
func (s *dbStore) persistSession(ctx context.Context, rows sessionRows) (err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	qtx := dbgen.New(tx).WithTx(tx)
	if _, err = qtx.InsertAccessToken(ctx, rows.access); err != nil {
		return err
	}
	if _, err = qtx.InsertRefreshToken(ctx, rows.refresh); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Compile-time guard that the production store satisfies the seam login depends on.
var _ store = (*dbStore)(nil)
