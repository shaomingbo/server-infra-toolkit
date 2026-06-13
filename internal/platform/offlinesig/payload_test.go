package offlinesig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// The empty-tail primary-shape input, matching the emptyTail vector in
// canonical-payload-vectors.json. Declared once so every test signs/asserts the
// same tuple.
var emptyTailFields = Fields{
	Version:          "1.4.0",
	Digest:           "sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49",
	MinAppVersion:    "3.2.0",
	FileManifestHash: "",
	RollbackFloor:    "",
}

// vectorFile mirrors canonical-payload-vectors.json (only the fields the tests
// consume). expectedPayloadHex is the signing-key-INDEPENDENT byte oracle.
type vectorFile struct {
	SigV2Tag string `json:"sigV2Tag"`
	Vectors  []struct {
		Name  string `json:"name"`
		Input struct {
			Version          string `json:"version"`
			Digest           string `json:"digest"`
			MinAppVersion    string `json:"minAppVersion"`
			FileManifestHash string `json:"fileManifestHash"`
			RollbackFloor    string `json:"rollbackFloor"`
		} `json:"input"`
		ExpectedPayloadHex string `json:"expectedPayloadHex"`
	} `json:"vectors"`
}

// loadVectors reads the embedded canonical-payload-vectors.json. Reading from
// the SAME embedded file the pin guard protects keeps the test oracle and the
// shipped contract in lockstep.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := contractFS.ReadFile("contract/canonical-payload-vectors.json")
	if err != nil {
		t.Fatalf("read canonical-payload-vectors.json: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode canonical-payload-vectors.json: %v", err)
	}
	return v
}

func vectorByName(t *testing.T, vf vectorFile, name string) (Fields, string) {
	t.Helper()
	for _, vec := range vf.Vectors {
		if vec.Name == name {
			return Fields{
				Version:          vec.Input.Version,
				Digest:           vec.Input.Digest,
				MinAppVersion:    vec.Input.MinAppVersion,
				FileManifestHash: vec.Input.FileManifestHash,
				RollbackFloor:    vec.Input.RollbackFloor,
			}, vec.ExpectedPayloadHex
		}
	}
	t.Fatalf("vector %q not found", name)
	return Fields{}, ""
}

// TestBuildCanonicalPayload_EmptyTailGolden (AC1/AC2) pins the empty-tail
// payload byte-for-byte against the canonical oracle, and asserts the empty-tail
// structural invariants: the payload ends in 0x0a 0x0a and contains exactly five
// 0x0A separators.
func TestBuildCanonicalPayload_EmptyTailGolden(t *testing.T) {
	vf := loadVectors(t)
	fields, wantHex := vectorByName(t, vf, "emptyTail")

	got, err := BuildCanonicalPayload(fields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}
	if gotHex := hex.EncodeToString(got); gotHex != wantHex {
		t.Fatalf("payload hex mismatch\n got: %s\nwant: %s", gotHex, wantHex)
	}

	// Empty-tail: two trailing newlines.
	if n := len(got); n < 2 || got[n-1] != 0x0a || got[n-2] != 0x0a {
		t.Fatalf("empty-tail payload must end in 0x0a 0x0a, got tail %x", got[max(0, len(got)-2):])
	}
	// Exactly five 0x0A separators (six fields).
	if n := countByte(got, 0x0a); n != 5 {
		t.Fatalf("expected exactly 5 0x0A separators, got %d", n)
	}
	// No BOM prefix.
	if strings.HasPrefix(string(got), "\uFEFF") {
		t.Fatal("payload must not start with a BOM")
	}
}

// TestBuildCanonicalPayload_PopulatedGolden (AC8) proves the SAME six-field
// builder produces the populated-tail oracle bytes (non-empty fileManifestHash
// and rollbackFloor) — i.e. there is no empty-tail special case.
func TestBuildCanonicalPayload_PopulatedGolden(t *testing.T) {
	vf := loadVectors(t)
	fields, wantHex := vectorByName(t, vf, "populatedTail")

	got, err := BuildCanonicalPayload(fields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}
	if gotHex := hex.EncodeToString(got); gotHex != wantHex {
		t.Fatalf("payload hex mismatch\n got: %s\nwant: %s", gotHex, wantHex)
	}
}

