package auth

import (
	"net/http/httptest"
	"testing"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
)

// TestErrorEnvelope_MatchesHTTPLayer (AC14) pins auth's local writeError output
// byte-for-byte against internal/http.WriteError, so the error-envelope shape
// ({"error":{"code","message"},"requestId"}) can never drift between the two
// layers. auth's PRODUCTION code must not import internal/http (frozen dependency
// direction), but this is a TEST file: a test import does not enter the auth
// package's production dependency closure (go list -deps, which verify.sh uses,
// excludes test-only imports), so the boundary is preserved while still giving a
// true cross-check. If either side's JSON shape changes, this test fails.
func TestErrorEnvelope_MatchesHTTPLayer(t *testing.T) {
	const (
		status    = 401
		code      = "unauthorized"
		message   = "invalid username or password"
		requestID = "req-abc-123"
	)

	// auth's local writer.
	authRec := httptest.NewRecorder()
	writeError(authRec, status, code, message, requestID)

	// internal/http's canonical writer with identical inputs.
	httpRec := httptest.NewRecorder()
	apphttp.WriteError(httpRec, status, code, message, requestID)

	if got, want := authRec.Code, httpRec.Code; got != want {
		t.Fatalf("status: auth=%d http=%d", got, want)
	}
	if got, want := authRec.Header().Get("Content-Type"), httpRec.Header().Get("Content-Type"); got != want {
		t.Fatalf("content-type: auth=%q http=%q", got, want)
	}
	if got, want := authRec.Body.String(), httpRec.Body.String(); got != want {
		t.Fatalf("envelope body diverged from internal/http.WriteError:\n  auth: %s\n  http: %s", got, want)
	}
}

// TestErrorEnvelope_CodesUsed asserts the slug constants auth emits are the
// expected stable strings, so a typo cannot silently change the wire code clients
// branch on.
func TestErrorEnvelope_CodesUsed(t *testing.T) {
	cases := map[string]string{
		codeUnauthorized: "unauthorized",
		codeBadRequest:   "bad_request",
		codeInternal:     "internal",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("error code slug = %q, want %q", got, want)
		}
	}
}
