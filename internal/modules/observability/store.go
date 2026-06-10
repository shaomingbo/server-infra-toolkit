package observability

// store.go is the narrow persistence seam the ingest handler depends on. The
// handler talks to this interface, not to the generated queries or the pool
// directly, so the validation/counting logic in events.go stays readable AND
// unit-testable without a real database (tests substitute a fake store to assert
// the accepted/duplicate/rejected counting and the zero-write-on-rejection
// invariant deterministically).
//
// The production implementation (dbStore) wraps the module's narrow DB seam and
// the generated queries; it owns the db.Begin -> per-row InsertEvent -> commit
// transaction that lands a whole batch atomically (FR4: one connection, one
// transaction; no per-event connection fan-out).

import (
	"context"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// store is the persistence surface the ingest flow consumes. It is intentionally
// tiny: land a batch of already-validated events idempotently and report how many
// were newly inserted (accepted); the caller derives duplicate = len(batch) -
// accepted.
type store interface {
	// insertBatch writes every row in one transaction with ON CONFLICT (source,
	// event_id) DO NOTHING, so a re-sent batch (client hold-and-retry) lands no
	// duplicate rows (D5/FR3). It returns the number of rows actually inserted
	// (accepted); the transaction is all-or-nothing, so a mid-batch failure rolls
	// the whole batch back and returns an error (FR4). An empty batch is the
	// caller's responsibility to reject before reaching here.
	insertBatch(ctx context.Context, rows []dbgen.InsertEventParams) (accepted int64, err error)
}

// dbStore is the production store: it wraps the narrow DB seam and the generated
// queries. Each row goes through InsertEvent bound to a single transaction opened
// via the DB seam's Begin.
type dbStore struct {
	db DB
	q  *dbgen.Queries
}

// newDBStore builds the production store from the module's DB seam.
func newDBStore(db DB) *dbStore {
	return &dbStore{db: db, q: dbgen.New(db)}
}

// insertBatch lands the whole batch in ONE transaction (FR4). It opens a
// transaction via the narrow DB seam, binds the generated queries to it, runs one
// idempotent InsertEvent per row accumulating the inserted count, then commits.
// Any error rolls the whole batch back (the deferred rollback after a successful
// commit is a documented pgx no-op).
func (s *dbStore) insertBatch(ctx context.Context, rows []dbgen.InsertEventParams) (accepted int64, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	qtx := s.q.WithTx(tx)
	for i := range rows {
		n, ierr := qtx.InsertEvent(ctx, rows[i])
		if ierr != nil {
			err = ierr
			return 0, err
		}
		accepted += n
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return accepted, nil
}

// Compile-time guard that the production store satisfies the seam the handler
// depends on.
var _ store = (*dbStore)(nil)
