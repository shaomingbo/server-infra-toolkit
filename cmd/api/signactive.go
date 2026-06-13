package main

// signactive.go is the offline-package v2 signing subcommand (`-sign-active`,
// T4 FR14/AC12). It is a one-shot operational CLI that turns a set of manifest
// inputs + an injected Ed25519 private key into a v2-signed active.json, written
// to stdout (or -o). It deliberately:
//
//   - never starts the HTTP server,
//   - never builds a connection pool or touches the database (it does NOT call
//     config.Load, so it has no NEON_DSN dependency — signing is a stateless,
//     offline computation),
//   - never makes any network request.
//
// The private key is read straight from the environment wrapped in config.Secret
// (mirroring the NEON_DSN precedent, D3/NFR2): its String/MarshalJSON/LogValue
// all render [REDACTED], so the plaintext key cannot leak into stdout, the
// emitted active.json, or any error this command prints.
//
// The keyId publish-order invariant (FR8) is enforced as a local Active
// allowlist: only keyIds listed in OFFLINE_SIGN_ACTIVE_KEY_IDS may sign. A keyId
// that is not on that list is treated as Minted and refused fail-closed (no
// signatureV2 is produced). Flipping a key to Active is a deliberate human action
// (adding it to that env), exactly as the contract requires.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/offlinesig"
)

// Environment variable names for the offline signing subcommand. The private key
// is the only secret; the keyId and the Active allowlist are public identifiers.
const (
	// envSignPrivateKey carries the Ed25519 private key as standard base64 (either
	// a 32-byte seed or a full 64-byte key). It is wrapped in config.Secret so it
	// is never logged or serialized in plaintext.
	envSignPrivateKey = "OFFLINE_SIGN_PRIVATE_KEY"
	// envSignKeyID is the keyId to sign under. Case-sensitive, untrimmed.
	envSignKeyID = "OFFLINE_SIGN_KEY_ID"
	// envSignActiveKeyIDs is the comma-separated list of keyIds whose local status
	// is Active (allowed to sign). A keyId absent from this list is Minted and
	// refused fail-closed.
	envSignActiveKeyIDs = "OFFLINE_SIGN_ACTIVE_KEY_IDS"
	// envSignExpectedPublicKey is the public key the app has PUBLISHED for this
	// keyId, taken from the app's release record. It is the standard padded base64
	// of the RAW 32-byte Ed25519 public key (the same wire form the app ships), NOT
	// derived from the private key here. SignActive checks the injected private key
	// derives exactly this published key (FR9); a mismatch means the operator paired
	// the wrong private key with this keyId and signing is refused fail-closed. It is
	// a public value (not a secret) so it is read plainly, never wrapped in Secret.
	envSignExpectedPublicKey = "OFFLINE_SIGN_EXPECTED_PUBLIC_KEY"
)

// signActiveFlags holds the per-invocation manifest inputs for -sign-active. The
// tail fields (fileManifestHash/rollbackFloor) are deliberately NOT flags: the
// empty-tail shape is the primary and only form this command emits (D10), so they
// are always empty. The v1 signature and url are wire fields carried verbatim into
// active.json; this command only computes signatureV2.
type signActiveFlags struct {
	version       *string
	digest        *string
	minAppVersion *string
	url           *string
	v1Signature   *string
	timestamp     *int64
	out           *string
}

// registerSignActiveFlags registers the -sign-active manifest input flags on fs
// and returns the bound values. They are only consumed when -sign-active is set.
func registerSignActiveFlags(fs *flag.FlagSet) *signActiveFlags {
	return &signActiveFlags{
		version:       fs.String("version", "", "active.json version (semver), e.g. 1.4.0 [-sign-active]"),
		digest:        fs.String("digest", "", "package digest, sha256:<64 lower-hex> [-sign-active]"),
		minAppVersion: fs.String("min-app-version", "", "minimum app version (semver) [-sign-active]"),
		url:           fs.String("url", "", "package download URL carried verbatim into active.json [-sign-active]"),
		v1Signature:   fs.String("v1-signature", "", "legacy v1 signature wire value carried verbatim into active.json.signature [-sign-active]"),
		timestamp:     fs.Int64("timestamp", 0, "active.json timestamp (unix seconds) [-sign-active]"),
		out:           fs.String("o", "", "write active.json to this path instead of stdout [-sign-active]"),
	}
}

