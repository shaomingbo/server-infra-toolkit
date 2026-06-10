package auth

// token.go mints the two opaque credentials a login issues: an access token and
// a split-token refresh token. Both are high-entropy random values; the database
// never stores the plaintext, only a SHA-256 of it (verification re-hashes and
// compares). SHA-256 is the right primitive here — unlike a *password*, these are
// long uniformly-random secrets, so a fast hash is sufficient (no argon2 needed);
// the hash exists only so a database leak does not yield usable tokens.
//
// TOKEN SHAPES (W1):
//   - access token:  32 random bytes -> base64.RawURLEncoding (URL-safe, no
//     padding). Stored column access_tokens.token_hash = SHA-256(raw bytes).
//   - refresh token: split-token. 16-byte selector + 16-byte verifier, each
//     RawURLEncoding, joined as "selector.verifier". The selector is the plaintext
//     lookup key (its own column, indexed); only the verifier is secret, stored as
//     verifier_hash = SHA-256(verifier raw bytes). The '.' separator is safe
//     because base64url's alphabet never contains '.', so strings.Cut splits
//     unambiguously on refresh.
//
// ENCODING NOTE: RawURLEncoding here is deliberately DIFFERENT from password.go's
// RawStdEncoding. The PHC password format mandates the standard alphabet; tokens
// travel in URLs/headers and use the URL-safe alphabet. Mixing them up would not
// break security but would produce values that round-trip incorrectly.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// accessTokenBytes is the raw entropy of an access token before encoding.
	// 32 bytes = 256 bits, the floor FR3 / OWASP set for an opaque session token.
	accessTokenBytes = 32
	// selectorBytes / verifierBytes size the two halves of a split-token refresh
	// credential. 16 bytes = 128 bits each: the selector only needs to be a
	// collision-resistant lookup key and the verifier a secret an attacker cannot
	// guess; 128 bits clears both comfortably.
	selectorBytes = 16
	verifierBytes = 16
)

// refreshSeparator joins the public selector and the secret verifier in the
// over-the-wire refresh token. base64url never emits '.', so this is an
// unambiguous delimiter that strings.Cut reverses exactly.
const refreshSeparator = "."

// tokenEnc is the encoding for all opaque tokens: URL-safe alphabet, no padding.
// Declared once so minting and any future parsing cannot drift.
var tokenEnc = base64.RawURLEncoding

// accessToken bundles a freshly minted access token's plaintext (returned to the
// client, never stored) with the SHA-256 hash that is persisted for lookup.
type accessToken struct {
	plaintext string // base64url opaque value handed to the client
	hash      []byte // SHA-256(raw bytes), stored in access_tokens.token_hash
}

// refreshToken bundles a freshly minted split-token refresh credential. plaintext
// is the "selector.verifier" string handed to the client; selector is persisted
// in cleartext as the lookup key; verifierHash is SHA-256(verifier raw bytes),
// the only secret-derived value stored.
type refreshToken struct {
	plaintext    string // "selector.verifier" handed to the client
	selector     string // base64url selector, stored in cleartext for lookup
	verifierHash []byte // SHA-256(verifier raw bytes), stored
}

// newAccessToken mints an opaque access token: 32 random bytes, base64url-encoded
// for the client, with the SHA-256 of the RAW bytes (not the encoded string) kept
// for storage. An error is returned only if the system CSPRNG is unavailable.
func newAccessToken() (accessToken, error) {
	raw, err := randomBytes(accessTokenBytes)
	if err != nil {
		return accessToken{}, fmt.Errorf("auth: mint access token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return accessToken{
		plaintext: tokenEnc.EncodeToString(raw),
		hash:      sum[:],
	}, nil
}

// newRefreshToken mints a split-token refresh credential: an independent random
// selector and verifier. The selector is stored in cleartext (it is only a lookup
// key); the verifier's SHA-256 is stored and its plaintext travels to the client.
// Hashing the verifier's RAW bytes (not its encoded form) keeps storage and any
// future verification consistent.
func newRefreshToken() (refreshToken, error) {
	selRaw, err := randomBytes(selectorBytes)
	if err != nil {
		return refreshToken{}, fmt.Errorf("auth: mint refresh selector: %w", err)
	}
	verRaw, err := randomBytes(verifierBytes)
	if err != nil {
		return refreshToken{}, fmt.Errorf("auth: mint refresh verifier: %w", err)
	}
	selector := tokenEnc.EncodeToString(selRaw)
	verifier := tokenEnc.EncodeToString(verRaw)
	sum := sha256.Sum256(verRaw)
	return refreshToken{
		plaintext:    selector + refreshSeparator + verifier,
		selector:     selector,
		verifierHash: sum[:],
	}, nil
}

// randomBytes returns n cryptographically random bytes, erroring if the CSPRNG
// cannot satisfy the read (treated as a server error by callers — never silently
// degraded, since a weak token is a security failure).
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// newTokenFamily generates a fresh random UUID (RFC 4122 version 4) for a refresh
// rotation chain. A new login starts a new family. We build it from crypto/rand
// directly into pgtype.UUID rather than adding a uuid dependency: the value only
// needs to be a unique 128-bit identifier, and pgtype.UUID is a plain [16]byte.
func newTokenFamily() (pgtype.UUID, error) {
	b, err := randomBytes(16)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("auth: mint token family: %w", err)
	}
	// Set the version (4) and variant (RFC 4122) bits so the value is a
	// well-formed v4 UUID, not just 16 arbitrary bytes.
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	var u pgtype.UUID
	copy(u.Bytes[:], b)
	u.Valid = true
	return u, nil
}
