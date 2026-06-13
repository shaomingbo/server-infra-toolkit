package main

// signactive_test.go pins the offline-signing subcommand's two security/behaviour
// guards:
//
//   - AC11 (NFR2, redaction): the private key wrapped in config.Secret renders
//     [REDACTED] through String/MarshalJSON/LogValue, and the plaintext key —
//     neither its base64 form nor its raw decoded bytes — ever appears in the
//     active.json that -sign-active emits.
//   - AC12 (FR14, behaviour): a happy-path invocation with an Active keyId emits a
//     well-formed active.json whose signatureV2 verifies against the public key the
//     private key derives, with schemaVersion=1; and a non-Active (Minted) keyId is
//     refused fail-closed with no signatureV2.
//
// The subcommand path itself does no DB or network I/O (it never calls
// config.Load, so it has no NEON_DSN dependency); the test drives runSignActive
// directly, so AC12's "no port / no network / no DB" is also covered by code
// review of signactive.go.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"testing"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/offlinesig"
)

// signTestSeed is a deterministic 32-byte Ed25519 seed for the subcommand tests
// (exactly ed25519.SeedSize bytes). It is a throwaway test key, never real.
var signTestSeed = []byte("cmd-sign-active-test-seed-32byte")

// newSignActiveFlags builds a signActiveFlags bound to a throwaway FlagSet and
// sets the manifest inputs to the empty-tail golden values, so the test does not
// have to parse os.Args.
func newSignActiveFlags(t *testing.T) *signActiveFlags {
	t.Helper()
	f := registerSignActiveFlags(flag.NewFlagSet("test", flag.ContinueOnError))
	*f.version = "1.4.0"
	*f.digest = "sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49"
	*f.minAppVersion = "3.2.0"
	*f.url = "https://cdn.example.test/pkg-1.4.0.zip"
	*f.v1Signature = "base64:legacyV1Placeholder"
	*f.timestamp = 1700000000
	return f
}

// TestSecretRedaction asserts the config.Secret wrapper hides the private key in
// every serialization surface slog/fmt/json could reach (AC11 first half). This is
// the property the subcommand relies on to keep the key out of any log line.
func TestSecretRedaction(t *testing.T) {
	const plaintext = "super-secret-private-key-base64=="
	s := config.Secret(plaintext)

	if got := s.String(); got != "[REDACTED]" {
		t.Errorf("Secret.String() = %q, want [REDACTED]", got)
	}
	jsonBytes, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal(Secret): %v", err)
	}
	if got := string(jsonBytes); got != `"[REDACTED]"` || strings.Contains(got, plaintext) {
		t.Errorf("Secret MarshalJSON = %s, want \"[REDACTED]\"", got)
	}
	if got := s.LogValue().String(); got != "[REDACTED]" || strings.Contains(got, plaintext) {
		t.Errorf("Secret.LogValue() = %q, want [REDACTED]", got)
	}
	// slog over a struct field carrying the Secret must not reflect the plaintext.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("k", "key", s)
	if strings.Contains(buf.String(), plaintext) {
		t.Errorf("slog leaked the secret plaintext: %s", buf.String())
	}
}

// TestSignActive_NoKeyLeakInOutput drives the real subcommand with an Active keyId
// and asserts (AC11 second half) the plaintext private key never appears in the
// emitted active.json — not its base64 wire form and not its raw decoded bytes —
// while the signatureV2 is present and verifies against the derived public key.
func TestSignActive_NoKeyLeakInOutput(t *testing.T) {
	if len(signTestSeed) != ed25519.SeedSize {
		t.Fatalf("signTestSeed is %d bytes, want %d", len(signTestSeed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(signTestSeed)
	pub := priv.Public().(ed25519.PublicKey)

	// The env private key is the base64 of the seed (the form the operator injects).
	seedB64 := base64.StdEncoding.EncodeToString(signTestSeed)
	// The full 64-byte key's base64 is another representation that must also not leak.
	fullB64 := base64.StdEncoding.EncodeToString(priv)

	const keyID = "test-key-1"
	t.Setenv(envSignPrivateKey, seedB64)
	t.Setenv(envSignKeyID, keyID)
	t.Setenv(envSignActiveKeyIDs, keyID)
	// The published public key for this keyId is the one the private key derives:
	// the FR9 consistency check must pass and signing must succeed.
	t.Setenv(envSignExpectedPublicKey, base64.StdEncoding.EncodeToString(pub))

	var out bytes.Buffer
	if err := runSignActive(&out, newSignActiveFlags(t)); err != nil {
		t.Fatalf("runSignActive: %v", err)
	}

	got := out.String()
	// The private key material — in any of its serialized forms — must be absent.
	for _, secret := range []string{seedB64, fullB64} {
		if strings.Contains(got, secret) {
			t.Fatalf("active.json leaked private key material %q:\n%s", secret, got)
		}
	}
	// The raw decoded seed bytes (as a literal substring) must also be absent.
	if bytes.Contains(out.Bytes(), signTestSeed) {
		t.Fatalf("active.json leaked the raw seed bytes:\n%s", got)
	}

	// The emitted manifest must be well-formed and its signatureV2 must verify.
	var manifest offlinesig.ActiveManifest
	if err := json.Unmarshal(out.Bytes(), &manifest); err != nil {
		t.Fatalf("emitted active.json is not valid JSON: %v\n%s", err, got)
	}
	if manifest.SchemaVersion != offlinesig.SchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", manifest.SchemaVersion, offlinesig.SchemaVersion)
	}
	rawB64, ok := strings.CutPrefix(manifest.SignatureV2, "base64:")
	if !ok {
		t.Fatalf("signatureV2 missing base64: prefix: %q", manifest.SignatureV2)
	}
	sig, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		t.Fatalf("signatureV2 body not standard base64: %v", err)
	}
	payload, err := offlinesig.BuildCanonicalPayload(offlinesig.Fields{
		Version:       "1.4.0",
		Digest:        "sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49",
		MinAppVersion: "3.2.0",
	})
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("emitted signatureV2 does not verify against the derived public key")
	}
}

