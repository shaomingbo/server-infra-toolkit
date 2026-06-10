package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// TestNewAccessToken_ShapeAndHash asserts a minted access token decodes to at
// least 32 raw bytes (FR3 ≥256-bit entropy) and that the stored hash is exactly
// SHA-256 of those raw bytes — the value Bearer verification will look up.
func TestNewAccessToken_ShapeAndHash(t *testing.T) {
	tok, err := newAccessToken()
	if err != nil {
		t.Fatalf("newAccessToken: %v", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(tok.plaintext)
	if err != nil {
		t.Fatalf("access token is not valid base64url-no-padding: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("access token decodes to %d bytes, want >= 32", len(raw))
	}
	// The plaintext must be URL-safe with no padding: no '+', '/', or '='.
	if strings.ContainsAny(tok.plaintext, "+/=") {
		t.Fatalf("access token %q contains non-URL-safe or padding chars", tok.plaintext)
	}

	want := sha256.Sum256(raw)
	if string(tok.hash) != string(want[:]) {
		t.Fatal("stored hash is not SHA-256 of the raw token bytes")
	}
}

// TestNewAccessToken_Unique asserts two mints differ (random, not fixed).
func TestNewAccessToken_Unique(t *testing.T) {
	a, err := newAccessToken()
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	b, err := newAccessToken()
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}
	if a.plaintext == b.plaintext {
		t.Fatal("two access tokens are identical (not random)")
	}
}

// TestNewRefreshToken_SplitShape asserts the split-token format: the plaintext is
// "selector.verifier", the selector half equals the stored selector, both halves
// are URL-safe base64, and the stored verifier hash is SHA-256 of the verifier's
// RAW bytes (not the encoded string).
func TestNewRefreshToken_SplitShape(t *testing.T) {
	tok, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken: %v", err)
	}

	sel, ver, found := strings.Cut(tok.plaintext, ".")
	if !found {
		t.Fatalf("refresh token %q is not selector.verifier", tok.plaintext)
	}
	if sel == "" || ver == "" {
		t.Fatal("refresh token has an empty selector or verifier half")
	}
	if sel != tok.selector {
		t.Fatalf("plaintext selector %q != stored selector %q", sel, tok.selector)
	}
	if strings.ContainsAny(tok.plaintext, "+/=") {
		t.Fatalf("refresh token %q contains non-URL-safe or padding chars", tok.plaintext)
	}

	verRaw, err := base64.RawURLEncoding.DecodeString(ver)
	if err != nil {
		t.Fatalf("verifier is not valid base64url: %v", err)
	}
	want := sha256.Sum256(verRaw)
	if string(tok.verifierHash) != string(want[:]) {
		t.Fatal("stored verifier hash is not SHA-256 of the raw verifier bytes")
	}
}

// TestNewRefreshToken_Unique asserts selector and verifier are independently
// random across mints (no collision, no fixed half).
func TestNewRefreshToken_Unique(t *testing.T) {
	a, err := newRefreshToken()
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	b, err := newRefreshToken()
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}
	if a.selector == b.selector {
		t.Fatal("two refresh selectors collide (not random)")
	}
	if a.plaintext == b.plaintext {
		t.Fatal("two refresh tokens are identical (not random)")
	}
}

// TestNewTokenFamily_V4 asserts the generated token family is a valid, unique,
// RFC 4122 version-4 UUID (correct version and variant bits, Valid set).
func TestNewTokenFamily_V4(t *testing.T) {
	u, err := newTokenFamily()
	if err != nil {
		t.Fatalf("newTokenFamily: %v", err)
	}
	if !u.Valid {
		t.Fatal("token family is not Valid")
	}
	if v := u.Bytes[6] >> 4; v != 0x4 {
		t.Fatalf("UUID version nibble = %x, want 4", v)
	}
	if variant := u.Bytes[8] >> 6; variant != 0x2 {
		t.Fatalf("UUID variant bits = %b, want 10", variant)
	}

	other, err := newTokenFamily()
	if err != nil {
		t.Fatalf("newTokenFamily 2: %v", err)
	}
	if u.Bytes == other.Bytes {
		t.Fatal("two token families are identical (not random)")
	}
}
