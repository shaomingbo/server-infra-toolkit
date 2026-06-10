package observability

// events_test.go is the no-DB unit coverage of the ingest flow's counting and
// envelope semantics. It drives the REAL handler over fake stores (fakeStore is
// defined in contract_conformance_test.go) so the accepted/duplicate/rejected
// derivation, the partial-duplicate count, the empty-/non-array rejection, and
// the DB-failure 5xx are pinned deterministically without a real database
// (AC10/AC4/FR10/D8).
//
// The idempotency/transaction SEMANTICS against a real Postgres (re-send N times
// -> rows=1) live in events_integration_test.go (TEST_DATABASE_URL-gated); here
// the store is faked, so these tests assert the handler's bookkeeping, not the
// SQL ON CONFLICT behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// nEvents builds a batch of n distinct schema-valid events (distinct eventId so
// the batch shape stays valid).
func nEvents(n int) []any {
	batch := make([]any, 0, n)
	for i := 0; i < n; i++ {
		e := validEvent()
		e["eventId"] = "evt-" + strconv.Itoa(i)
		batch = append(batch, e)
	}
	return batch
}

// newTestHandlerWith builds a real Handler over an arbitrary store implementation,
// with the no-op rate limiter wired (so the ingest seam call site does not
// nil-panic).
func newTestHandlerWith(s store) *Handler {
	return &Handler{store: s, rateLimiter: noopRateLimiter{}}
}

// TestIngest_DuplicateCount asserts duplicate = len(batch) - accepted: when the
// store reports fewer accepted rows than the batch size (some events were skipped
// by ON CONFLICT, simulated via fakeStore.accepted), the handler reports the
// remainder as duplicate and rejected stays 0 (E8/D8/FR10).
func TestIngest_DuplicateCount(t *testing.T) {
	fs := &fakeStore{accepted: 3} // store says 3 of 5 were newly inserted
	h := newTestHandler(fs)

	rec := postBatch(t, h, nEvents(5))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody=%s", rec.Code, rec.Body.String())
	}
	if fs.calls != 1 {
		t.Fatalf("store insertBatch called %d times, want 1", fs.calls)
	}
	if fs.lastRows != 5 {
		t.Fatalf("store received %d rows, want 5", fs.lastRows)
	}
	var resp ingestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode success body: %v", err)
	}
	if resp.Accepted != 3 || resp.Duplicate != 2 || resp.Rejected != 0 {
		t.Errorf("counts = %+v, want accepted=3 duplicate=2 rejected=0", resp)
	}
}

// zeroAcceptStore records the call and always reports 0 accepted (every row was
// an idempotent duplicate). fakeStore conflates accepted==0 with "accept all", so
// this dedicated fake expresses the all-duplicate case cleanly.
type zeroAcceptStore struct {
	calls    int
	lastRows int
}

func (z *zeroAcceptStore) insertBatch(_ context.Context, rows []dbgen.InsertEventParams) (int64, error) {
	z.calls++
	z.lastRows = len(rows)
	return 0, nil
}

// TestIngest_AllDuplicate asserts a fully re-sent batch (store accepts 0) reports
// duplicate = batch size, accepted = 0, rejected = 0 — the hold-and-retry happy
// path (AC3 surfaces this at the count layer; the DB-level rows=1 proof is in the
// integration test).
func TestIngest_AllDuplicate(t *testing.T) {
	zf := &zeroAcceptStore{}
	h := newTestHandlerWith(zf)

	rec := postBatch(t, h, nEvents(4))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody=%s", rec.Code, rec.Body.String())
	}
	if zf.calls != 1 || zf.lastRows != 4 {
		t.Fatalf("store calls=%d rows=%d, want calls=1 rows=4", zf.calls, zf.lastRows)
	}
	var resp ingestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode success body: %v", err)
	}
	if resp.Accepted != 0 || resp.Duplicate != 4 || resp.Rejected != 0 {
		t.Errorf("counts = %+v, want accepted=0 duplicate=4 rejected=0", resp)
	}
}

