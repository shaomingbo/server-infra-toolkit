package http

// ingest_auth_test.go is the behavioral test for the T5 ingest-token verifier
// (AC2/AC3/AC4/AC7/AC8). It exercises the func(http.Handler) http.Handler
// middleware through httptest, asserting: either configured hash passes and a
// removed previous hash fails (AC2), missing-header and wrong-token responses are
// byte-for-byte identical (AC3), verification precedes any body read (AC4), and
// the verifier package carries no auth/observability dependency (AC7).
//
// The token/hash pairs are anchored to real SHA-256 digests (verified with an
// independent `shasum -a 256` of the literal token bytes), so a hash-convention
// drift in config.HashIngestToken would fail these tests, not just pass on a
// self-referential value.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// Anchored token/hash pairs (hex(sha256(utf8_bytes(token))), verified out-of-band
// with `printf '%s' <token> | shasum -a 256`). These are test fixtures, NOT real
// credentials.
const (
	currentIngestToken = "current-ingest-token-aaaa"
	currentIngestHash  = "269d1745a8ddf07873973f935cea8ed7ea4b1338a9d0ef584566e5886493f7da"

	previousIngestToken = "previous-ingest-token-bbbb"
	previousIngestHash  = "94300b50e4697a67820524d29b1ac93f82002a567f2984a83fdcfb1489bff68a"

	wrongIngestToken = "wrong-ingest-token-cccc"
)

// okHandler is the downstream handler the verifier gates: it writes 200 and a
// sentinel body, so a test can tell "passed the verifier" (200 + sentinel) from
// "rejected" (401 + envelope) unambiguously.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "PASSED")
	})
}

// TestIngestVerifier_DualHashEitherPasses pins AC2: with current+previous
// configured, a request carrying EITHER token passes; after previous is removed
// from the config, the old token is rejected with 401.
func TestIngestVerifier_DualHashEitherPasses(t *testing.T) {
	dual := IngestVerifier([]string{currentIngestHash, previousIngestHash})(okHandler())

	for _, tok := range []string{currentIngestToken, previousIngestToken} {
		req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
		req.Header.Set(ingestTokenHeader, tok)
		rec := httptest.NewRecorder()
		dual.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("token %q: status = %d, want 200 (either configured hash must pass)", tok, rec.Code)
		}
		if rec.Body.String() != "PASSED" {
			t.Fatalf("token %q: body = %q, want PASSED", tok, rec.Body.String())
		}
	}

	// Rotate previous OUT: only current remains, so the old token now 401s (AC2).
	currentOnly := IngestVerifier([]string{currentIngestHash})(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	req.Header.Set(ingestTokenHeader, previousIngestToken)
	rec := httptest.NewRecorder()
	currentOnly.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("removed previous token: status = %d, want 401", rec.Code)
	}
}

// TestIngestVerifier_FailuresByteIdentical pins AC3/E7: a missing X-Ingest-Token
// header and a wrong token produce byte-for-byte identical 401 responses, so a
// probe cannot tell them apart. The verifier is tested directly (NOT under the
// request-id middleware) so the envelope's requestId is empty and identical in
// both cases — the comparison is meaningful only when the only variable is the
// failure class, which is exactly what this asserts.
func TestIngestVerifier_FailuresByteIdentical(t *testing.T) {
	verifier := IngestVerifier([]string{currentIngestHash})(okHandler())

	// (a) missing header entirely.
	missingReq := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	missingRec := httptest.NewRecorder()
	verifier.ServeHTTP(missingRec, missingReq)

	// (b) present but wrong token.
	wrongReq := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	wrongReq.Header.Set(ingestTokenHeader, wrongIngestToken)
	wrongRec := httptest.NewRecorder()
	verifier.ServeHTTP(wrongRec, wrongReq)

	if missingRec.Code != http.StatusUnauthorized || wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("status: missing=%d wrong=%d, want both 401", missingRec.Code, wrongRec.Code)
	}
	if missingRec.Body.String() != wrongRec.Body.String() {
		t.Fatalf("response bodies differ — must be byte-identical (E7)\nmissing=%q\nwrong=  %q",
			missingRec.Body.String(), wrongRec.Body.String())
	}
	// The full header set must match too, not just the body: a differing header
	// (e.g. a length or a stray field present in only one path) would let a probe
	// distinguish missing-header from wrong-token even with identical bodies (E7).
	if !reflect.DeepEqual(missingRec.Header(), wrongRec.Header()) {
		t.Fatalf("response headers differ — must be identical (E7)\nmissing=%v\nwrong=  %v",
			missingRec.Header(), wrongRec.Header())
	}
	if ct := wrongRec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	// The body is the frozen unauthorized envelope with the existing code (FR5/D4).
	if !strings.Contains(wrongRec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("body does not carry the unauthorized code: %q", wrongRec.Body.String())
	}
}

// countingReadCloser counts Read calls so a test can assert the body was never
// touched. Read returns the configured bytes; Close is a no-op.
type countingReadCloser struct {
	reads int
	data  []byte
	off   int
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	c.reads++
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.off:])
	c.off += n
	return n, nil
}

