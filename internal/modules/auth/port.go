// Package auth is the T2 authentication module. T1 builds only the foundation:
// the DB seam (this file), the HTTP handler skeleton (handler.go), and the
// Bearer middleware skeleton. No business logic lives here yet — argon2 hashing,
// login verification, token minting, refresh rotation/family replay detection,
// and Bearer verification are all stubs filled by later tasks (T2-T7).
//
// DEPENDENCY DIRECTION (frozen contract): this package MUST NOT import
// internal/http. It MAY import third-party libraries (pgx) and the generated
// query package (dbgen). The HTTP layer never imports this package either; main
// (cmd/api) is the only place that wires auth into the server (see handler.go
// and the NewServer route-callback seam).
package auth

import (
	"context"

	"github.com/jackc/pgx/v5"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// DB is the narrow database surface the auth module consumes. It is declared
// here, on the CONSUMER side, intentionally: auth depends on this small
// interface, not on the concrete *pgxpool.Pool or *db.Pool, so the pool type
// never crosses the auth boundary (the same pattern internal/http uses for its
// own DB interface).
//
// It is exactly the surface needed for both access paths:
//   - Non-transactional queries go through dbgen.New(db): that constructor takes
//     dbgen.DBTX (Exec/Query/QueryRow), which this interface embeds by including
//     those three methods.
//   - Transactional work (refresh rotation: mark-used + insert-new + issue-access
//     atomically) needs Begin. We add Begin(ctx) (pgx.Tx, error) here. Importing
//     pgx for the pgx.Tx type is allowed — pgx is a third-party library, not
//     internal/http — and is required to express the transaction capability
//     without leaking a concrete pool type.
//
// *db.Pool (the runtime retry pool) satisfies this interface natively; the
// compile-time guard for that assertion lives at the wiring seam in
// cmd/api/main.go, where the concrete db package may be imported.
type DB interface {
	dbgen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Compile-time guard that the generated Queries type can be built from this
// module's narrow DB surface (DB embeds dbgen.DBTX). This pins the seam: if a
// future sqlc upgrade widens dbgen.DBTX beyond what DB provides, the build
// breaks here instead of at every call site.
var _ dbgen.DBTX = (DB)(nil)
