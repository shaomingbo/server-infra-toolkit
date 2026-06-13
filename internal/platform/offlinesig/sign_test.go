package offlinesig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// fixedSeed is a deterministic 32-byte seed so the signing round-trip tests use a
// stable keypair (it is a throwaway test key, never a real signing key).
var fixedSeed = []byte("offlinesig-s3-test-seed-32-bytes")

func testKeypair(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	if len(fixedSeed) != ed25519.SeedSize {
		t.Fatalf("fixedSeed is %d bytes, want %d", len(fixedSeed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(fixedSeed)
	return priv, priv.Public().(ed25519.PublicKey)
}

// TestSignPayload_SelfRoundTrip (AC6) signs the empty-tail canonical payload with
// a locally generated keypair and verifies the signature against the public key
// returned by EncodePublicKey's round-trip. The signature must carry the
// "base64:" prefix and a standard-padded base64 body.
func TestSignPayload_SelfRoundTrip(t *testing.T) {
	priv, pub := testKeypair(t)

	payload, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}

	wire := SignPayload(payload, priv)
	rawB64, ok := strings.CutPrefix(wire, "base64:")
	if !ok {
		t.Fatalf("signature missing base64: prefix: %q", wire)
	}
	sig, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		t.Fatalf("signature body is not standard base64: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	// Decode the public key the same way the app would, then verify.
	pubB64 := EncodePublicKey(pub)
	pubRaw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("decode encoded public key: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubRaw), payload, sig) {
		t.Fatal("self round-trip: signature does not verify against own public key")
	}
}

// TestSignPayload_Deterministic confirms Ed25519 signing is deterministic: the
// same payload + key yields the identical wire value.
func TestSignPayload_Deterministic(t *testing.T) {
	priv, _ := testKeypair(t)
	payload, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}
	if SignPayload(payload, priv) != SignPayload(payload, priv) {
		t.Fatal("SignPayload is not deterministic")
	}
}

// TestEncodePublicKey_RawThirtyTwoBytes (AC6) asserts the published public key
// decodes to EXACTLY 32 raw bytes, NOT 44 (the SPKI/X.509 wrapping the contract
// forbids — a 44-byte key would make the app decode wrong).
func TestEncodePublicKey_RawThirtyTwoBytes(t *testing.T) {
	_, pub := testKeypair(t)
	encoded := EncodePublicKey(pub)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("encoded public key is not standard base64: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Fatalf("public key decoded to %d bytes, want %d (raw, not 44-byte SPKI)", len(raw), ed25519.PublicKeySize)
	}
	if len(raw) == 44 {
		t.Fatal("public key is 44 bytes — SPKI/X.509 wrapping is forbidden")
	}
}

// TestSignActive_PopulatesSignatureV2 asserts the happy path: an Active keyId with
// a matching private key yields a manifest whose SignatureV2 verifies against the
// published public key, with SchemaVersion=1, and the caller-provided base fields
// left untouched.
func TestSignActive_PopulatesSignatureV2(t *testing.T) {
	priv, pub := testKeypair(t)
	const keyID = "test-key-1"
	allow := Allowlist{keyID: {PublicKey: pub, Status: StatusActive}}

	base := ActiveManifest{
		Version:       emptyTailFields.Version,
		URL:           "https://cdn.example.test/pkg-1.4.0.zip",
		Digest:        emptyTailFields.Digest,
		Signature:     "base64:legacyV1Placeholder",
		KeyID:         keyID,
		Timestamp:     1700000000,
		MinAppVersion: emptyTailFields.MinAppVersion,
	}

	out, err := SignActive(base, emptyTailFields, priv, allow)
	if err != nil {
		t.Fatalf("SignActive: %v", err)
	}
	if out.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", out.SchemaVersion, SchemaVersion)
	}
	// base fields preserved.
	if out.Version != base.Version || out.URL != base.URL || out.KeyID != base.KeyID || out.Timestamp != base.Timestamp {
		t.Fatalf("base fields mutated: %+v", out)
	}

	rawB64, ok := strings.CutPrefix(out.SignatureV2, "base64:")
	if !ok {
		t.Fatalf("signatureV2 missing base64: prefix: %q", out.SignatureV2)
	}
	sig, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		t.Fatalf("signatureV2 body not standard base64: %v", err)
	}
	payload, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("assembled signatureV2 does not verify against the published public key")
	}
}

// TestSignActive_MintedKeyRejected (AC10) asserts a keyId whose status is Minted
// (app has not yet shipped its public key) is refused fail-closed: an error and
// the zero manifest, no signatureV2.
func TestSignActive_MintedKeyRejected(t *testing.T) {
	priv, pub := testKeypair(t)
	const keyID = "test-key-1"
	allow := Allowlist{keyID: {PublicKey: pub, Status: StatusMinted}}

	base := ActiveManifest{KeyID: keyID, Version: emptyTailFields.Version, Digest: emptyTailFields.Digest, MinAppVersion: emptyTailFields.MinAppVersion}
	out, err := SignActive(base, emptyTailFields, priv, allow)
	if !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("expected ErrKeyNotActive, got %v", err)
	}
	if out.SignatureV2 != "" {
		t.Fatalf("minted key must not produce a signatureV2, got %q", out.SignatureV2)
	}
}

