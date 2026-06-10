package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

func testServer() http.Handler {
	// nil DB: these tests exercise only DB-independent routes (/livez, 404).
	return NewServer(&config.Config{Version: "test-version"}, nil)
}

// AC1: /livez returns 200 and does not depend on the database.
func TestLivez_OK(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
	if body["version"] != "test-version" {
		t.Fatalf("version field = %q, want test-version", body["version"])
	}
}

// AC2a: two requests in the same process get distinct generated request ids.
func TestRequestID_DistinctAcrossRequests(t *testing.T) {
	srv := testServer()

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/livez", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Header().Get(requestIDHeader)
	}

	id1, id2 := get(), get()
	if id1 == "" || id2 == "" {
		t.Fatalf("expected non-empty request ids, got %q and %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("expected distinct request ids, both were %q", id1)
	}
}

// AC2c: an inbound X-Request-Id is echoed and used as the request id.
func TestRequestID_InboundEchoed(t *testing.T) {
	srv := testServer()

	const inbound = "client-supplied-id"
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	req.Header.Set(requestIDHeader, inbound)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get(requestIDHeader); got != inbound {
		t.Fatalf("echoed request id = %q, want %q", got, inbound)
	}
}

// AC2b: a panicking handler is recovered: the response is a 5xx envelope and the
// process (test) does not crash.
func TestRecover_PanicReturns500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	handler := chain("test-version", mux)

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	// Must not propagate the panic out of ServeHTTP.
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != CodeInternal {
		t.Fatalf("error code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if env.Error.Message == "" {
		t.Fatalf("error message is empty")
	}
	if env.RequestID == "" {
		t.Fatalf("requestId is empty")
	}
}

// AC3: an unknown route returns the fixed 404 envelope with matching content
// type, and the body's requestId equals the echoed X-Request-Id header.
func TestNotFound_Envelope(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != CodeNotFound {
		t.Fatalf("error code = %q, want %q", env.Error.Code, CodeNotFound)
	}
	if env.Error.Message == "" {
		t.Fatalf("error message is empty")
	}

	headerID := rec.Header().Get(requestIDHeader)
	if headerID == "" {
		t.Fatalf("response is missing %s header", requestIDHeader)
	}
	if env.RequestID != headerID {
		t.Fatalf("envelope requestId %q != header %q", env.RequestID, headerID)
	}
}

// TestEventsIngest_NotMountedWhenFlagOff pins the seam-first feature-flag behavior
// (FR1/AC9/D2): when the EVENTS_INGEST_ENABLED flag is off, main does not pass the
// observability registrar to NewServer, so POST /v1/events is never registered and
// hits the catch-all 404. testServer() registers no module routes — exactly the
// flag-off wiring — so a POST here proves the route does not exist by default. This
// lives in package http (not the observability module) precisely so it does NOT
// import internal/modules/observability, keeping the frozen dependency direction
// intact (internal/http never imports a module).
func TestEventsIngest_NotMountedWhenFlagOff(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/events status = %d, want 404 (route must not be mounted when the flag is off)\nbody=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != CodeNotFound {
		t.Fatalf("error code = %q, want %q (the disabled endpoint is indistinguishable from any unknown route)", env.Error.Code, CodeNotFound)
	}
}
