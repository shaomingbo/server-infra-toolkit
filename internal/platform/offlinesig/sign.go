package offlinesig

// sign.go is the signing + encoding + active.json assembly stage (S3) layered on
// top of the bit-exact canonical payload builder (S2, payload.go). It signs the
// canonical payload with PureEd25519, wraps the wire encodings the app's verifier
// expects, and gates signing behind a keyId allowlist + private-key↔public-key
// consistency check so the server can never emit a signatureV2 the app would
// fail-close on.
//
// FAIL-CLOSED ORDERING INVARIANT (the keyId publish-order contract): the app
// ships a keyId->publicKey mapping in its bundle BEFORE the server may sign with
// that keyId. Signing with a keyId the app does not yet know is a publisher-side
// availability incident (app unknownKeyId -> fail-closed). This stage enforces
// the server half: a keyId must be marked Active in the local allowlist before
// SignActive will produce a signature. A Minted-but-not-yet-Active keyId is
// refused (no signatureV2 emitted). Flipping a key to Active is a deliberate
// deploy-time human action; this stage only implements the gate, it never
// auto-promotes.
//
// WIRE ENCODINGS (must match the app verifier byte-for-byte):
//   - signatureV2 = "base64:" + StdEncoding(64-byte raw Ed25519 signature),
//     padding KEPT, standard +/ alphabet (NOT URL-safe).
//   - publicKey   = StdEncoding(32-byte RAW Ed25519 public key), padding KEPT.
//     NEVER SPKI/X.509/DER-wrapped (a 44-byte SPKI key makes the app decode wrong).

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// sigWirePrefix is the literal prefix on the signatureV2 wire value. The base64
// body that follows it is standard (padded, +/) Ed25519 signature bytes.
const sigWirePrefix = "base64:"

// SignPayload signs the exact canonical-payload bytes with PureEd25519 (RFC 8032,
// over the full payload, not pre-hashed) and returns the wire value
// "base64:"+StdEncoding(sig). Ed25519 signing is deterministic, so the same
// payload + key always yields the same wire value. The caller is responsible for
// producing payload via BuildCanonicalPayload — this function does not re-derive
// or re-validate the bytes.
func SignPayload(payload []byte, priv ed25519.PrivateKey) string {
	sig := ed25519.Sign(priv, payload)
	return sigWirePrefix + base64.StdEncoding.EncodeToString(sig)
}

// EncodePublicKey returns the published wire form of an Ed25519 public key: the
// standard padded base64 of its RAW 32 bytes. It deliberately does NOT wrap the
// key in SPKI/X.509/DER — that would be 44 bytes and the app would decode it
// wrong. Decoding the result must yield exactly 32 bytes.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// KeyStatus is a keyId's lifecycle state in the local signing allowlist. Only
// Active permits signing. The deliberately small set models the publish-order
// contract: a key is Minted when the server has the keypair but the app bundle
// has not yet shipped its public key; it is Active once the app can verify it.
type KeyStatus string

const (
	// StatusMinted means the keypair exists server-side but the app does not yet
	// know the public key. Signing with it is refused (fail-closed): the app
	// would reject the signatureV2 as unknownKeyId.
	StatusMinted KeyStatus = "minted"
	// StatusActive means the app has shipped the public key for this keyId and
	// the server may sign with it.
	StatusActive KeyStatus = "active"
)

// KeyState records a keyId's published public key and its allowlist status. The
// PublicKey is the SAME key the app ships under this keyId; SignActive asserts
// the private key derives exactly this public key before signing.
type KeyState struct {
	PublicKey ed25519.PublicKey
	Status    KeyStatus
}

// Allowlist maps keyId -> KeyState. keyId comparison is case-sensitive and
// untrimmed (the contract: keyId is case-sensitive, fixtures use [a-z0-9-]); the
// map's exact-string key semantics give this for free.
type Allowlist map[string]KeyState

// Errors returned by the signing gate. They are sentinels so callers can branch
// on the reason (e.g. distinguish "not active yet" from "unknown key") without
// string matching.
var (
	// ErrUnknownKeyID is returned when the keyId is absent from the allowlist.
	ErrUnknownKeyID = errors.New("offlinesig: unknown keyId")
	// ErrKeyNotActive is returned when the keyId exists but its status is not
	// Active (e.g. still Minted). Fail-closed: no signatureV2 is produced.
	ErrKeyNotActive = errors.New("offlinesig: keyId is not active")
	// ErrKeyMismatch is returned when the private key does not derive the public
	// key the allowlist records for the keyId. Signing would produce a signature
	// the app rejects under the published public key, so it is refused.
	ErrKeyMismatch = errors.New("offlinesig: private key does not match the keyId's published public key")
)

