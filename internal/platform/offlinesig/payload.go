package offlinesig

// payload.go builds the v2 canonical signing payload — the EXACT bytes both the
// server (signing) and the app (verifying) feed to Ed25519. Getting this
// bit-exact is the whole point of the offline-package v2 contract: a single
// stray byte makes the app's signature check fail in a way that is near
// impossible to diagnose from the wire value alone.
//
// CANONICAL PAYLOAD (contractVersion 1.0.0):
//
//	UTF8( SigV2Tag + LF + version + LF + digest + LF + minAppVersion + LF +
//	      fileManifestHash + LF + rollbackFloor )
//
// where LF is a single 0x0A byte. Six fields, joined by exactly five 0x0A
// separators, in a FIXED order that is never reordered. The leading SigV2Tag
// domain-separates this scheme from the legacy v1 payload (version+LF+digest) so
// a v1 signature is structurally invalid here.
//
// EMPTY-TAIL PRIMARY SHAPE: the live app-manifest fetch signs with
// fileManifestHash="" and rollbackFloor="". Both empty fields are STILL joined
// (no "skip empty field" logic), so the payload ends in two trailing newlines —
// the final two bytes are 0x0a 0x0a. Dropping the empty trailers would change
// the bytes and break the app's verifier.
//
// HARD INVARIANTS (any violation here would silently desync the two ends):
//   - separator is a single 0x0A; never 0x0D 0x0A (CRLF), never multi-byte.
//   - no trailing newline after the last field, no UTF-8 BOM prefix.
//   - digest is lower-hex normalized before signing (the contract requires the
//     server to EMIT lower-hex; an upper-hex digest would pass the app's digest
//     check yet fail its signature check — the exact "passes one, fails the
//     other" trap this normalization exists to prevent).
//
// FIELD HYGIENE (injectivity guard, fail-closed on BOTH ends): pure-newline
// field joining is not injective, so a field carrying a 0x0A could re-split into
// a DIFFERENT tuple with the SAME payload bytes and smuggle, e.g., a lower
// rollbackFloor past the signature. Every legal field value (semver,
// "sha256:<hex>", or the empty string) is pure printable ASCII, so the guard
// allowlists exactly the printable-ASCII range [0x20, 0x7E] and REJECTS any
// other byte before the payload is built. That one rule subsumes 0x0A/0x0D, the
// C0 control set, DEL (0x7F), and every >=0x80 byte — so multi-byte controls
// whose UTF-8 encoding is all >=0x80 (the BOM U+FEFF = EF BB BF, U+2028 =
// E2 80 A8, U+0085 NEL = C2 85, etc.) are refused too. This only ever refuses
// adversarial input.

import (
	"errors"
	"fmt"
	"strings"
)

// SigV2Tag is the constant first line of every v2 canonical payload. It
// domain-separates the v2 scheme from the legacy v1 payload (version+LF+digest)
// so a v1 signature cannot be replayed against a v2 verifier.
const SigV2Tag = "offline-package-sig-v2"

// lf is the single-byte field separator. It is exactly 0x0A — never CRLF.
const lf = "\n"

// digestPrefix is the mandatory algorithm prefix on a digest field. The prefix
// is part of the signed bytes (it is not stripped before signing).
const digestPrefix = "sha256:"

// digestHexLen is the required length of the hex portion of a sha256 digest:
// 32 bytes -> 64 hex characters.
const digestHexLen = 64

// Fields is the six-field input tuple of a v2 canonical payload, in canonical
// order. FileManifestHash and RollbackFloor are empty in the empty-tail primary
// shape. This struct is the stable input contract the signing stage depends on.
type Fields struct {
	Version          string
	Digest           string
	MinAppVersion    string
	FileManifestHash string
	RollbackFloor    string
}

// BuildCanonicalPayload assembles the bit-exact v2 canonical payload bytes for
// f. It normalizes the digest to lower-hex (after validating its shape) and runs
// every field through the hygiene guard; it returns an error — and no bytes — if
// any field is malformed, so a caller can never sign a payload that would
// silently desync from the app's verifier.
func BuildCanonicalPayload(f Fields) ([]byte, error) {
	digest, err := normalizeDigest(f.Digest)
	if err != nil {
		return nil, err
	}

	// Build the six-field tuple in canonical order. The digest is already
	// normalized; every other field is validated by the hygiene guard below.
	// SigV2Tag is a trusted constant and needs no guarding.
	values := []struct {
		name, value string
	}{
		{"version", f.Version},
		{"minAppVersion", f.MinAppVersion},
		{"fileManifestHash", f.FileManifestHash},
		{"rollbackFloor", f.RollbackFloor},
	}
	for _, fv := range values {
		if err := checkFieldHygiene(fv.name, fv.value); err != nil {
			return nil, err
		}
	}

	// Fixed six-element order, single 0x0A between each. The empty tail fields
	// are joined like any other, so the empty-tail shape ends in 0x0a 0x0a.
	parts := []string{
		SigV2Tag,
		f.Version,
		digest,
		f.MinAppVersion,
		f.FileManifestHash,
		f.RollbackFloor,
	}
	return []byte(strings.Join(parts, lf)), nil
}

// normalizeDigest validates a digest field and returns it with the hex portion
// lower-cased. It rejects a missing "sha256:" prefix, a hex portion that is not
// exactly 64 characters, or any non-hex character. The contract requires the
// server to EMIT lower-hex, so an upper-hex input is normalized (not rejected)
// here — but a structurally invalid digest is rejected fail-closed.
func normalizeDigest(digest string) (string, error) {
	// A digest can never legally contain a control character either; guard it
	// before the structural checks so a newline-bearing digest is rejected with
	// a clear reason rather than failing the hex test.
	if err := checkFieldHygiene("digest", digest); err != nil {
		return "", err
	}
	hexPart, ok := strings.CutPrefix(digest, digestPrefix)
	if !ok {
		return "", fmt.Errorf("offlinesig: digest %q missing %q prefix", digest, digestPrefix)
	}
	if len(hexPart) != digestHexLen {
		return "", fmt.Errorf("offlinesig: digest hex length is %d, want %d", len(hexPart), digestHexLen)
	}
	lower := strings.ToLower(hexPart)
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("offlinesig: digest hex contains non-hex byte %q", lower[i])
		}
	}
	return digestPrefix + lower, nil
}

// errNonPrintableASCII names a field that carries a byte outside printable
// ASCII. Returned (wrapped) by checkFieldHygiene.
var errNonPrintableASCII = errors.New("contains a non-printable-ASCII byte")

// checkFieldHygiene rejects any field value carrying a byte outside printable
// ASCII [0x20, 0x7E]. Every legal field value (semver, "sha256:<hex>", or "")
// is pure printable ASCII, so this allowlist refuses only adversarial input. The
// single range check subsumes the separator 0x0A, CR 0x0D, the rest of the C0
// control set, DEL (0x7F), and every >=0x80 byte — so multi-byte controls
// (BOM U+FEFF, U+2028, U+0085 NEL, ...) whose UTF-8 bytes are all >=0x80 are
// rejected without any special-casing.
func checkFieldHygiene(name, value string) error {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("offlinesig: field %q at byte %d %w (0x%02x)", name, i, errNonPrintableASCII, c)
		}
	}
	return nil
}
