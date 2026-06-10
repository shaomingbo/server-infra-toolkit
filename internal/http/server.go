package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// DB is the narrow database surface the HTTP layer consumes. It is declared
// here, on the consumer side, intentionally: the http package must NOT import
// internal/platform/db, because AC11 requires `go list -deps internal/http` to
// stay free of the db package (the /livez liveness path must carry no DB
// dependency). The db package exports an equivalent db.DBTX for the same shape;
// *pgxpool.Pool satisfies both. Method signatures mirror pgx/pgxpool exactly.
//
// T1 wires the pool in via NewServer but no handler uses it yet (/livez never
// touches the DB). It exists so business handlers (T1+) take this small
// interface rather than a concrete pool type.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewServer builds the fully wired HTTP handler: the routes plus the fixed
// middleware chain. The version reported by /livez and the access log comes
// from cfg.Version, which main populates from the ldflags-injected value.
//
// db is injected for future business handlers; it is intentionally not used by
// /livez, which stays a pure liveness signal (AC11/NFR5). db may be nil in
// tests that exercise only DB-independent routes.
//
// registrars is the seam by which module routes are mounted WITHOUT this package
// importing any module: each registrar is a callback that adds its routes to the
// mux. main (cmd/api) constructs module handlers (e.g. auth) and passes their
// RegisterRoutes method values here, so internal/http never imports
// internal/modules/* (the frozen dependency direction). They run before the
// catch-all /, though ServeMux pattern precedence makes specific routes win
// regardless.
func NewServer(cfg *config.Config, db DB, registrars ...func(*http.ServeMux)) http.Handler {
	version := cfg.Version
	_ = db // reserved for T1+ business handlers; /livez never touches the DB.

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", livezHandler(version))

	// Mount module routes via the injected registrars (e.g. auth from main).
	for _, register := range registrars {
		register(mux)
	}

	// Catch-all: any unmatched route returns the 404 error envelope.
	mux.HandleFunc("/", notFoundHandler)

	return chain(version, mux)
}

// livezHandler reports liveness. It never touches the database or imports any
// db package: it is a pure, dependency-free liveness signal (AC11).
func livezHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version,
		})
	}
}

// notFoundHandler returns the 404 error envelope for unmatched routes.
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, CodeNotFound, "resource not found", RequestIDFromContext(r.Context()))
}
