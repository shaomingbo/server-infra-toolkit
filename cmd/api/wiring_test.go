package main

// wiring_test.go pins the T5 ingest-auth ASSEMBLY seam (finding 3): it wires the
// REAL observability registrar through ingestVerifierRegistrar and the real
// IngestVerifier, exactly as run() does when EVENTS_INGEST_ENABLED is on, then
// drives the assembled handler with httptest. The point is to catch a "pattern
// drift" that the per-package unit tests cannot: the verifier guards "POST
// /v1/events" (the outer literal in ingestVerifierRegistrar) and the observability
// module registers "POST /v1/events" (the inner literal in its RegisterRoutes). If
// either literal moves, the verifier no longer sits in front of the events route
// and the unit tests — which test each side in isolation — would still pass while
// the real wiring is wide open or 404-only. Driving the REAL registrar end-to-end
// is what bites that seam.
//
// No real database is needed: a POST carrying a valid token with an empty body
// passes the verifier and reaches the observability handler, which rejects the
// empty body with a 400 BEFORE it ever touches the store/DB (the empty body fails
// JSON parsing up front). A 400 there is enough to prove the request PENETRATED the
// verifier (it is not the verifier's 401), so observability.NewHandler(nil) is a
// safe construction — the nil DB is unreachable on this path.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
	"github.com/shaomingbo/server-infra-toolkit/internal/modules/observability"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// Anchored token/hash pair (hex(sha256(utf8_bytes(token)))). This is the SAME
// fixture pair used in internal/http/ingest_auth_test.go (verified out-of-band with
// `printf '%s' <token> | shasum -a 256`); it is NOT a real credential. Mirrored here
// because package main cannot import that test's unexported constants.
const (
	wiringValidToken = "current-ingest-token-aaaa"
	wiringValidHash  = "269d1745a8ddf07873973f935cea8ed7ea4b1338a9d0ef584566e5886493f7da"
	wiringWrongToken = "wrong-ingest-token-cccc"
)

// assembleIngestServer builds the HTTP handler exactly as run() does for the
// ingest-enabled path: a real observability handler (nil DB — unreachable on the
// pre-store 400/401 paths exercised here), wrapped by the real IngestVerifier via
// ingestVerifierRegistrar, mounted on a real NewServer. Returning the assembled
// http.Handler lets each case drive it through httptest.
func assembleIngestServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{Version: "wiring-test"}
	obsHandler := observability.NewHandler(nil)
	verifier := apphttp.IngestVerifier([]string{wiringValidHash})
	registrar := ingestVerifierRegistrar(verifier, obsHandler.RegisterRoutes)
	// nil DB: /livez never touches it, and the ingest path under test rejects (401)
	// or 400s before the store is reached.
	return apphttp.NewServer(cfg, nil, registrar)
}

// post issues a POST /v1/events with an empty body and the given token header (no
// header when token is ""), returning the recorder.
func post(t *testing.T, srv http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	if token != "" {
		req.Header.Set("X-Ingest-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestIngestWiring_MissingTokenRejected pins that, in the REAL assembly, a POST with
// NO token gets the standard 401 unauthorized envelope — the verifier sits in front
// of the events route.
func TestIngestWiring_MissingTokenRejected(t *testing.T) {
	srv := assembleIngestServer(t)
	rec := post(t, srv, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without token: status = %d, want 401\nbody=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("401 body is not the error envelope: %v\n%s", err, rec.Body.String())
	}
	if env.Error.Code != "unauthorized" {
		t.Fatalf("error code = %q, want unauthorized", env.Error.Code)
	}
}

// TestIngestWiring_WrongTokenByteIdenticalToMissing pins that a wrong token and a
// missing token produce byte-for-byte identical 401 responses through the REAL
// assembly (E7/AC3), so a probe cannot tell them apart even end-to-end. The
// request-id middleware is active here, so to keep the only variable the failure
// class we pin the SAME inbound X-Request-Id on both requests (otherwise the
// generated ids would differ and mask the real comparison).
func TestIngestWiring_WrongTokenByteIdenticalToMissing(t *testing.T) {
	srv := assembleIngestServer(t)

	fixedID := "wiring-fixed-request-id"
	do := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
		req.Header.Set("X-Request-Id", fixedID)
		if token != "" {
			req.Header.Set("X-Ingest-Token", token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	missing := do("")
	wrong := do(wiringWrongToken)

	if missing.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("status: missing=%d wrong=%d, want both 401", missing.Code, wrong.Code)
	}
	if missing.Body.String() != wrong.Body.String() {
		t.Fatalf("401 bodies differ — must be byte-identical (E7)\nmissing=%q\nwrong=  %q",
			missing.Body.String(), wrong.Body.String())
	}
}

// TestIngestWiring_ValidTokenPenetrates pins that a VALID token passes the verifier
// in the REAL assembly: the request reaches the observability handler (which then
// 400s on the empty body, BEFORE the store/DB). A non-401 here proves penetration —
// if the inner or outer "POST /v1/events" literal drifted, the verifier would not
// front the route and this would not 400-from-the-handler.
func TestIngestWiring_ValidTokenPenetrates(t *testing.T) {
	srv := assembleIngestServer(t)
	rec := post(t, srv, wiringValidToken)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid token was rejected with 401 — it must pass the verifier and reach the handler\nbody=%s", rec.Body.String())
	}
	// The empty body is rejected by the observability handler as a 400 bad_request
	// (it never reaches the nil store). Pinning 400 specifically also proves the
	// request landed in the REAL ingest handler, not some other route.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("valid-token empty-body status = %d, want 400 (handler rejects the empty body before the store)\nbody=%s", rec.Code, rec.Body.String())
	}
}

// TestIngestWiring_NonPostIsNotVerifierRejected pins the finding-1 fix end-to-end: a
// GET to /v1/events must NOT reach the verifier (no 401, no rejection log). With the
// method pattern "POST /v1/events", the outer ServeMux routes only POST into the
// verifier; a GET falls through to NewServer's catch-all and gets the 404 envelope.
//
// NOTE ON 404 vs 405: the brief framed this as "405, not 401". Go's ServeMux only
// synthesizes a 405 when NO broader pattern covers the path — but NewServer always
// registers a catch-all "/", which DOES cover /v1/events, so a method-mismatched GET
// dispatches to that catch-all (404) instead of a 405. The security-relevant
// guarantee the finding is about still holds exactly: the GET never enters the
// verifier, so it cannot produce a 401 or a rejection-log line. This test asserts
// that (not-401) and pins the actual 404 the assembly returns.
func TestIngestWiring_NonPostIsNotVerifierRejected(t *testing.T) {
	srv := assembleIngestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("GET /v1/events returned 401 — a non-POST must NOT pass through the verifier (finding 1)\nbody=%s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/events status = %d, want 404 (method pattern routes only POST into the verifier; the GET falls through to the catch-all)\nbody=%s", rec.Code, rec.Body.String())
	}
}