// TestBuildCanonicalPayload_Deterministic (AC2) asserts two builds of the same
// input are byte-identical.
func TestBuildCanonicalPayload_Deterministic(t *testing.T) {
	a, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload a: %v", err)
	}
	b, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload b: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("non-deterministic output:\n a=%x\n b=%x", a, b)
	}
}

// TestBuildCanonicalPayload_DigestLowerHexNormalized (AC3) asserts an upper-hex
// digest is normalized to lower-hex, producing the identical golden bytes. This
// is the trap the normalization closes: an upper-hex digest would pass the app's
// digest check but fail its signature check.
func TestBuildCanonicalPayload_DigestLowerHexNormalized(t *testing.T) {
	vf := loadVectors(t)
	_, wantHex := vectorByName(t, vf, "emptyTail")

	upper := emptyTailFields
	upper.Digest = "sha256:FD3C6583E8CB43379BE18D1DBC374A171094C275F094E91D9837F2673EF53C49"

	got, err := BuildCanonicalPayload(upper)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload (upper-hex digest): %v", err)
	}
	if gotHex := hex.EncodeToString(got); gotHex != wantHex {
		t.Fatalf("upper-hex digest not normalized to lower\n got: %s\nwant: %s", gotHex, wantHex)
	}
}

// TestBuildCanonicalPayload_FieldHygieneReject (AC4) asserts every illegal field
// byte class is rejected fail-closed (error, no bytes). The newline cases are
// the injectivity guard; CR/BOM/control extend it.
func TestBuildCanonicalPayload_FieldHygieneReject(t *testing.T) {
	cases := map[string]func(*Fields){
		"version LF":           func(f *Fields) { f.Version = "1.4.0\n9f86" },
		"minAppVersion LF":     func(f *Fields) { f.MinAppVersion = "3.2.0\nx" },
		"fileManifestHash LF":  func(f *Fields) { f.FileManifestHash = "sha256:9f86\n" },
		"rollbackFloor LF":     func(f *Fields) { f.RollbackFloor = "1.2.0\n0.0.0" },
		"version CR":           func(f *Fields) { f.Version = "1.4.0\r" },
		"minAppVersion BOM":    func(f *Fields) { f.MinAppVersion = "\uFEFF3.2.0" },
		"version U+2028":       func(f *Fields) { f.Version = "1.4.0\u2028" },
		"minAppVersion U+0085": func(f *Fields) { f.MinAppVersion = "3.2.0\u0085" },
		"rollbackFloor tab":    func(f *Fields) { f.RollbackFloor = "1.2.0\t" },
		"version NUL":          func(f *Fields) { f.Version = "1.4.0\x00" },
		"version DEL":          func(f *Fields) { f.Version = "1.4.0\x7f" },
		"digest CR in hexpart": func(f *Fields) { f.Digest = "sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53\r" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := emptyTailFields
			mutate(&f)
			got, err := BuildCanonicalPayload(f)
			if err == nil {
				t.Fatalf("expected rejection, got nil error and payload %x", got)
			}
			if got != nil {
				t.Fatalf("rejected input must produce no bytes, got %x", got)
			}
		})
	}
}

// TestBuildCanonicalPayload_DigestFormatReject (AC8 negatives) asserts a
// structurally invalid digest is rejected.
func TestBuildCanonicalPayload_DigestFormatReject(t *testing.T) {
	cases := map[string]string{
		"missing prefix": "fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49",
		"wrong prefix":   "md5:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49",
		"hex too short":  "sha256:fd3c6583",
		"hex too long":   "sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c4900",
		"non-hex char":   "sha256:gd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49",
		"empty digest":   "",
		"prefix only":    "sha256:",
	}
	for name, digest := range cases {
		t.Run(name, func(t *testing.T) {
			f := emptyTailFields
			f.Digest = digest
			if got, err := BuildCanonicalPayload(f); err == nil {
				t.Fatalf("expected rejection for %q, got payload %x", digest, got)
			}
		})
	}
}