// TestSignActive_MintedKeyFailClosed asserts a keyId that is NOT in the Active
// allowlist (Minted) is refused fail-closed: runSignActive returns an error and
// writes no active.json (AC12 / FR8). The publish-order invariant is the reason —
// the server must not sign before the app ships the public key.
func TestSignActive_MintedKeyFailClosed(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(signTestSeed)
	pub := priv.Public().(ed25519.PublicKey)
	seedB64 := base64.StdEncoding.EncodeToString(signTestSeed)
	const keyID = "test-key-1"
	t.Setenv(envSignPrivateKey, seedB64)
	t.Setenv(envSignKeyID, keyID)
	// The published public key matches the private key, so the FR9 gate would pass;
	// the ONLY reason signing is refused here is the Minted status (keyId absent from
	// the active list), isolating this test to the publish-order invariant.
	t.Setenv(envSignExpectedPublicKey, base64.StdEncoding.EncodeToString(pub))
	// keyID is NOT in the active list -> Minted -> fail-closed.
	t.Setenv(envSignActiveKeyIDs, "some-other-key")

	var out bytes.Buffer
	err := runSignActive(&out, newSignActiveFlags(t))
	if err == nil {
		t.Fatal("expected fail-closed error for a Minted keyId, got nil")
	}
	if out.Len() != 0 {
		t.Fatalf("Minted keyId must not emit any active.json, got:\n%s", out.String())
	}
}

// TestSignActive_KeyMismatchFailClosed is the FR9 gate proper: an Active keyId
// whose OFFLINE_SIGN_EXPECTED_PUBLIC_KEY is a DIFFERENT keypair's public key (the
// operator paired the wrong private key with this keyId) is refused fail-closed —
// runSignActive returns offlinesig.ErrKeyMismatch and emits no active.json. This
// is the case the old self-derived-public-key bug could never trigger, because the
// allowlist key was derived from the same private key it was compared against.
func TestSignActive_KeyMismatchFailClosed(t *testing.T) {
	// The injected private key derives one public key...
	seedB64 := base64.StdEncoding.EncodeToString(signTestSeed)
	// ...but the operator pastes a DIFFERENT (unrelated) keypair's public key as the
	// keyId's published key.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate mismatched keypair: %v", err)
	}

	const keyID = "test-key-1"
	t.Setenv(envSignPrivateKey, seedB64)
	t.Setenv(envSignKeyID, keyID)
	t.Setenv(envSignActiveKeyIDs, keyID) // Active, so we reach the consistency check.
	t.Setenv(envSignExpectedPublicKey, base64.StdEncoding.EncodeToString(otherPub))

	var out bytes.Buffer
	err = runSignActive(&out, newSignActiveFlags(t))
	if err == nil {
		t.Fatal("expected ErrKeyMismatch for a wrong private-key/keyId pairing, got nil")
	}
	if !errors.Is(err, offlinesig.ErrKeyMismatch) {
		t.Fatalf("expected error to wrap offlinesig.ErrKeyMismatch, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("key mismatch must not emit any active.json, got:\n%s", out.String())
	}
}

// TestSignActive_ExpectedPublicKeyInputErrors asserts the published-public-key
// input is validated: a missing value, undecodable base64, and a decoded length
// other than 32 bytes each error out with no active.json — signing an active keyId
// requires a well-formed published key to check the pairing against (FR9). The
// errors must not echo the (already public, but kept clean) key bytes.
func TestSignActive_ExpectedPublicKeyInputErrors(t *testing.T) {
	seedB64 := base64.StdEncoding.EncodeToString(signTestSeed)
	const keyID = "test-key-1"

	cases := map[string]string{
		"missing":        "",
		"not base64":     "!!!not-base64!!!",
		"too short (31)": base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"too long (33)":  base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"full 64-byte":   base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envSignPrivateKey, seedB64)
			t.Setenv(envSignKeyID, keyID)
			t.Setenv(envSignActiveKeyIDs, keyID)
			t.Setenv(envSignExpectedPublicKey, expected)

			var out bytes.Buffer
			err := runSignActive(&out, newSignActiveFlags(t))
			if err == nil {
				t.Fatal("expected an error for an invalid expected public key, got nil")
			}
			if out.Len() != 0 {
				t.Fatalf("invalid expected public key must not emit any active.json, got:\n%s", out.String())
			}
		})
	}
}