func (c *countingReadCloser) Close() error { return nil }

// TestIngestVerifier_RejectsBeforeBodyRead pins AC4: a 2 MiB unauthenticated
// request returns 401 (NOT 413) and the verifier does not read a single byte of
// the body. The body is a counting reader whose Read must never be called on the
// rejection path.
func TestIngestVerifier_RejectsBeforeBodyRead(t *testing.T) {
	verifier := IngestVerifier([]string{currentIngestHash})(okHandler())

	body := &countingReadCloser{data: make([]byte, 2<<20)} // 2 MiB
	req := httptest.NewRequest(http.MethodPost, "/v1/events", http.NoBody)
	req.Body = body
	req.Header.Set(ingestTokenHeader, wrongIngestToken) // wrong → rejected

	rec := httptest.NewRecorder()
	verifier.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (must reject before the body-size gate, not 413)", rec.Code)
	}
	if body.reads != 0 {
		t.Fatalf("verifier read the request body %d time(s); it must reject before any body read (AC4)", body.reads)
	}
}

// TestIngestVerifier_DoesNotGuardOtherRoutes pins AC8: the verifier only gates the
// route it wraps. Routes NOT wrapped by it (e.g. /livez via the full server)
// behave as before without any X-Ingest-Token. This exercises the assembled
// server, which never wraps /livez with the verifier.
func TestIngestVerifier_DoesNotGuardOtherRoutes(t *testing.T) {
	srv := testServer() // no ingest verifier wraps /livez

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/livez without X-Ingest-Token: status = %d, want 200 (verifier must not guard it)", rec.Code)
	}
}

// captureIngestRejects swaps ingestRejectLogger for one writing to buf for the
// duration of the test, restoring the original on cleanup — the SAME pattern as
// auth's captureLockoutEvents.
func captureIngestRejects(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := ingestRejectLogger
	ingestRejectLogger = slog.New(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { ingestRejectLogger = orig })
	return &buf
}

// TestIngestVerifier_LogsRejection pins AC5: every 401 rejection emits exactly one
// structured line with event=ingest_auth_rejected and a request_id field, and the
// line NEVER carries the presented token (or its hash). The verifier runs outside
// requestIDMiddleware here, so request_id is the empty string — present but blank.
func TestIngestVerifier_LogsRejection(t *testing.T) {
	buf := captureIngestRejects(t)

	verifier := IngestVerifier([]string{currentIngestHash})(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	req.Header.Set(ingestTokenHeader, wrongIngestToken) // wrong → rejected
	rec := httptest.NewRecorder()
	verifier.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	// One rejection → exactly one JSON line (single trailing newline, no extras).
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no rejection line was emitted")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("emitted more than one rejection line, want exactly 1:\n%s", buf.String())
	}

	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("event is not valid JSON: %v\n%s", err, line)
	}
	// (a) event field.
	if ev["event"] != "ingest_auth_rejected" {
		t.Fatalf("event field = %v, want ingest_auth_rejected", ev["event"])
	}
	// (b) request_id field is present (blank is fine outside the middleware).
	if _, ok := ev["request_id"]; !ok {
		t.Fatalf("rejection line missing request_id field: %s", line)
	}
	// (c) the line must not leak any credential-bearing material (AC5): not the
	// presented token, not the hash of any configured token (current or previous),
	// not the hash the verifier computed from the presented token, and not even the
	// header NAME — logging the header label is the first step toward logging its
	// value, so the guard forbids it outright.
	forbidden := map[string]string{
		"presented token":      wrongIngestToken,
		"current hash":         currentIngestHash,
		"previous hash":        previousIngestHash,
		"presented-token hash": config.HashIngestToken(wrongIngestToken),
		"header name":          ingestTokenHeader,
	}
	for label, secret := range forbidden {
		if strings.Contains(line, secret) {
			t.Fatalf("rejection line leaks %s (%q): %s", label, secret, line)
		}
	}
}

// TestIngestVerifier_NoForbiddenDependency pins AC7: the package that defines the
// verifier (internal/http) must NOT depend on internal/modules/auth (the verifier
// must not import auth or query access_tokens). It already cannot import the db
// package (livez_guard_test.go), so this adds the auth-direction assertion,
// mirroring the module dependency guards.
func TestIngestVerifier_NoForbiddenDependency(t *testing.T) {
	const authPkgPath = "github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"

	// Reuse livez_guard_test.go's runGoListDeps helper (same package) rather than
	// shelling out to `go list` again — one mechanism for the dependency-direction
	// gate.
	out, err := runGoListDeps(t, httpPkg)
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", httpPkg, err, out)
	}
	for _, dep := range strings.Fields(out) {
		if dep == authPkgPath || strings.HasPrefix(dep, authPkgPath+"/") {
			t.Fatalf("internal/http depends on %s — the ingest verifier must NOT import auth or query access_tokens (AC7)", dep)
		}
	}
}
