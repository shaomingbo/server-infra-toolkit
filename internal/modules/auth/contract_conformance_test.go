package auth

// contract_conformance_test.go is the server-side half of the T3 contract
// reconciliation: it pins the login/refresh success wire (the loginSession
// struct, serialized exactly as the handlers serialize it) against the
// machine-readable JSON Schemas in contract/. If a future change renames a
// field, drops it, changes its JSON type, or adds an unexpected field, the
// schema validation here fails under `go test`, so wire drift is caught in CI
// before it can reach the frozen client contract (T2 D13/NFR7).
//
// Two invariants beyond the schema, asserted explicitly:
//   - expiresAt is a Unix MILLISECOND integer (the value-level discriminator):
//     the wire carries the exact fixed ms literal, decoded as an integer number
//     (never a string, never float-rounded), so a ms↔s unit regression that
//     produced a 1000x-different value would fail the equality assertion. (The
//     source-level unit guard lives in expires_unit_guard_test.go.)
//   - the schema's tightening keywords actually bite — five negative cases feed
//     malformed objects through the SAME compiled schema and assert validation
//     fails, so a too-loose schema cannot pass silently: an EXTRA field proves
//     additionalProperties:false bites, a MISSING expiresAt proves required bites,
//     a STRING expiresAt proves type:integer bites, a non-UUID userId proves
//     format:uuid bites (under AssertFormat), and a Unix-SECONDS expiresAt proves
//     minimum (the ms floor) bites. Without these, relaxing a keyword in the
//     schema would leave the positive test still green.
//
// Determinism: every input is a fixed literal (token strings, UUID, expiresAt);
// no CSPRNG, no time.Now. The schemas are read from contract/ relative to the
// package dir (the test cwd), and a missing schema is a t.Fatal — deleting a
// schema file fails the build, it never skips (fail-closed).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Fixed, deterministic wire inputs shared by both conformance tests. The
// expiresAt is a non-trivial Unix MILLISECONDS value whose seconds truncation
// (1748563200123 ms -> 1748563200 s) is obviously different, and whose last
// three digits are non-zero so a ms-vs-s confusion or a /1000 truncation would
// change the value: picking a ms value that is NOT an exact multiple of 1000
// makes the unit/precision assertion bite.
const (
	fixedUserID       = "11111111-2222-4333-8444-555555555555"
	fixedAccessToken  = "fixed-access-token-AAAAAAAAAAAAAAAAAAAAAAAA"
	fixedRefreshToken = "fixed-refresh-token-BBBBBBBBBBBBBBBBBBBBBBBB"
	fixedExpiresAtMs  = int64(1748563200123)
)

// fixedSession builds the deterministic success response value. It is the SAME
// loginSession struct the login and refresh handlers return, so validating it
// validates the real wire shape — login.go and refresh.go both encode this exact
// type via json.NewEncoder(w).Encode(session).
func fixedSession() loginSession {
	return loginSession{
		UserID:       fixedUserID,
		AccessToken:  fixedAccessToken,
		RefreshToken: fixedRefreshToken,
		ExpiresAt:    fixedExpiresAtMs,
	}
}

// encodeWire serializes a value with the SAME encoder the handlers use
// (json.NewEncoder(...).Encode, NOT json.Marshal): Encode appends a trailing
// newline and HTML-escapes by default, so this reproduces the exact bytes that
// go on the wire — including the trailing '\n'.
func encodeWire(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatalf("encode wire: %v", err)
	}
	return buf.Bytes()
}

// compileSchema reads a schema from contract/<name> off disk and compiles it
// with jsonschema/v6. A missing or unreadable schema is a fatal failure (never a
// skip): the contract files are load-bearing, so deleting one must turn the
// build red. The schema is decoded with the library's UnmarshalJSON helper,
// which preserves number precision (json.Number) so large integer bounds are
// never rounded through float64.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("contract", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open schema %s: %v (contract schema is required; this test fails closed if it is missing)", path, err)
	}
	defer f.Close()

	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("decode schema %s: %v", path, err)
	}

	c := jsonschema.NewCompiler()
	// Turn "format" from an annotation into an assertion: jsonschema/v6 treats
	// format as annotation-only by default (collect, don't enforce), so without
	// this the schema's format:"uuid" would never reject a malformed userId. The
	// negative case below proves this bites.
	c.AssertFormat()
	// Register the schema under its file path and compile by the same URL.
	if err := c.AddResource(path, doc); err != nil {
		t.Fatalf("add schema resource %s: %v", path, err)
	}
	sch, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile schema %s: %v", path, err)
	}
	return sch
}

// decodeWire parses serialized JSON bytes into a Go value using UseNumber so an
// int64 expiresAt survives as json.Number (never coerced to float64 and rounded).
// The same json.Number form is what jsonschema/v6 needs to apply "type":"integer"
// without precision loss, so this single decode feeds both the schema validation
// and the value-level assertions.
func decodeWire(t *testing.T, wire []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wire))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	return m
}