// SchemaVersion is the integer forward-compat header the app reads from
// active.json. It is 1 for contractVersion 1.0.0; signatureV2 is additive and
// does NOT bump it.
const SchemaVersion = 1

// ActiveManifest is the on-the-wire active.json shape the app's deserializer
// consumes field-for-field. Field names and JSON types match the app's golden
// active-config-schema-cases.json: required strings (Version/URL/Digest/Signature/
// KeyID), integer Timestamp and SchemaVersion, optional string MinAppVersion, and
// the additive optional string SignatureV2. fileManifestHash and rollbackFloor
// are NOT wire fields — they live only inside the signed v2 payload (dead wire
// fields, always empty in the empty-tail primary shape).
type ActiveManifest struct {
	Version       string `json:"version"`
	URL           string `json:"url"`
	Digest        string `json:"digest"`
	Signature     string `json:"signature"`
	KeyID         string `json:"keyId"`
	Timestamp     int64  `json:"timestamp"`
	MinAppVersion string `json:"minAppVersion"`
	SchemaVersion int    `json:"schemaVersion"`
	SignatureV2   string `json:"signatureV2"`
}

// SignActive builds the canonical v2 payload from f, gates it through the keyId
// allowlist + key-consistency check, signs it, and returns the assembled
// ActiveManifest with SignatureV2 populated. base is the manifest envelope the
// caller has already filled (Version/URL/Digest/Signature(v1)/KeyID/Timestamp/
// MinAppVersion); this function fills SchemaVersion and SignatureV2 and leaves the
// rest untouched.
//
// It refuses to emit a signatureV2 — returning an error and the zero manifest —
// when:
//   - the keyId is absent from the allowlist (ErrUnknownKeyID),
//   - the keyId's status is not Active (ErrKeyNotActive),
//   - priv does not derive the keyId's published public key (ErrKeyMismatch),
//   - or BuildCanonicalPayload rejects f (malformed digest/field).
//
// The private key never appears in the result or in any error.
func SignActive(base ActiveManifest, f Fields, priv ed25519.PrivateKey, allow Allowlist) (ActiveManifest, error) {
	state, ok := allow[base.KeyID]
	if !ok {
		return ActiveManifest{}, fmt.Errorf("%w: %q", ErrUnknownKeyID, base.KeyID)
	}
	if state.Status != StatusActive {
		return ActiveManifest{}, fmt.Errorf("%w: %q is %q", ErrKeyNotActive, base.KeyID, state.Status)
	}

	// The private key MUST derive the public key the allowlist records for this
	// keyId. If it does not, signing would produce a signature the app rejects
	// under its published public key — a silent desync. constant-time compare so
	// the check itself leaks nothing about the key bytes.
	derived := priv.Public().(ed25519.PublicKey)
	if subtle.ConstantTimeCompare(derived, state.PublicKey) != 1 {
		return ActiveManifest{}, ErrKeyMismatch
	}

	payload, err := BuildCanonicalPayload(f)
	if err != nil {
		return ActiveManifest{}, fmt.Errorf("offlinesig: build canonical payload: %w", err)
	}

	out := base
	out.SchemaVersion = SchemaVersion
	out.SignatureV2 = SignPayload(payload, priv)
	return out, nil
}

// privKeySeedLen is the length of an Ed25519 seed (32 bytes); ed25519.NewKeyFromSeed
// expands it into the 64-byte private key.
const privKeySeedLen = ed25519.SeedSize

// ParsePrivateKey decodes a base64-encoded Ed25519 private key from a Secret. It
// accepts EITHER a 32-byte seed (expanded via ed25519.NewKeyFromSeed) OR a full
// 64-byte private key, both as standard padded base64. Any other length, or
// undecodable base64, is rejected. The Secret wrapper keeps the key material out
// of logs; this function reveals it only to decode and never echoes it into the
// returned error.
func ParsePrivateKey(s config.Secret) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s.Reveal())
	if err != nil {
		// Do not include the decoded/raw value — only the (already redacted) shape.
		return nil, errors.New("offlinesig: private key is not valid standard base64")
	}
	switch len(raw) {
	case privKeySeedLen:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("offlinesig: private key is %d bytes, want %d (seed) or %d (full key)",
			len(raw), privKeySeedLen, ed25519.PrivateKeySize)
	}
}
