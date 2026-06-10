package db

// row_test.go covers the QueryRow path's row adapter (retryRow), which turns the
// retrying Query's (pgx.Rows, error) result back into a single pgx.Row with the
// standard pgx QueryRow semantics (P1 #6 refactor). The end-to-end retry of the
// (*Pool).Exec/Query/QueryRow/Begin methods is exercised structurally by retry()
// (retry_test.go) since those methods are thin retry(...) wrappers over the
// concrete *pgxpool.Pool — a concrete type that cannot be faked without a real
// database (E5/NFR8). What IS deterministically testable, and what the refactor
// changed, is retryRow.Scan, covered here against a fake pgx.Rows.

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRows is a minimal pgx.Rows for testing retryRow.Scan. It models a result
// set of n rows, an optional error surfaced via Err(), and records whether Close
// was called (retryRow must always close).
type fakeRows struct {
	remaining int   // rows left to hand out via Next()
	err       error // returned by Err()
	scanErr   error // returned by Scan()
	scanned   bool  // set true if Scan() was invoked
	closed    bool  // set true if Close() was invoked
}

func (r *fakeRows) Close()     { r.closed = true }
func (r *fakeRows) Err() error { return r.err }
func (r *fakeRows) Next() bool {
	if r.remaining <= 0 {
		return false
	}
	r.remaining--
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	r.scanned = true
	return r.scanErr
}

// Unused-by-retryRow methods, present to satisfy the pgx.Rows interface.
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

var _ pgx.Rows = (*fakeRows)(nil)

// TestRetryRow_QueryError: when the retrying Query itself failed (retries
// exhausted or a real server error), Scan returns that error and still closes
// the rows.
func TestRetryRow_QueryError(t *testing.T) {
	queryErr := errors.New("query failed after retries")
	rows := &fakeRows{}
	row := &retryRow{rows: rows, err: queryErr}

	if err := row.Scan(); !errors.Is(err, queryErr) {
		t.Fatalf("Scan() = %v, want the query error %v", err, queryErr)
	}
	if !rows.closed {
		t.Fatal("Scan() did not close the rows on the query-error path")
	}
}

// TestRetryRow_NoRows: an empty result set returns pgx.ErrNoRows (matching pgx's
// own QueryRow semantics) and closes the rows.
func TestRetryRow_NoRows(t *testing.T) {
	rows := &fakeRows{remaining: 0}
	row := &retryRow{rows: rows}

	if err := row.Scan(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Scan() on empty result = %v, want pgx.ErrNoRows", err)
	}
	if rows.scanned {
		t.Fatal("Scan() should not have scanned a row when there were none")
	}
	if !rows.closed {
		t.Fatal("Scan() did not close the rows on the no-rows path")
	}
}

// TestRetryRow_OneRow: a single row is scanned, no error, rows closed.
func TestRetryRow_OneRow(t *testing.T) {
	rows := &fakeRows{remaining: 1}
	row := &retryRow{rows: rows}

	if err := row.Scan(); err != nil {
		t.Fatalf("Scan() = %v, want nil", err)
	}
	if !rows.scanned {
		t.Fatal("Scan() did not scan the available row")
	}
	if !rows.closed {
		t.Fatal("Scan() did not close the rows")
	}
}

// TestRetryRow_MultipleRowsIgnoresExtra: with more than one row, the first is
// scanned and the rest are ignored (rows closed after the first), matching pgx
// QueryRow which discards additional rows.
func TestRetryRow_MultipleRowsIgnoresExtra(t *testing.T) {
	rows := &fakeRows{remaining: 3}
	row := &retryRow{rows: rows}

	if err := row.Scan(); err != nil {
		t.Fatalf("Scan() = %v, want nil", err)
	}
	if !rows.scanned {
		t.Fatal("Scan() did not scan the first row")
	}
	if !rows.closed {
		t.Fatal("Scan() did not close the rows after taking the first")
	}
}

// TestRetryRow_ErrBeforeIteration: an error already captured on the Rows (Err()
// non-nil before Next) is surfaced and the rows are closed.
func TestRetryRow_ErrBeforeIteration(t *testing.T) {
	rowsErr := errors.New("result-set error")
	rows := &fakeRows{remaining: 1, err: rowsErr}
	row := &retryRow{rows: rows}

	if err := row.Scan(); !errors.Is(err, rowsErr) {
		t.Fatalf("Scan() = %v, want the rows error %v", err, rowsErr)
	}
	if !rows.closed {
		t.Fatal("Scan() did not close the rows on the rows-error path")
	}
}
