package observability

// envelope_test.go pins the observability module's local writeError output
// byte-for-byte against internal/http.WriteError, so the error-envelope shape
// ({"error":{"code","message"},"requestId"}) can never drift between the two
// layers. The module's PRODUCTION code must NOT import internal/http (frozen
// dependency direction, NFR3), but this is a TEST file: a test import does not
// enter the package's production dependency closure (go list -deps, which
// verify.sh and dependency_guard_test.go use, excludes test-only imports), so the
// boundary is preserved while still giving a true cross-check. If either side's
// JSON shape changes, this test fails. Mirrors auth/envelope_test.go.

import (
	"net/http/httptest"
	"testing"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
)

// TestErrorEnvelope_MatchesHTTPLayer pins the module's local writeError output
// byte-for-byte against internal/http.WriteError.
func TestErrorEnvelope_MatchesHTTPLayer(t *testing.T) {
	const (
		status    = 400
		code      = "bad_request"
		message   = "schema validation failed: 1 of 100 events rejected"
		requestID = "req-abc-123"
	)

	// the module's local writer.
	modRec := httptest.NewRecorder()
	writeError(modRec, status, code, message, requestID)

	// internal/http's canonical writer with identical inputs.
	httpRec := httptest.NewRecorder()
	apphttp.WriteError(httpRec, status, code, message, requestID)

	if got, want := modRec.Code, httpRec.Code; got != want {
		t.Fatalf("status: observability=%d http=%d", got, want)
	}
	if got, want := modRec.Header().Get("Content-Type"), httpRec.Header().Get("Content-Type"); got != want {
		t.Fatalf("content-type: observability=%q http=%q", got, want)
	}
	if got, want := modRec.Body.String(), httpRec.Body.String(); got != want {
		t.Fatalf("envelope body diverged from internal/http.WriteError:\n  observability: %s\n  http: %s", got, want)
	}
}

// TestErrorEnvelope_CodesUsed asserts the slug constants the module emits are the
// expected stable strings, so a typo cannot silently change the wire code clients
// branch on. S2 appended payload_too_large (413) and rate_limited (429) to the
// frozen vocabulary, returned by the request hard limits and the rate-limit seam.
func TestErrorEnvelope_CodesUsed(t *testing.T) {
	cases := map[string]string{
		codeBadRequest:      "bad_request",
		codeInternal:        "internal",
		codePayloadTooLarge: "payload_too_large",
		codeRateLimited:     "rate_limited",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("error code slug = %q, want %q", got, want)
		}
	}
}
