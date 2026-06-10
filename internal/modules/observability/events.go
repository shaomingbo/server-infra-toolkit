package observability

// events.go is the ingest flow for POST /v1/events: decode the bare JSON array,
// validate the batch shape and every event against the embedded inbound schema,
// reject the WHOLE batch with zero writes if ANY event is invalid (FR10/AC4),
// then land the valid batch idempotently in one transaction and return the
// accepted/duplicate/rejected counts (D8).
//
// The schema is the validation gate (additionalProperties:false / required /
// enum / type all bite there), so the Go decode into typed structs happens ONLY
// after the schema passes — it is just value extraction for persistence, never
// the validator.

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/santhosh-tekuri/jsonschema/v6"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// Request hard limits (FR5/AC5/AC6) — the billing ceiling, independent of
// max-instances. These cap cost at the DB/request side, not via the rate-limit
// seam (which is not a safety boundary, see ratelimit.go).
//
//   - maxRequestBytes caps the request body via http.MaxBytesReader so a giant
//     body is rejected as it is read (the buffer never grows past the cap and
//     process memory is not blown), returning 413 payload_too_large.
//   - maxBatchSize caps the per-batch event count. It mirrors the batch schema's
//     maxItems (double-bite): the schema is the correctness gate, this is the
//     early/explicit-reject path that returns 413 payload_too_large with the same
//     code as the body cap (both are "you sent too much").
//
// Per-field max lengths (source/name/traceId/eventId/attribute keys+string
// values) are enforced by the inbound JSON Schema (contract/event.schema.json),
// not here — the schema is the single place those bounds live.
const (
	maxRequestBytes = 1 << 20 // 1 MiB
	maxBatchSize    = 500     // mirrors batch.schema.json maxItems
)

// moduleLogger emits the module's own telemetry as JSON to stdout (FR12/AC13),
// the same sink as the platform access log. The receive statistics go HERE, never
// into the events table — writing self-telemetry back into events would create a
// recursion/amplification loop the PRD forbids.
var moduleLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// ingestResponse is the success wire (D9, UNSTABLE — not frozen). rejected is
// always 0 on a 2xx: a batch with any rejected event returns 4xx with zero
// writes (FR10), so a 200 means every event was valid and either newly inserted
// (accepted) or an idempotent duplicate (duplicate).
type ingestResponse struct {
	Accepted  int    `json:"accepted"`
	Duplicate int    `json:"duplicate"`
	Rejected  int    `json:"rejected"`
	RequestID string `json:"requestId"`
}

// inboundEvent is the typed view used ONLY to extract values for persistence
// AFTER the schema has accepted the event. The schema (not this struct) is the
// validation gate, so this does not need DisallowUnknownFields — an extra field
// is already rejected upstream by additionalProperties:false.
//
// The schema validates the VALUE semantics ("type":"integer", minimum/maximum),
// not the JSON LEXICAL form: a body like `1.0` or `1.7e12` is the integer value
// 1 / 1.7e12 to the validator and passes, yet does NOT decode into int64
// TimestampMs below. So this decode CAN still fail on input that cleared the
// schema — it is an external-input problem, not an internal inconsistency, and is
// mapped to 4xx (the client must drop, not retry, such a batch).
type inboundEvent struct {
	EventID     string                     `json:"eventId"`
	Kind        string                     `json:"kind"`
	TraceID     string                     `json:"traceId"`
	TimestampMs int64                      `json:"timestampMs"`
	Source      string                     `json:"source"`
	Name        string                     `json:"name"`
	Attributes  map[string]json.RawMessage `json:"attributes"`
}

