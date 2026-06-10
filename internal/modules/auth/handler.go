package auth

import (
	"net/http"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// requestIDHeader mirrors the canonical header internal/http's requestIDMiddleware
// sets on the response before any handler runs. auth must NOT import internal/http
// (dependency direction), so it reads the id off the response header rather than
// the request context key (which is an unexported type in internal/http). The
// value is present because main wires auth routes INSIDE that middleware chain.
const requestIDHeader = "X-Request-Id"

// Handler holds the auth module's HTTP handlers and their dependencies. It takes
// the narrow DB seam (port.go), never a concrete pool type. db and q drive the
// refresh/Bearer seams; store is the persistence surface the login flow consumes
// (login.go), defaulting to a dbStore over the same seam but substitutable in
// tests. rateLimiter is the FR12/D9 facade — a no-op by default that imposes no
// limit (the real brute-force defense is the DB account lockout, not this seam).
type Handler struct {
	db          DB
	q           *dbgen.Queries
	store       store
	rateLimiter RateLimiter
}

// NewHandler builds the auth Handler from the narrow DB seam. main (cmd/api)
// constructs it with the runtime *db.Pool, which satisfies DB. The rate-limiter
// seam defaults to the no-op (no limiting; see ratelimit.go / D9).
func NewHandler(db DB) *Handler {
	return &Handler{
		db:          db,
		q:           dbgen.New(db),
		store:       newDBStore(db),
		rateLimiter: noopRateLimiter{},
	}
}

// RegisterRoutes registers the auth routes on the provided mux. It is the seam
// main uses to mount auth without internal/http ever importing this package:
// NewServer accepts a func(*http.ServeMux) registrar, and main passes this method
// value. Routes live under /v1/ (the business-API prefix; probes stay bare).
//
// login is implemented in login.go (credential verification + token minting);
// refresh is implemented in refresh.go (row-locked rotation + replay detection).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
}

// Bearer middleware lives in middleware.go.