// TestBuildCanonicalPayload_MutationDefense (defensive) confirms hand-built
// mutations of the correct payload do NOT equal the oracle: a trailing 0x0A and
// a BOM prefix both diverge. This guards against a builder that accidentally
// appended a newline or a BOM.
func TestBuildCanonicalPayload_MutationDefense(t *testing.T) {
	vf := loadVectors(t)
	_, wantHex := vectorByName(t, vf, "emptyTail")

	got, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}

	trailingLF := hex.EncodeToString(append(append([]byte{}, got...), 0x0a))
	if trailingLF == wantHex {
		t.Fatal("payload-with-trailing-LF unexpectedly equals oracle")
	}
	withBOM := hex.EncodeToString(append([]byte{0xef, 0xbb, 0xbf}, got...))
	if withBOM == wantHex {
		t.Fatal("BOM-prefixed payload unexpectedly equals oracle")
	}
}

// interopFile mirrors the parts of verify-extended-cases.json the interop test
// consumes: the keyId->publicKey map and the empty-tail real signature.
type interopFile struct {
	PublicKeys map[string]string `json:"publicKeys"`
	Cases      []struct {
		Name      string `json:"name"`
		KeyID     string `json:"keyId"`
		Signature string `json:"signature"`
	} `json:"cases"`
}

func loadInterop(t *testing.T) interopFile {
	t.Helper()
	raw, err := contractFS.ReadFile("contract/verify-extended-cases.json")
	if err != nil {
		t.Fatalf("read verify-extended-cases.json: %v", err)
	}
	var f interopFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode verify-extended-cases.json: %v", err)
	}
	return f
}

// TestInterop_EmptyTailSignatureVerifies (AC7 — milestone ① core) is the
// strongest cross-repo correctness proof available without a private key. It
// takes the REAL Ed25519 signature the app produced over the empty-tail payload
// and verifies it against THIS package's BuildCanonicalPayload output. If it
// passes, the server's payload bytes are provably identical to the bytes the app
// signed — bit-exact replication. It then flips one payload byte and asserts
// verification fails, proving the signature actually binds the payload.
func TestInterop_EmptyTailSignatureVerifies(t *testing.T) {
	f := loadInterop(t)

	const keyID = "test-key-1"
	pubB64, ok := f.PublicKeys[keyID]
	if !ok {
		t.Fatalf("public key %q not present in interop fixture", keyID)
	}
	pubRaw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(pubRaw) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d (raw 32-byte Ed25519, not SPKI)", len(pubRaw), ed25519.PublicKeySize)
	}
	pub := ed25519.PublicKey(pubRaw)

	// Find the empty-tail case's real signature.
	var sigB64 string
	for _, c := range f.Cases {
		if c.Name == "validManifestEmptyTail" {
			if c.KeyID != keyID {
				t.Fatalf("empty-tail case keyId %q != %q", c.KeyID, keyID)
			}
			sigB64 = c.Signature
			break
		}
	}
	if sigB64 == "" {
		t.Fatal("validManifestEmptyTail case not found in interop fixture")
	}
	rawSigB64, ok := strings.CutPrefix(sigB64, "base64:")
	if !ok {
		t.Fatalf("signature missing %q prefix: %q", "base64:", sigB64)
	}
	sig, err := base64.StdEncoding.DecodeString(rawSigB64)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	payload, err := BuildCanonicalPayload(emptyTailFields)
	if err != nil {
		t.Fatalf("BuildCanonicalPayload: %v", err)
	}

	if !ed25519.Verify(pub, payload, sig) {
		t.Fatalf("INTEROP FAILED: app's real signature does not verify against this builder's payload\n payload=%x", payload)
	}

	// Negative: flip one payload byte and the signature must NOT verify, proving
	// the signature binds these exact bytes (not just "any payload").
	tampered := append([]byte{}, payload...)
	tampered[0] ^= 0x01
	if ed25519.Verify(pub, tampered, sig) {
		t.Fatal("signature verified against a tampered payload — binding is broken")
	}
}

// countByte counts occurrences of b in p.
func countByte(p []byte, b byte) int {
	n := 0
	for _, c := range p {
		if c == b {
			n++
		}
	}
	return n
}