// assertSessionValues is the value-level discriminator that sits beside the
// schema: it asserts the decoded wire carries the exact fixed literals, and that
// expiresAt is an integer json.Number equal to the fixed ms value — proving the
// timestamp is a Unix millisecond integer, not a seconds value, a float, or a
// string. A ms↔s unit regression (off by 1000x) would fail the equality check.
func assertSessionValues(t *testing.T, m map[string]any) {
	t.Helper()

	if got, ok := m["userId"].(string); !ok || got != fixedUserID {
		t.Errorf("userId = %v (%T), want string %q", m["userId"], m["userId"], fixedUserID)
	}
	if got, ok := m["accessToken"].(string); !ok || got != fixedAccessToken {
		t.Errorf("accessToken = %v (%T), want string %q", m["accessToken"], m["accessToken"], fixedAccessToken)
	}
	if got, ok := m["refreshToken"].(string); !ok || got != fixedRefreshToken {
		t.Errorf("refreshToken = %v (%T), want string %q", m["refreshToken"], m["refreshToken"], fixedRefreshToken)
	}

	// expiresAt must be a JSON number (json.Number under UseNumber), NEVER a
	// string, and must equal the fixed ms literal exactly as an int64.
	num, ok := m["expiresAt"].(json.Number)
	if !ok {
		t.Fatalf("expiresAt = %v (%T), want a JSON number (json.Number) — a string or float would mean the wire type drifted", m["expiresAt"], m["expiresAt"])
	}
	got, err := num.Int64()
	if err != nil {
		t.Fatalf("expiresAt %q is not an integer: %v — expiresAt must be a Unix millisecond integer", num, err)
	}
	if got != fixedExpiresAtMs {
		t.Errorf("expiresAt = %d, want %d (Unix milliseconds); a 1000x difference signals a ms↔s unit regression", got, fixedExpiresAtMs)
	}
}

// TestLoginConformance validates the login success wire against
// contract/login.schema.json. It loads ONLY the login schema, so a change to the
// refresh schema cannot make this test fail (independent anchoring).
func TestLoginConformance(t *testing.T) {
	sch := compileSchema(t, "login.schema.json")
	wire := encodeWire(t, fixedSession())

	// Schema validation: decode with the library's precision-preserving helper,
	// then Validate the decoded value.
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("decode login wire for schema validation: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("login wire failed schema validation: %v", err)
	}

	// Value-level discriminator beyond the schema.
	assertSessionValues(t, decodeWire(t, wire))

	// Negative case: an object with an EXTRA field must be rejected, proving the
	// schema's additionalProperties:false actually bites (guards against a schema
	// that was accidentally written too loose).
	extra := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    fixedExpiresAtMs,
		"unexpected":   "should-be-rejected",
	}
	extraWire := encodeWire(t, extra)
	extraInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(extraWire))
	if err != nil {
		t.Fatalf("decode extra-field instance: %v", err)
	}
	if err := sch.Validate(extraInst); err == nil {
		t.Fatal("login schema accepted an object with an extra field — additionalProperties:false is not enforcing")
	}

	// Negative case: an object MISSING the required expiresAt must be rejected,
	// proving the schema's required list actually bites. If someone drops
	// expiresAt from required, the schema went too loose and this fails closed.
	missing := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
	}
	missingWire := encodeWire(t, missing)
	missingInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(missingWire))
	if err != nil {
		t.Fatalf("decode missing-field instance: %v", err)
	}
	if err := sch.Validate(missingInst); err == nil {
		t.Fatal("login schema accepted an object missing expiresAt — required is not enforcing (schema written too loose)")
	}

	// Negative case: a STRING expiresAt must be rejected, proving the schema's
	// type:integer actually bites. If someone relaxes expiresAt's type, a client
	// reading it as a number would break and this fails closed.
	typed := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    "1748563200123",
	}
	typedWire := encodeWire(t, typed)
	typedInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(typedWire))
	if err != nil {
		t.Fatalf("decode typed-field instance: %v", err)
	}
	if err := sch.Validate(typedInst); err == nil {
		t.Fatal("login schema accepted a string expiresAt — type:integer is not enforcing (schema written too loose)")
	}

	// Negative case: a userId that is NOT a UUID must be rejected, proving the
	// schema's format:"uuid" actually bites under AssertFormat. If format were
	// left annotation-only (or the keyword dropped), this would pass and a client
	// pinning a UUID-shaped id would silently accept garbage.
	badUUID := map[string]any{
		"userId":       "not-a-uuid",
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    fixedExpiresAtMs,
	}
	badUUIDWire := encodeWire(t, badUUID)
	badUUIDInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(badUUIDWire))
	if err != nil {
		t.Fatalf("decode bad-uuid instance: %v", err)
	}
	if err := sch.Validate(badUUIDInst); err == nil {
		t.Fatal("login schema accepted a non-UUID userId — format:uuid is not enforcing (AssertFormat missing or schema too loose)")
	}

	// Negative case: an expiresAt that is a Unix SECONDS value (1748563200, the
	// seconds truncation of the fixed ms literal) must be rejected, proving the
	// schema's minimum:1000000000000 actually bites. A seconds value is ~1000x
	// too small, so a ms↔s unit regression on the wire would fail this floor and
	// be caught at the schema, not just the value-level assertion.
	seconds := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    int64(1748563200),
	}
	secondsWire := encodeWire(t, seconds)
	secondsInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(secondsWire))
	if err != nil {
		t.Fatalf("decode seconds-value instance: %v", err)
	}
	if err := sch.Validate(secondsInst); err == nil {
		t.Fatal("login schema accepted a seconds-value expiresAt — minimum:1000000000000 is not enforcing (schema too loose; a ms↔s regression would slip)")
	}
}