// TestIngest_EmptyBatchRejected asserts an empty array is a 400 (batch schema
// minItems:1) with zero store writes — the empty-batch state from the PRD flow
// table resolved to 400 (the schema bites it before persistence).
func TestIngest_EmptyBatchRejected(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	rec := postBatch(t, h, []any{})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty batch)\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codeBadRequest)
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on an empty batch, want 0", fs.calls)
	}
}

// TestIngest_NonArrayBodyRejected asserts a JSON object (not an array) is a 400:
// the batch schema requires a top-level array, so a single object body is not an
// acceptable batch (zero writes).
func TestIngest_NonArrayBodyRejected(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	rec := postBatch(t, h, validEvent()) // a single object, not wrapped in an array

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (non-array body)\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codeBadRequest)
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on a non-array body, want 0", fs.calls)
	}
}

// TestIngest_PartialRejectionCountsInMessage asserts that when a 100-event batch
// carries exactly 1 schema-invalid event, the whole batch is rejected 4xx with
// zero store writes and the rejected count appears in the error message (AC4/D8 —
// the count is diagnostic, reported in the message text, never as an envelope
// field). This is the count-layer companion to the conformance negative cases.
func TestIngest_PartialRejectionCountsInMessage(t *testing.T) {
	fs := &fakeStore{}
	h := newTestHandler(fs)

	batch := nEvents(100)
	bad := validEvent()
	bad["kind"] = "metric" // out-of-enum -> this one event is invalid
	batch[42] = bad

	rec := postBatch(t, h, batch)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codeBadRequest)
	if fs.calls != 0 {
		t.Fatalf("store insertBatch called %d times on a batch with an invalid event, want 0 (zero-write)", fs.calls)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	wantFragment := "1 of 100 events rejected"
	if !strings.Contains(env.Error.Message, wantFragment) {
		t.Errorf("error.message = %q, want it to contain %q", env.Error.Message, wantFragment)
	}
}

// TestIngest_LexicalTimestampRejected drives single-event batches whose
// timestampMs is written in a NON-integer JSON lexical form (1748563200123.0,
// 1.748563200123e12) or exceeds int64 (99999999999999999999). Each carries an
// integer VALUE that the schema's "type":"integer"/minimum/maximum would accept
// (the first two) or reject (the last, over maximum), but none decodes into the
// int64 timestampMs field. Whichever gate bites — the value-semantics schema or
// the lexical-form decode — the result MUST be 4xx (the client drops the batch)
// with ZERO store writes, never a 500 that would make the client hold-and-retry a
// poison batch forever (the Major gap this fix closes).
//
// The body is hand-built JSON: marshaling a Go struct would normalize 1.0 to 1
// and drop the lexical form under test, so the literal must be written verbatim.
func TestIngest_LexicalTimestampRejected(t *testing.T) {
	const eventPre = `[{"eventId":"evt-lex","kind":"log","traceId":"trace-x","timestampMs":`
	const eventPost = `,"source":"client-ios","name":"app.launch","attributes":{"ok":true}}]`

	cases := map[string]string{
		"trailing .0 (1748563200123.0)":           "1748563200123.0",
		"scientific notation (1.748563200123e12)": "1.748563200123e12",
		"over int64 (99999999999999999999)":       "99999999999999999999",
	}

	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{}
			h := newTestHandler(fs)

			body := eventPre + ts + eventPost
			rec := postRaw(t, h, []byte(body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (lexical/over-range timestamp must be 4xx, not 5xx)\nbody=%s", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec, codeBadRequest)
			if fs.calls != 0 {
				t.Fatalf("store insertBatch called %d times on a rejected batch, want 0 (zero-write)", fs.calls)
			}
		})
	}
}

// TestIngest_StoreFailureIs500 asserts a store/DB error becomes a 500 with the
// internal code — so the client (hold-and-retry) treats it as a transient failure
// and re-sends the whole batch (5xx = retry; 4xx = drop). The error envelope
// shape is preserved.
func TestIngest_StoreFailureIs500(t *testing.T) {
	fs := &fakeStore{returnErr: errors.New("boom: simulated store failure")}
	h := newTestHandler(fs)

	rec := postBatch(t, h, nEvents(2))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (store failure must be 5xx for client retry)\nbody=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, codeInternal)
	if fs.calls != 1 {
		t.Fatalf("store insertBatch called %d times, want 1 (the error came from the store)", fs.calls)
	}
}
