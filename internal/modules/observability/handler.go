package observability

// handler.go holds the observability module's HTTP handler skeleton and route
// registration, mirroring auth's Handler/NewHandler/RegisterRoutes shape (D10).
// The ingest flow itself lives in events.go.

import (
	"net/http"
)

// Handler holds the observability module's HTTP handlers and their dependencies.
// It takes the narrow DB seam (port.go), never a concrete pool type. store is the
// persistence surface the ingest flow consumes (events.go), defaulting to a
// dbStore over the same seam but substitutable in tests with a fake. rateLimiter
// is the FR7/D7 facade — a no-op by default that imposes no limit (NOT a security
// or billing control; the billing ceiling is the FR5 request hard limits below).
type Handler struct {
	store       store
	rateLimiter RateLimiter
}

// NewHandler builds the observability Handler from the narrow DB seam. main
// (cmd/api) constructs it with the runtime *db.Pool (which satisfies DB) and
// mounts it ONLY when the EVENTS_INGEST_ENABLED feature flag is set (D2:
// seam-first, the endpoint stays off the public router by default). The
// rate-limiter seam defaults to the no-op (no limiting; see ratelimit.go / D7).
func NewHandler(db DB) *Handler {
	return &Handler{
		store:       newDBStore(db),
		rateLimiter: noopRateLimiter{},
	}
}

// RegisterRoutes registers the observability routes on the provided mux. It is the
// seam main uses to mount the module without internal/http ever importing this
// package: NewServer accepts a func(*http.ServeMux) registrar, and main passes
// this method value. Routes live under /v1/ (the business-API prefix; probes stay
// bare).
//
// ingest is implemented in events.go (decode -> per-event schema validation ->
// single-transaction idempotent insert -> count response).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/events", h.ingest)
}
