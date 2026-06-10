package observability

// limits_test.go pins the FR5 request hard limits (AC5/AC6) through the REAL
// ingest handler with a fake store (no DB): an over-size request body and an
// over-count batch are each rejected with 413 payload_too_large and ZERO store
// writes — the billing ceiling that does not depend on max-instances. These run
// without a database (AC10).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postRaw drives an arbitrary raw body through the real handler (postBatch only
// sends JSON-marshalable values; the over-size-body case needs a raw payload that
// exceeds the cap without building a huge in-memory Go value).
func postRaw(t *testing.T, h *Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "conf-req-1")
	h.ingest(rec, req)
	return rec
}

// TestIngest_OversizeBodyIs413 asserts a request body larger than maxRequestBytes
// is rejected 413 payload_too_large (the MaxBytesReader stops the read at the cap)
// with zero store writes (AC5). The body is a syntactically-plausible JSON array
// padded past the 1 MiB cap so the rejection is the size cap, not a parse error.
func TestIngest_OversizeBodyIs413(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	// A JSON array whose single string element is padded past the 1 MiB cap.
	var b bytes.Buffer
	b.WriteString(`["`)
	b.WriteString(strings.Repeat("a", maxRequestBytes+1024))
	b.WriteString(`"]`)

	rec := postRaw(t, h, b.Bytes())

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (oversize body)\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codePayloadTooLarge)
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on an oversize body, want 0", fs.calls)
	}
}

// TestIngest_OverCountBatchIs413 asserts a batch carrying more than maxBatchSize
// events is rejected 413 payload_too_large (the count cap) with zero store writes
// (AC6). This double-bites with the batch schema's maxItems; here the explicit
// count-cap path is what produces the 413.
func TestIngest_OverCountBatchIs413(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	rec := postBatch(t, h, nEvents(maxBatchSize+1))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (over-count batch)\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codePayloadTooLarge)
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on an over-count batch, want 0", fs.calls)
	}
}

// TestBatchSchemaMaxItemsMatchesConstant pins the batch schema's maxItems (the
// correctness gate) to the maxBatchSize constant (the early/explicit count-cap),
// and minItems to 1 (empty-batch reject). The two numbers are deliberately
// duplicated — schema as the gate, constant as the explicit-reject path (see the
// events.go header) — so this test fails loudly if either drifts, instead of the
// double-bite silently disagreeing. It parses the embedded schema as raw JSON (no
// jsonschema library needed) to read the keyword values directly.
func TestBatchSchemaMaxItemsMatchesConstant(t *testing.T) {
	raw, err := contractFS.ReadFile("contract/batch.schema.json")
	if err != nil {
		t.Fatalf("read embedded batch schema: %v", err)
	}
	var doc struct {
		MinItems *int `json:"minItems"`
		MaxItems *int `json:"maxItems"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse batch schema JSON: %v", err)
	}
	if doc.MaxItems == nil {
		t.Fatal("batch schema has no maxItems")
	}
	if *doc.MaxItems != maxBatchSize {
		t.Errorf("batch schema maxItems = %d, want %d (== maxBatchSize); the schema gate and the count-cap constant drifted", *doc.MaxItems, maxBatchSize)
	}
	if doc.MinItems == nil {
		t.Fatal("batch schema has no minItems")
	}
	if *doc.MinItems != 1 {
		t.Errorf("batch schema minItems = %d, want 1 (empty-batch reject)", *doc.MinItems)
	}
}

// TestBillingCeilingConstantsPinned pins the two FR5 billing hard-ceiling
// constants to their literal values. These are NOT free to tune: they are the
// cost cap (max request bytes and max events per batch), so any change is a
// change to the spending ceiling and must be a deliberate decision — when you
// touch either, also update batch.schema.json maxItems, the PRD/CONTRACTS limit
// declarations, and the limit table in client-handoff.md so the contract, the
// docs, and the code never drift apart. This anchor is what makes such a change
// loud instead of silent.
func TestBillingCeilingConstantsPinned(t *testing.T) {
	if maxRequestBytes != 1<<20 {
		t.Errorf("maxRequestBytes = %d, want %d (1 MiB); this is the FR5 billing hard ceiling — see this test's header before changing", maxRequestBytes, 1<<20)
	}
	if maxBatchSize != 500 {
		t.Errorf("maxBatchSize = %d, want 500; this is the FR5 billing hard ceiling — see this test's header before changing", maxBatchSize)
	}
}

// TestIngest_MaxCountBatchAccepted asserts the boundary just below the cap is
// accepted (exactly maxBatchSize events pass the count cap and reach the store),
// so the cap is an upper bound, not an off-by-one rejection of the max batch.
func TestIngest_MaxCountBatchAccepted(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	rec := postBatch(t, h, nEvents(maxBatchSize))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (exactly maxBatchSize events)\nbody=%s", rec.Code, rec.Body.String())
	}
	if fs.calls != 1 || fs.lastRows != maxBatchSize {
		t.Fatalf("store calls=%d rows=%d, want calls=1 rows=%d", fs.calls, fs.lastRows, maxBatchSize)
	}
}
