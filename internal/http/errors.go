// Package http builds the service's HTTP handler: the middleware chain, the
// error envelope writer, and the routes. It depends on internal/platform/* but
// never on any module implementation or database package.
package http

import (
	"encoding/json"
	"net/http"
)

// Generic error code slugs for T0. Business error codes are out of scope; these
// are stable, human-meaningful slugs used in the error envelope.
const (
	CodeInternal   = "internal"
	CodeBadRequest = "bad_request"
	CodeNotFound   = "not_found"
	// CodeUnauthorized is the credential-failure slug. It mirrors the value the
	// auth module emits locally (codeUnauthorized) and is reused by the ingest
	// verifier (FR5/D4: 401 reuses the existing "unauthorized" code, no new code).
	// This is an append-only addition; the error envelope's top-level shape is
	// unchanged (CONTRACTS frozen envelope).
	CodeUnauthorized = "unauthorized"
)

// errorEnvelope is the frozen top-level shape of every error response.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"requestId"`
}

// WriteError is the single error exit for the HTTP layer. It sets the JSON
// content type, writes the status, and emits the frozen error envelope.
func WriteError(w http.ResponseWriter, status int, code, message, requestID string) {
	var env errorEnvelope
	env.Error.Code = code
	env.Error.Message = message
	env.RequestID = requestID

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Best-effort encode; if the client connection is gone there is nothing to
	// recover, and the status/headers are already committed.
	_ = json.NewEncoder(w).Encode(env)
}