// ingest handles POST /v1/events. The numbered steps mirror the PRD ingest flow
// (§7): rate-limit seam -> body cap -> decode -> count cap -> batch+per-event
// schema validation -> single-transaction idempotent insert -> count response.
func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	requestID := w.Header().Get(requestIDHeader)

	// (0) RateLimiter facade call site (FR7/D7). The default no-op always allows,
	// so this never blocks today; it is the single, unconditional seam where a
	// future limiter plugs in. It runs FIRST so a (future) limiter can shed load
	// before the body is read. The key is the request path (a future limiter could
	// bucket by source/IP instead). On denial we return 429 rate_limited with the
	// limiter's Retry-After hint — this is NOT a security boundary (see
	// ratelimit.go); the billing ceiling is the body/count caps below.
	if allowed, retryAfter := h.rateLimiter.Allow(r.Context(), r.URL.Path); !allowed {
		if retryAfter > 0 {
			// Round UP: Retry-After is whole seconds, and a sub-second hint must not
			// floor to 0 (which would tell the client to retry immediately).
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		}
		writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limited", requestID)
		return
	}

	// (1) Cap the request body (FR5/AC5) BEFORE reading it: http.MaxBytesReader
	// stops the read at maxRequestBytes, so a giant body is rejected as it streams
	// in and process memory is never blown. Exceeding the cap surfaces as a
	// *http.MaxBytesError from the read below, which we map to 413
	// payload_too_large; any other read error is a 400. The body is then decoded
	// with the library's precision-preserving helper so int64 timestampMs survives
	// as json.Number (never rounded through float64) for "type":"integer"
	// validation. A malformed (but in-size) body is a 400.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	raw, err := readBody(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body too large", requestID)
			return
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body", requestID)
		return
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body", requestID)
		return
	}

	// (1b) Count cap (FR5/AC6): if the body is an array, reject early when it
	// carries more than maxBatchSize events — a 413 payload_too_large, the same
	// "you sent too much" code as the body cap. This double-bites with the batch
	// schema's maxItems (the schema is the correctness gate; this is the explicit
	// early-reject path so an over-count batch is a 413, not a generic 400). A
	// non-array body falls through to the batch schema below, which rejects it.
	if arr, ok := inst.([]any); ok && len(arr) > maxBatchSize {
		writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "too many events in batch", requestID)
		return
	}

	// (2) Batch-shape validation: ARRAY-LEVEL invariants ONLY — a bare JSON array,
	// minItems 1 (empty batch is a 400), maxItems 500. The batch schema deliberately
	// does NOT validate per-event fields (no items->$ref): that is done element by
	// element in step (3) so an invalid event can be COUNTED rather than collapsing
	// the whole body into one array-level failure. If the batch schema fails here,
	// the body is structurally not an acceptable batch (not an array, empty, or too
	// many events).
	if err := batchSchema.Validate(inst); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid event batch", requestID)
		return
	}

	// inst is now known to be a non-empty array (batchSchema passed). Re-assert the
	// shape for the per-event pass; a non-array here would be a programming error
	// given the schema above, so a defensive 400 keeps the handler total.
	arr, ok := inst.([]any)
	if !ok || len(arr) == 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid event batch", requestID)
		return
	}

	// (3) Per-event validation to COUNT rejections: validate each element against
	// the event schema individually. Any single invalid event rejects the WHOLE
	// batch with zero writes (FR10/AC4); the rejected count goes in the error
	// message text (the error envelope is a frozen contract — no extra top-level
	// field; a 4xx means the client permanently drops the batch, so the count is
	// diagnostic only).
	rejected := 0
	for _, e := range arr {
		if err := eventSchema.Validate(e); err != nil {
			rejected++
		}
	}
	if rejected > 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			rejectionMessage(rejected, len(arr)), requestID)
		return
	}

	// (4) All events valid (by the schema's VALUE semantics): decode into typed
	// rows for persistence. The schema guaranteed every field's presence and
	// integer-value bounds, but NOT the JSON lexical form — a non-integer literal
	// (1.0, 1.7e12) carrying an integer value clears the schema yet fails to decode
	// into int64 here. That is still an EXTERNAL-input problem, so a decode error is
	// a 4xx bad_request (client drops the batch), NOT a 500 (which would make the
	// client hold-and-retry a poison batch forever).
	rows, err := buildRows(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid event values", requestID)
		return
	}

	// (5) Land the batch idempotently in one transaction. accepted = newly
	// inserted; duplicate = re-sent events skipped by ON CONFLICT (D5/E8). A DB
	// failure is a 500 so the client retries the whole batch (5xx = hold-and-retry).
	accepted, err := h.store.insertBatch(r.Context(), rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error", requestID)
		return
	}

	resp := ingestResponse{
		Accepted:  int(accepted),
		Duplicate: len(rows) - int(accepted),
		Rejected:  0,
		RequestID: requestID,
	}

	// (6) Self-telemetry (FR12/AC13): emit the per-batch receive statistics as one
	// JSON line to stdout, the SAME sink as the access log — NEVER back into the
	// events table (that would create a recursion/amplification loop). This runs on
	// the success path only; rejected batches already surfaced their count in the
	// 4xx error message, and we do not want self-telemetry on the abuse path.
	moduleLogger.Info("events_ingested",
		slog.String("request_id", requestID),
		slog.Int("batch_size", len(rows)),
		slog.Int("accepted", resp.Accepted),
		slog.Int("duplicate", resp.Duplicate),
		slog.Int("rejected", resp.Rejected),
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// readBody reads the whole request body. The caller wraps r.Body in
// http.MaxBytesReader before calling this, so a body over the cap surfaces here
// as a *http.MaxBytesError (which the caller maps to 413); the read stops at the
// cap and the buffer never grows past it.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rejectionMessage renders the rejected count into the error message text. The
// count is diagnostic (a 4xx already tells the client to drop the batch); it is
// reported in the message, NOT as a new envelope field (the envelope is frozen).
func rejectionMessage(rejected, total int) string {
	return "schema validation failed: " +
		strconv.Itoa(rejected) + " of " + strconv.Itoa(total) + " events rejected"
}

// buildRows decodes the already-schema-validated batch into InsertEventParams.
// Each event's timestampMs becomes event_ts via time.UnixMilli (lossless), and
// its attributes map is re-marshaled to the jsonb column bytes (defaulting to an
// empty object when the client omitted attributes — the client inits it empty).
//
// The schema validated the integer VALUE of timestampMs, not its JSON lexical
// form. A non-integer literal (1.0, 1.7e12) representing an integer value passes
// the schema but fails this decode into int64; that decode error is returned to
// the caller, which maps it to 4xx (external-input problem), not 500.
func buildRows(raw []byte) ([]dbgen.InsertEventParams, error) {
	var events []inboundEvent
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&events); err != nil {
		return nil, err
	}

	rows := make([]dbgen.InsertEventParams, 0, len(events))
	for i := range events {
		ev := events[i]

		attrs := []byte("{}")
		if ev.Attributes != nil {
			b, err := json.Marshal(ev.Attributes)
			if err != nil {
				return nil, err
			}
			attrs = b
		}

		rows = append(rows, dbgen.InsertEventParams{
			Source:     ev.Source,
			EventID:    ev.EventID,
			Kind:       ev.Kind,
			TraceID:    ev.TraceID,
			Name:       ev.Name,
			EventTs:    pgtype.Timestamptz{Time: time.UnixMilli(ev.TimestampMs).UTC(), Valid: true},
			Attributes: attrs,
		})
	}
	return rows, nil
}
