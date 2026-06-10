package auth

// credential_test.go pins the password-redaction invariants (FR7/FR9/AC10): the
// credential type redacts on the fmt (String) and slog (LogValue) paths, a
// loginRequest carrying a known plaintext password never leaks it through slog.Any
// (the panic-audit channel platformlog.Panic uses), and — critically — switching
// the password field to credential does NOT break strict JSON parsing (R5).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// knownSecret is a distinctive plaintext we grep for; if it appears in any log or
// formatted output, redaction failed.
const knownSecret = "hunter2-PLAINTEXT-leak-canary"

// TestCredential_StringRedacts asserts fmt verbs that use Stringer render the
// marker, not the plaintext.
func TestCredential_StringRedacts(t *testing.T) {
	c := credential(knownSecret)

	if got := c.String(); got != redactedMarker {
		t.Fatalf("String() = %q, want %q", got, redactedMarker)
	}
	for _, verb := range []string{"%v", "%s"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, knownSecret) {
			t.Fatalf("fmt %q leaked plaintext: %s", verb, out)
		}
		if !strings.Contains(out, redactedMarker) {
			t.Fatalf("fmt %q did not redact: %s", verb, out)
		}
	}
}

// TestCredential_LogValueRedacts asserts the slog path redacts, both when the
// credential is logged directly and when it is reached reflectively as a struct
// field via slog.Any — the latter is exactly how platformlog.Panic emits a
// recovered loginRequest (FR9/AC10).
func TestCredential_LogValueRedacts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// Direct slog.Any of the credential.
	logger.Info("direct", slog.Any("password", credential(knownSecret)))

	// Reflective: a whole loginRequest carried as one slog.Any value — the shape
	// platformlog.Panic('panic', value) produces when value is a recovered request.
	req := loginRequest{Username: "audit-user", Password: credential(knownSecret)}
	logger.Error("panic recovered", slog.Any("panic", req))

	out := buf.String()
	if strings.Contains(out, knownSecret) {
		t.Fatalf("slog output leaked plaintext password:\n%s", out)
	}
	if !strings.Contains(out, redactedMarker) {
		t.Fatalf("slog output did not contain the redaction marker:\n%s", out)
	}
}

// TestCredential_StrictParsingPreserved (R5) asserts the credential field unmarshals
// from a JSON string with the DEFAULT behavior and does NOT break the strict-parse
// path: DisallowUnknownFields still rejects extra fields, a non-string password is
// still a type error, and a valid password decodes to its exact plaintext (so
// argon2 verification still sees the real value).
func TestCredential_StrictParsingPreserved(t *testing.T) {
	t.Run("valid decodes to plaintext", func(t *testing.T) {
		var req loginRequest
		if err := json.Unmarshal([]byte(`{"username":"u","password":"`+knownSecret+`"}`), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(req.Password) != knownSecret {
			t.Fatalf("decoded password = %q, want the exact plaintext (argon2 needs it)", string(req.Password))
		}
	})

	t.Run("unknown field still rejected", func(t *testing.T) {
		dec := json.NewDecoder(strings.NewReader(`{"username":"u","password":"p","extra":true}`))
		dec.DisallowUnknownFields()
		var req loginRequest
		if err := dec.Decode(&req); err == nil {
			t.Fatal("DisallowUnknownFields did not reject an unknown field (credential broke strict parsing)")
		}
	})

	t.Run("wrong type still rejected", func(t *testing.T) {
		var req loginRequest
		if err := json.Unmarshal([]byte(`{"username":"u","password":123}`), &req); err == nil {
			t.Fatal("a numeric password did not error (credential broke type checking)")
		}
	})
}

// TestLogin_ResponseAndLogNoPlaintext (AC10) asserts that a login carrying a known
// plaintext password — whether it 401s (wrong password) or 200s (success) — never
// echoes the plaintext into the response body. (The slog redaction is covered
// above; the handler never logs the body, and writeError never formats the
// credential.)
func TestLogin_ResponseAndLogNoPlaintext(t *testing.T) {
	const username = "Mona"
	s := storeOf(username, testUser(t, username, knownSecret))
	h := newTestHandler(s)

	// 200 path (correct password).
	okRec := doLogin(h, `{"username":"Mona","password":"`+knownSecret+`"}`)
	if okRec.Code != 200 {
		t.Fatalf("success status = %d, want 200; body=%s", okRec.Code, okRec.Body.String())
	}
	if strings.Contains(okRec.Body.String(), knownSecret) {
		t.Fatalf("200 response echoed the plaintext password: %s", okRec.Body.String())
	}

	// 401 path (wrong password) — the wrong value is the canary here.
	failRec := doLogin(h, `{"username":"Mona","password":"`+knownSecret+`-WRONG"}`)
	if failRec.Code != 401 {
		t.Fatalf("failure status = %d, want 401", failRec.Code)
	}
	if strings.Contains(failRec.Body.String(), knownSecret) {
		t.Fatalf("401 response echoed the password attempt: %s", failRec.Body.String())
	}
}