// runSignActive builds a v2-signed active.json from the OFFLINE_* environment and
// the parsed manifest flags, then writes it (pretty JSON + trailing newline) to w
// or to -o. It performs no DB or network I/O. Any malformed input or a fail-closed
// keyId gate returns an error and writes nothing.
func runSignActive(w io.Writer, f *signActiveFlags) error {
	// Best-effort local .env load so an operator can keep OFFLINE_SIGN_* in .env
	// during development, matching config.Load's convention. A missing .env is fine;
	// values already in the environment (e.g. the test harness's t.Setenv) win.
	_ = godotenv.Load()

	priv, keyID, allow, err := loadSigningKey()
	if err != nil {
		return err
	}

	fields := offlinesig.Fields{
		Version:       *f.version,
		Digest:        *f.digest,
		MinAppVersion: *f.minAppVersion,
		// Empty-tail primary shape (D10): both tail fields stay empty.
		FileManifestHash: "",
		RollbackFloor:    "",
	}

	base := offlinesig.ActiveManifest{
		Version:       *f.version,
		URL:           *f.url,
		Digest:        *f.digest,
		Signature:     *f.v1Signature,
		KeyID:         keyID,
		Timestamp:     *f.timestamp,
		MinAppVersion: *f.minAppVersion,
	}

	manifest, err := offlinesig.SignActive(base, fields, priv, allow)
	if err != nil {
		// SignActive never echoes the private key into its errors; safe to return.
		return err
	}

	// Marshal with indentation for a readable active.json. The Secret-wrapped key
	// is not part of ActiveManifest, so nothing here can leak it.
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active.json: %w", err)
	}
	encoded = append(encoded, '\n')

	if path := *f.out; path != "" {
		// 0o644: the active.json contains only public manifest fields + the
		// signature, no secret material.
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}

	if _, err := w.Write(encoded); err != nil {
		return fmt.Errorf("write active.json: %w", err)
	}
	return nil
}

// loadSigningKey reads the private key (Secret), the keyId, the Active allowlist,
// and the app-PUBLISHED public key from the environment, then records that
// published key in the keyId's allowlist entry. The published key is an
// INDEPENDENT operator input (OFFLINE_SIGN_EXPECTED_PUBLIC_KEY, the value the app
// shipped for this keyId), NOT derived from the private key — that independence is
// what makes SignActive's private-key↔keyId consistency check (FR9) real: if the
// operator paired the wrong private key with this keyId, the derived public key
// will not match the published one and signing is refused fail-closed. The keyId's
// status is Active iff it appears in OFFLINE_SIGN_ACTIVE_KEY_IDS, else Minted
// (refused fail-closed by SignActive).
func loadSigningKey() (ed25519.PrivateKey, string, offlinesig.Allowlist, error) {
	rawKey := os.Getenv(envSignPrivateKey)
	if rawKey == "" {
		return nil, "", nil, fmt.Errorf("%s 未设置(Ed25519 私钥的标准 base64:32 字节 seed 或 64 字节完整私钥)", envSignPrivateKey)
	}
	keyID := os.Getenv(envSignKeyID)
	if keyID == "" {
		return nil, "", nil, fmt.Errorf("%s 未设置(要签发的 keyId,大小写敏感)", envSignKeyID)
	}

	// The published public key is a required, independent input: without it there is
	// nothing to check the private key against, so the FR9 consistency gate would be
	// vacuous. It is the standard padded base64 of the RAW 32-byte Ed25519 key the
	// app released for this keyId (operator copies it from the app's release record).
	expectedPub, err := loadExpectedPublicKey(os.Getenv(envSignExpectedPublicKey))
	if err != nil {
		return nil, "", nil, err
	}

	// Wrap the key in Secret before doing anything else so it is redacted in any
	// log/serialization from here on; ParsePrivateKey reveals it only to decode.
	priv, err := offlinesig.ParsePrivateKey(config.Secret(rawKey))
	if err != nil {
		// ParsePrivateKey already refuses to echo the key material into its error.
		return nil, "", nil, err
	}

	allow := offlinesig.Allowlist{
		keyID: {
			PublicKey: expectedPub,
			Status:    activeStatusFor(keyID, os.Getenv(envSignActiveKeyIDs)),
		},
	}
	return priv, keyID, allow, nil
}

// loadExpectedPublicKey decodes the app-published public key from its standard
// padded base64 wire form and requires exactly 32 bytes (a RAW Ed25519 public
// key, NOT a 44-byte SPKI/DER wrapping). It is empty/undecodable/wrong-length →
// error; the error never echoes the key bytes (the public key is not secret, but
// keeping errors free of decoded material matches the rest of this command). A
// missing value is an error: signing an active keyId requires something to verify
// the private-key pairing against.
func loadExpectedPublicKey(rawPub string) (ed25519.PublicKey, error) {
	if rawPub == "" {
		return nil, fmt.Errorf("%s 未设置(该 keyId 已发布的公钥:RAW 32 字节 Ed25519 的标准 base64,从 app 发布记录获取)", envSignExpectedPublicKey)
	}
	decoded, err := base64.StdEncoding.DecodeString(rawPub)
	if err != nil {
		return nil, fmt.Errorf("%s 不是合法的标准 base64", envSignExpectedPublicKey)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s 解码后是 %d 字节,要求恰好 %d 字节(RAW Ed25519 公钥,不要 SPKI/DER 包装)",
			envSignExpectedPublicKey, len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

// activeStatusFor returns StatusActive when keyID appears (exact, case-sensitive,
// trimmed of surrounding whitespace) in the comma-separated activeList, otherwise
// StatusMinted. A keyId the operator has not explicitly marked Active is treated
// as Minted and SignActive refuses it fail-closed (FR8) — the server must not sign
// with a keyId before the app has shipped its public key.
func activeStatusFor(keyID, activeList string) offlinesig.KeyStatus {
	for _, entry := range strings.Split(activeList, ",") {
		if strings.TrimSpace(entry) == keyID {
			return offlinesig.StatusActive
		}
	}
	return offlinesig.StatusMinted
}