// TestSignActive_UnknownKeyRejected (AC10) asserts a keyId absent from the
// allowlist is refused.
func TestSignActive_UnknownKeyRejected(t *testing.T) {
	priv, pub := testKeypair(t)
	allow := Allowlist{"known-key": {PublicKey: pub, Status: StatusActive}}

	base := ActiveManifest{KeyID: "unknown-key", Version: emptyTailFields.Version, Digest: emptyTailFields.Digest, MinAppVersion: emptyTailFields.MinAppVersion}
	out, err := SignActive(base, emptyTailFields, priv, allow)
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("expected ErrUnknownKeyID, got %v", err)
	}
	if out.SignatureV2 != "" {
		t.Fatalf("unknown key must not produce a signatureV2, got %q", out.SignatureV2)
	}
}

// TestSignActive_KeyMismatchRejected (AC10) asserts that when the private key does
// NOT derive the public key the allowlist records for the keyId, signing is
// refused — this prevents emitting a signature the app would reject under its
// published public key.
func TestSignActive_KeyMismatchRejected(t *testing.T) {
	priv, _ := testKeypair(t)
	const keyID = "test-key-1"

	// A DIFFERENT public key than the one priv derives.
	otherPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	allow := Allowlist{keyID: {PublicKey: otherPub, Status: StatusActive}}

	base := ActiveManifest{KeyID: keyID, Version: emptyTailFields.Version, Digest: emptyTailFields.Digest, MinAppVersion: emptyTailFields.MinAppVersion}
	out, err := SignActive(base, emptyTailFields, priv, allow)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("expected ErrKeyMismatch, got %v", err)
	}
	if out.SignatureV2 != "" {
		t.Fatalf("key mismatch must not produce a signatureV2, got %q", out.SignatureV2)
	}
}

// TestSignActive_CaseSensitiveKeyID asserts keyId comparison is case-sensitive:
// an allowlist keyed by "test-key-1" does NOT match a request for "Test-Key-1".
func TestSignActive_CaseSensitiveKeyID(t *testing.T) {
	priv, pub := testKeypair(t)
	allow := Allowlist{"test-key-1": {PublicKey: pub, Status: StatusActive}}

	base := ActiveManifest{KeyID: "Test-Key-1", Version: emptyTailFields.Version, Digest: emptyTailFields.Digest, MinAppVersion: emptyTailFields.MinAppVersion}
	_, err := SignActive(base, emptyTailFields, priv, allow)
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("case-sensitive keyId: expected ErrUnknownKeyID for differing case, got %v", err)
	}
}

// TestSignActive_MalformedDigestRejected asserts a malformed digest in Fields is
// propagated as an error (no signatureV2), since BuildCanonicalPayload rejects it.
func TestSignActive_MalformedDigestRejected(t *testing.T) {
	priv, pub := testKeypair(t)
	const keyID = "test-key-1"
	allow := Allowlist{keyID: {PublicKey: pub, Status: StatusActive}}

	bad := emptyTailFields
	bad.Digest = "sha256:not-hex"
	base := ActiveManifest{KeyID: keyID, Version: bad.Version, Digest: bad.Digest, MinAppVersion: bad.MinAppVersion}
	out, err := SignActive(base, bad, priv, allow)
	if err == nil {
		t.Fatal("expected error for malformed digest")
	}
	if out.SignatureV2 != "" {
		t.Fatalf("malformed digest must not produce a signatureV2, got %q", out.SignatureV2)
	}
}

// TestParsePrivateKey accepts both a 32-byte seed and a full 64-byte key (both
// standard base64), and that both yield the same keypair; it rejects wrong-length
// and non-base64 input without leaking key material into the error.
func TestParsePrivateKey(t *testing.T) {
	seed := fixedSeed
	full := ed25519.NewKeyFromSeed(seed)

	seedB64 := base64.StdEncoding.EncodeToString(seed)
	fullB64 := base64.StdEncoding.EncodeToString(full)

	fromSeed, err := ParsePrivateKey(config.Secret(seedB64))
	if err != nil {
		t.Fatalf("ParsePrivateKey(seed): %v", err)
	}
	fromFull, err := ParsePrivateKey(config.Secret(fullB64))
	if err != nil {
		t.Fatalf("ParsePrivateKey(full): %v", err)
	}
	if !bytes.Equal(fromSeed, fromFull) {
		t.Fatal("seed and full-key inputs produced different private keys")
	}
	if !bytes.Equal(fromSeed, full) {
		t.Fatal("parsed key does not equal the expected key")
	}

	// Wrong length (16 bytes) is rejected.
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := ParsePrivateKey(config.Secret(short)); err == nil {
		t.Fatal("expected rejection for wrong-length private key")
	}

	// Non-base64 input is rejected, and the error must not echo the secret.
	const garbage = "not!base64!!!"
	if _, err := ParsePrivateKey(config.Secret(garbage)); err == nil {
		t.Fatal("expected rejection for non-base64 private key")
	} else if strings.Contains(err.Error(), garbage) {
		t.Fatalf("error leaked the secret value: %v", err)
	}
}
