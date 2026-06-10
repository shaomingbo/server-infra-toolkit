package auth

// credential.go defines the password redaction type (FR7 / D7). The login
// request's password field is this type rather than a bare string so that ANY
// reflective or string formatting of the decoded request — most importantly
// slog.Any over a recovered panic value (FR9) — renders "[REDACTED]" instead of
// the plaintext.
//
// WHY A LOCAL string-alias TYPE (and NOT config.Secret):
//   - config.Secret lives in internal/platform/config and is the DSN's wrapper;
//     reusing it here would make platform/config carry business-credential
//     semantics it has no reason to own (D7). It also has NO UnmarshalJSON, so
//     decoding into it would not round-trip a JSON string field cleanly.
//   - credential is a `type credential string` alias. A string alias unmarshals
//     from a JSON string with the DEFAULT encoding/json behavior — we deliberately
//     do NOT implement UnmarshalJSON — so json.Decoder.DisallowUnknownFields and
//     the rest of the strict-parse path (NFR8) behave EXACTLY as they did when the
//     field was a plain string. The redaction is a property of how the value is
//     LOGGED/STRINGIFIED, not of how it is parsed.
//
// REDACTION PATHS — String, LogValue, AND MarshalJSON (a deliberate, fact-based
// deviation from PRD D7, which said to OMIT MarshalJSON):
//   - String covers fmt verbs (%v/%s).
//   - LogValue covers slog when the credential is logged as its OWN attr
//     (slog.Any("password", cred)).
//   - MarshalJSON is REQUIRED for the FR9/AC10 panic-audit case. The realistic
//     leak is platformlog.Panic doing slog.Any("panic", recoveredValue) where the
//     recovered value embeds a loginRequest. slog's JSON handler encodes a struct
//     value reflectively and DOES NOT invoke LogValuer on nested struct fields — it
//     DOES honor json.Marshaler. Verified empirically: with only LogValue, a
//     loginRequest carried through slog.Any leaks the plaintext password; adding
//     MarshalLJSON redacts it. So LogValue alone does NOT satisfy FR9; MarshalJSON
//     is the mechanism that does.
//   - This MarshalJSON is SAFE for the frozen wire (D8/D13): a login RESPONSE is a
//     loginSession (plain-string tokens), never a loginRequest, so the request's
//     password type is never serialized onto the response path. Redacting its
//     MarshalJSON therefore cannot change any client-visible payload — the success
//     response still carries plaintext tokens, asserted by TestLogin_Success.

import (
	"encoding/json"
	"log/slog"
)

// redactedMarker is the single placeholder every redaction path emits in place of
// a credential's plaintext. Pinned as a constant so String() and LogValue() can
// never drift.
const redactedMarker = "[REDACTED]"

// credential is a password (or any in-memory plaintext credential) that redacts
// itself when logged or stringified. It is a `string` alias so JSON decoding into
// it uses the default string behavior (no custom UnmarshalJSON) and the strict
// login parse path is unaffected (FR7/D7/R5). To read the plaintext for the one
// legitimate use — argon2 verification — convert it back with string(c).
type credential string

// String redacts the plaintext for any fmt verb that uses Stringer (%v, %s) and
// for any code that calls .String() directly. The plaintext never appears.
//
// credential satisfies fmt.Stringer, so fmt-based formatting of a struct holding
// a credential field prints the marker. (slog uses LogValue below, not String,
// for the structured path.)
func (c credential) String() string {
	return redactedMarker
}

// LogValue is what closes the panic-audit leak (FR9): slog resolves a value that
// implements slog.LogValuer by calling LogValue, EVEN when the value is reached
// reflectively through slog.Any over a struct (e.g. platformlog.Panic emitting a
// recovered loginRequest). Returning a redacted string here means the plaintext
// password can never reach the structured log, regardless of how the request
// struct is handed to slog.
func (c credential) LogValue() slog.Value {
	return slog.StringValue(redactedMarker)
}

// MarshalJSON redacts on the encoding/json path. This is the path slog's JSON
// handler uses to render a struct reached reflectively via slog.Any, so it is what
// actually closes the panic-audit leak for a recovered loginRequest (FR9/AC10) —
// LogValue is not consulted for nested struct fields there. It is independent of
// the response wire: loginRequest is never serialized onto a login response (the
// response is loginSession), so this redaction is invisible to clients (D8/D13).
func (c credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(redactedMarker)
}

// Compile-time assertions that credential redacts on the slog and json paths.
var (
	_ slog.LogValuer = credential("")
	_ json.Marshaler = credential("")
)
