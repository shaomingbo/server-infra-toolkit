package observability

// errors.go renders the frozen error-envelope shape LOCALLY, mirroring auth's
// writeError. The module must not import internal/http (frozen dependency
// direction), so it cannot call internal/http.WriteError; instead it produces the
// SAME bytes and envelope_test.go pins them byte-for-byte against the HTTP layer's
// writer so the two can never drift.

import (
	"encoding/json"
	"net/http"
)

// Error code slugs emitted by the observability module. These mirror the HTTP
// layer's stable vocabulary so a client sees one consistent set regardless of
// which layer produced the error. codeBadRequest/codeInternal reuse the existing
// frozen codes; codePayloadTooLarge and codeRateLimited are the T5 additions that
// land with the S2 enforcement actually returning them (the request hard limits
// and the rate-limit seam) plus the CONTRACTS append (FR9). They are appended to
// the frozen vocabulary — the envelope's top-level shape is unchanged.
const (
	codeBadRequest      = "bad_request"
	codeInternal        = "internal"
	codePayloadTooLarge = "payload_too_large"
	codeRateLimited     = "rate_limited"
)

// requestIDHeader mirrors the canonical header internal/http's request-id
// middleware sets on the response before any handler runs. The module must NOT
// import internal/http (dependency direction), so it reads the id off the
// response header rather than the request context key (an unexported type in
// internal/http). The value is present because main wires these routes INSIDE
// that middleware chain (same pattern as auth's requestIDHeader).
const requestIDHeader = "X-Request-Id"

// writeError emits the frozen error-envelope shape
// ({"error":{"code","message"},"requestId"}) used across the service. The module
// cannot import internal/http's WriteError (frozen dependency direction), so it
// renders the SAME shape locally; envelope_test.go pins this output byte-for-byte
// against internal/http.WriteError so the two can never drift. requestID comes
// from the X-Request-Id header the middleware set upstream.
func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	type envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	var env envelope
	env.Error.Code = code
	env.Error.Message = message
	env.RequestID = requestID

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}
