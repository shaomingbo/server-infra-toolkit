// Package observability is the T5 event-ingestion module: it accepts batches of
// inbound Envelopes at POST /v1/events, validates each against a machine-readable
// inbound JSON Schema (the server's ACCEPTED shape), and idempotently lands the
// valid ones in a single statement (FR1-FR4/FR10).
//
// DEPENDENCY DIRECTION (frozen contract): this package MUST NOT import
// internal/http NOR internal/modules/auth. It MAY import third-party libraries
// (pgx, jsonschema) and the generated query package (dbgen). The HTTP layer never
// imports this package either; main (cmd/api) is the only place that wires the
// module into the server (see handler.go and the NewServer route-callback seam).
// The error envelope is therefore rendered LOCALLY (errors.go), not via
// internal/http.WriteError — envelope_test.go pins the two byte-for-byte.
package observability

import (
	"context"

	"github.com/jackc/pgx/v5"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// DB is the narrow database surface the observability module consumes. It is
// declared here, on the CONSUMER side, intentionally: the module depends on this
// small interface, not on the concrete *pgxpool.Pool or *db.Pool, so the pool type
// never crosses the module boundary (the same pattern auth and internal/http use).
//
// Ingestion lands a whole batch atomically. The ideal form is one multi-array
// unnest statement, but sqlc v1.31's static analyzer cannot type-infer a
// multi-arg unnest (see sql/events.sql header), so we take the brief's §4
// fallback: a single-row parameterized INSERT issued per event inside ONE
// explicit transaction. That still honors FR4 (one connection, one transaction
// for the whole request — no per-event connection fan-out), so the surface needs
// both dbgen.DBTX (for the non-tx read path, unused today) and Begin to open the
// batch transaction. Importing pgx for the pgx.Tx type is allowed (third-party
// library, not internal/http), the same way auth's DB seam does.
//
// *db.Pool (the runtime retry pool) satisfies this interface natively; the
// compile-time guard for that assertion lives at the wiring seam in
// cmd/api/main.go (S2), where the concrete db package may be imported.
type DB interface {
	dbgen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}