// TestRefreshConformance validates the refresh success wire against
// contract/refresh.schema.json. It loads ONLY the refresh schema, so a change to
// the login schema cannot make this test fail (independent anchoring). The
// refresh handler returns the same loginSession struct, so the same fixed value
// exercises its wire.
func TestRefreshConformance(t *testing.T) {
	sch := compileSchema(t, "refresh.schema.json")
	wire := encodeWire(t, fixedSession())

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("decode refresh wire for schema validation: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("refresh wire failed schema validation: %v", err)
	}

	assertSessionValues(t, decodeWire(t, wire))

	extra := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    fixedExpiresAtMs,
		"unexpected":   "should-be-rejected",
	}
	extraWire := encodeWire(t, extra)
	extraInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(extraWire))
	if err != nil {
		t.Fatalf("decode extra-field instance: %v", err)
	}
	if err := sch.Validate(extraInst); err == nil {
		t.Fatal("refresh schema accepted an object with an extra field — additionalProperties:false is not enforcing")
	}

	// Negative case: an object MISSING the required expiresAt must be rejected,
	// proving the schema's required list actually bites. If someone drops
	// expiresAt from required, the schema went too loose and this fails closed.
	missing := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
	}
	missingWire := encodeWire(t, missing)
	missingInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(missingWire))
	if err != nil {
		t.Fatalf("decode missing-field instance: %v", err)
	}
	if err := sch.Validate(missingInst); err == nil {
		t.Fatal("refresh schema accepted an object missing expiresAt — required is not enforcing (schema written too loose)")
	}

	// Negative case: a STRING expiresAt must be rejected, proving the schema's
	// type:integer actually bites. If someone relaxes expiresAt's type, a client
	// reading it as a number would break and this fails closed.
	typed := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    "1748563200123",
	}
	typedWire := encodeWire(t, typed)
	typedInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(typedWire))
	if err != nil {
		t.Fatalf("decode typed-field instance: %v", err)
	}
	if err := sch.Validate(typedInst); err == nil {
		t.Fatal("refresh schema accepted a string expiresAt — type:integer is not enforcing (schema written too loose)")
	}

	// Negative case: a userId that is NOT a UUID must be rejected, proving the
	// schema's format:"uuid" actually bites under AssertFormat. If format were
	// left annotation-only (or the keyword dropped), this would pass and a client
	// pinning a UUID-shaped id would silently accept garbage.
	badUUID := map[string]any{
		"userId":       "not-a-uuid",
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    fixedExpiresAtMs,
	}
	badUUIDWire := encodeWire(t, badUUID)
	badUUIDInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(badUUIDWire))
	if err != nil {
		t.Fatalf("decode bad-uuid instance: %v", err)
	}
	if err := sch.Validate(badUUIDInst); err == nil {
		t.Fatal("refresh schema accepted a non-UUID userId — format:uuid is not enforcing (AssertFormat missing or schema too loose)")
	}

	// Negative case: an expiresAt that is a Unix SECONDS value (1748563200, the
	// seconds truncation of the fixed ms literal) must be rejected, proving the
	// schema's minimum:1000000000000 actually bites. A seconds value is ~1000x
	// too small, so a ms↔s unit regression on the wire would fail this floor and
	// be caught at the schema, not just the value-level assertion.
	seconds := map[string]any{
		"userId":       fixedUserID,
		"accessToken":  fixedAccessToken,
		"refreshToken": fixedRefreshToken,
		"expiresAt":    int64(1748563200),
	}
	secondsWire := encodeWire(t, seconds)
	secondsInst, err := jsonschema.UnmarshalJSON(bytes.NewReader(secondsWire))
	if err != nil {
		t.Fatalf("decode seconds-value instance: %v", err)
	}
	if err := sch.Validate(secondsInst); err == nil {
		t.Fatal("refresh schema accepted a seconds-value expiresAt — minimum:1000000000000 is not enforcing (schema too loose; a ms↔s regression would slip)")
	}
}
