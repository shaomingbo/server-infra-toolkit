package observability

// contract_conformance_test.go is the inbound half of the T5 contract: it drives
// real HTTP requests through the real ingest handler (with a fake store, no DB)
// and asserts that
//   - a VALID batch is accepted (200) and reaches the store, and
//   - six INVALID batches are each rejected (4xx) with the frozen error envelope
//     and ZERO writes to the store.
//
// This is the T3 paradigm INVERTED to inbound (PRD D3): the server holds the
// accepted-shape schema and the conformance test proves the handler REJECTS
// malformed input, rather than self-validating a response it produces. Each
// negative case proves a specific tightening keyword bites:
//   - an EXTRA field        -> additionalProperties:false
//   - a MISSING required    -> required (eventId)
//   - a wrong-typed field   -> type (timestampMs as string)
//   - an out-of-enum kind   -> enum (kind)
//   - a nested-object value -> the AttributeValue closed set (no object value)
//   - an array value        -> the AttributeValue closed set (no array value)
//
// FAIL-CLOSED (AC7): the schemas are compiled from go:embed'd files at package
// init (schema.go). Deleting contract/*.json makes this package fail to BUILD —
// a stronger fail-closed than a runtime disk read that could silently skip — so
// `go test` cannot pass with a missing schema. TestSchemasCompiled below pins the
// compiled schemas non-nil as a belt-and-suspenders assertion that init ran.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// fakeStore is a no-DB substitute for the persistence seam. It records every
// insertBatch call so a test can assert ZERO writes happened on a rejected batch,
// and how many rows a successful batch carried.
type fakeStore struct {
	calls     int
	lastRows  int
	accepted  int64
	returnErr error
}

func (f *fakeStore) insertBatch(_ context.Context, rows []dbgen.InsertEventParams) (int64, error) {
	f.calls++
	f.lastRows = len(rows)
	if f.returnErr != nil {
		return 0, f.returnErr
	}
	// Default: treat every row as newly accepted unless a test overrides accepted.
	if f.accepted == 0 {
		return int64(len(rows)), nil
	}
	return f.accepted, nil
}

// newTestHandler builds a real Handler over a fake store (no DB), the way the
// unit/conformance tests exercise the ingest flow black-box. It installs the
// no-op rate limiter so the ingest seam call site does not nil-panic (the same
// default NewHandler wires).
func newTestHandler(fs *fakeStore) *Handler {
	return &Handler{store: fs, rateLimiter: noopRateLimiter{}}
}

// validEvent is one schema-valid inbound event (every required field present and
// correctly typed). Tests mutate a copy of it to build negative cases.
func validEvent() map[string]any {
	return map[string]any{
		"eventId":     "evt-0001",
		"kind":        "log",
		"traceId":     "trace-aaaa",
		"timestampMs": 1748563200123,
		"source":      "client-ios",
		"name":        "app.launch",
		"attributes": map[string]any{
			"screen":  "home",
			"latency": 42,
			"ok":      true,
			"note":    nil,
		},
	}
}

// postBatch sends a batch (any JSON-encodable value) through the real handler and
// returns the recorder. It sets the X-Request-Id header the middleware would set
// upstream, so the handler reads a non-empty requestId.
func postBatch(t *testing.T, h *Handler, batch any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "conf-req-1")
	h.ingest(rec, req)
	return rec
}

// assertErrorEnvelope decodes a 4xx response and asserts the frozen envelope
// shape: {"error":{"code","message"},"requestId"} with a non-empty code/message
// and the request id echoed back.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\nbody=%s", err, rec.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q", env.Error.Code, wantCode)
	}
	if env.Error.Message == "" {
		t.Error("error.message is empty")
	}
	if env.RequestID != "conf-req-1" {
		t.Errorf("requestId = %q, want %q", env.RequestID, "conf-req-1")
	}
}

// TestSchemasCompiled pins that package init compiled both schemas (fail-closed
// belt-and-suspenders: init panics on a bad/missing embedded schema, so reaching
// here with non-nil schemas proves the embedded contract files are present and
// valid). Deleting contract/*.json makes the package fail to build entirely.
func TestSchemasCompiled(t *testing.T) {
	if eventSchema == nil {
		t.Fatal("eventSchema is nil — embedded event schema failed to compile (fail-closed)")
	}
	if batchSchema == nil {
		t.Fatal("batchSchema is nil — embedded batch schema failed to compile (fail-closed)")
	}
}

// TestIngest_AcceptsValidBatch is the positive case: a schema-valid batch passes
// the real handler, reaches the store, and returns 200 with the count response.
func TestIngest_AcceptsValidBatch(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	rec := postBatch(t, h, []any{validEvent()})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody=%s", rec.Code, rec.Body.String())
	}
	if fs.calls != 1 {
		t.Fatalf("store insertBatch called %d times, want 1", fs.calls)
	}
	if fs.lastRows != 1 {
		t.Fatalf("store received %d rows, want 1", fs.lastRows)
	}
	var resp ingestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode success body: %v", err)
	}
	if resp.Accepted != 1 || resp.Duplicate != 0 || resp.Rejected != 0 {
		t.Errorf("counts = %+v, want accepted=1 duplicate=0 rejected=0", resp)
	}
}

// negativeCases are the five malformed batches, each proving a specific schema
// keyword bites through the REAL handler path.
func negativeCases() map[string]any {
	extra := validEvent()
	extra["unexpected"] = "should-be-rejected" // additionalProperties:false

	missing := validEvent()
	delete(missing, "eventId") // required

	typed := validEvent()
	typed["timestampMs"] = "1748563200123" // type:integer

	enumed := validEvent()
	enumed["kind"] = "metric" // enum (kind ∉ {log,crash,telemetry})

	nested := validEvent()
	nested["attributes"] = map[string]any{
		"obj": map[string]any{"deep": 1}, // AttributeValue closed set: no nested object
	}

	arrayAttr := validEvent()
	arrayAttr["attributes"] = map[string]any{
		"list": []any{"a"}, // AttributeValue closed set: no array value either
	}

	return map[string]any{
		"extra field (additionalProperties)": extra,
		"missing eventId (required)":         missing,
		"string timestampMs (type)":          typed,
		"out-of-enum kind (enum)":            enumed,
		"nested-object attribute (closed)":   nested,
		"array attribute value (closed)":     arrayAttr,
	}
}

// TestIngest_RejectsMalformedEvent runs each negative case through the real
// handler and asserts: 4xx, frozen error envelope, and ZERO store writes (the
// batch is rejected before any persistence — FR10/AC2/AC4).
func TestIngest_RejectsMalformedEvent(t *testing.T) {
	for name, bad := range negativeCases() {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{}
			h := newTestHandler(fs)

			rec := postBatch(t, h, []any{bad})

			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want 4xx\nbody=%s", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec, codeBadRequest)
			if fs.calls != 0 {
				t.Fatalf("store insertBatch called %d times on a rejected batch, want 0 (zero-write invariant)", fs.calls)
			}
		})
	}
}
