package auth

// password.go implements argon2id password hashing with a self-written PHC
// (Password Hashing Competition) string encoder/decoder.
//
// WHY HAND-ROLL THE PHC CODEC: the de-facto convenience wrappers
// (alexedwards/argon2id et al.) pull in a dependency for ~100 lines of
// encode/parse logic. The encoding is a frozen, spec'd format, so we own it
// directly on top of golang.org/x/crypto/argon2 (the only dependency, already
// pinned for the KDF itself). Self-written PHC is easy to get subtly wrong; the
// codec is therefore pinned byte-for-byte against the official P-H-C test vector
// in password_test.go.
//
// PHC string shape (parameter order m,t,p is mandatory):
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>
//
// Base64 is base64.RawStdEncoding: the STANDARD alphabet (+/), NO '=' padding.
// This is NOT URL-safe encoding — using URLEncoding would produce a string that
// does not match other argon2 implementations or the reference test vector.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2Params bundles the four cost/output knobs of one argon2id invocation.
// Centralizing them in one type lets Hash, the current-recommendation constant,
// the dummy hash, and NeedsRehash all speak the same language.
type argon2Params struct {
	memory      uint32 // m: memory cost in KiB
	iterations  uint32 // t: number of passes
	parallelism uint8  // p: degree of parallelism (lanes/threads)
	saltLength  uint32 // salt length in bytes
	keyLength   uint32 // derived key (hash) length in bytes
}

// currentParams is the SINGLE source of truth for the recommended argon2id cost.
// Hashing always uses these; verification uses whatever the stored PHC string
// encodes; NeedsRehash compares a stored string's params against these to decide
// whether to upgrade an old hash on next login.
//
// PARAMETER PROVENANCE:
//   - m=19456 KiB (=19 MiB), t=2, p=1 are the OWASP Password Storage Cheat Sheet
//     MINIMUM for argon2id, verified live 2026-06-09 ("19 MiB of memory, an
//     iteration count of 2, and 1 degree of parallelism"). Source:
//     https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id
//   - saltLength=16 / keyLength=32 come from RFC 9106 (Argon2 spec), NOT OWASP —
//     RFC 9106 §3.1 recommends a 128-bit (16-byte) salt and a 256-bit (32-byte)
//     tag for password hashing.
//
// CALIBRATION CAVEAT (DO NOT calibrate on this Mac): these are the OWASP floor,
// not a machine-tuned value. The real target hardware is Cloud Run, not a local
// dev box, so cost MUST be re-tuned on the actual Cloud Run instance class to
// land around ~500ms per hash while staying clear of OOM — memory cost m is
// multiplied by request concurrency, so a too-large m under load can exhaust the
// instance's RAM. Re-test whenever the instance size changes. Until that
// calibration runs on Cloud Run, we ship the OWASP floor as a safe baseline.
var currentParams = argon2Params{
	memory:      19456,
	iterations:  2,
	parallelism: 1,
	saltLength:  16,
	keyLength:   32,
}

// phcPrefix is the algorithm id segment all our hashes carry. We only ever
// produce and accept argon2id (the hybrid variant); argon2i / argon2d strings
// are rejected at parse time.
const phcAlgo = "argon2id"

// PHC cost-parameter ceilings. These are NOT operational limits (current values are
// m=19456,t=2,p=1) — they are absurdity guards far above any value this deployment
// would legitimately use, so a tampered or corrupted stored hash cannot force a
// multi-gigabyte / many-thousand-iteration argon2 at Verify time (resource-exhaustion
// DoS). decodeHash is the trust boundary for stored hashes; bound it on both ends.
const (
	maxMemoryKiB   = 1 << 20 // 1 GiB
	maxIterations  = 64
	maxParallelism = 16
)

// b64 is the exact encoding the PHC format mandates: standard alphabet, no
// padding. Declared once so encode and decode can never drift apart.
var b64 = base64.RawStdEncoding

// Sentinel errors so callers can branch on cause if needed; all malformed input
// yields an error (never a panic).
var (
	errInvalidHashFormat = errors.New("auth: invalid PHC hash format")
	errIncompatibleAlgo  = errors.New("auth: incompatible password hash algorithm")
	errIncompatibleVer   = errors.New("auth: incompatible argon2 version")
)

// Hash derives an argon2id PHC string from a plaintext password using the
// current recommended parameters and a fresh cryptographically random salt.
// Two calls with the same password return different strings (different salts),
// and the returned string contains the salt and parameters but never the
// plaintext password.
func Hash(password string) (string, error) {
	salt := make([]byte, currentParams.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	return encodeHash(password, salt, currentParams), nil
}

// Verify reports whether password matches the stored argon2id PHC string. It
// parses the parameters and salt out of phc, recomputes the hash with exactly
// those parameters, and compares in constant time. A malformed phc string
// returns (false, error) rather than panicking; a well-formed but non-matching
// password returns (false, nil).
//
// Constant-time comparison (crypto/subtle.ConstantTimeCompare) is mandatory
// here: a byte-wise == / bytes.Equal would short-circuit on the first differing
// byte and leak, via timing, how much of a guessed hash was correct.
func Verify(password, phc string) (bool, error) {
	params, salt, want, err := decodeHash(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey(
		[]byte(password),
		salt,
		params.iterations,
		params.memory,
		params.parallelism,
		params.keyLength,
	)
	// ConstantTimeCompare returns 1 iff the slices are equal length AND content.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether a stored PHC string was produced with parameters
// weaker than the current recommendation, so login can transparently upgrade it.
// A string that fails to parse needs a rehash (true): we cannot trust it and the
// caller will recompute from the just-verified plaintext.
//
// "Weaker" means any cost knob (memory, iterations, parallelism) or output sizes
// (salt, key) below the current value. Stronger-than-current params are left
// alone (returns false) — we never downgrade an existing hash.
func NeedsRehash(phc string) bool {
	params, _, _, err := decodeHash(phc)
	if err != nil {
		return true
	}
	return params.memory < currentParams.memory ||
		params.iterations < currentParams.iterations ||
		params.parallelism < currentParams.parallelism ||
		params.saltLength < currentParams.saltLength ||
		params.keyLength < currentParams.keyLength
}

// encodeHash runs argon2id over (password, salt) with the given params and
// renders the canonical PHC string. The salt and key are base64.RawStdEncoding
// (standard alphabet, no padding). saltLength/keyLength in params are derived
// from the actual slice lengths so the encoding is always self-consistent.
func encodeHash(password string, salt []byte, params argon2Params) string {
	key := argon2.IDKey(
		[]byte(password),
		salt,
		params.iterations,
		params.memory,
		params.parallelism,
		params.keyLength,
	)
	return fmt.Sprintf(
		"$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcAlgo,
		argon2.Version,
		params.memory,
		params.iterations,
		params.parallelism,
		b64.EncodeToString(salt),
		b64.EncodeToString(key),
	)
}

// decodeHash strictly parses a PHC string into its parameters, salt, and stored
// key. It rejects, with an error (never a panic): wrong field count, unknown
// algorithm id, mismatched argon2 version, malformed parameter segment,
// out-of-range parameter values, and invalid base64. saltLength/keyLength in the
// returned params are set from the decoded byte lengths so a later recompute
// uses the right output size.
func decodeHash(phc string) (argon2Params, []byte, []byte, error) {
	// Canonical shape: "$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>" splits
	// into 6 parts on '$', the first being empty (leading '$').
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}

	if parts[1] != phcAlgo {
		return argon2Params{}, nil, nil, errIncompatibleAlgo
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	// Reject non-canonical version segments (trailing chars, leading zeros): Sscanf
	// stops at the first non-digit and ignores the rest, so "v=19x" would parse as
	// 19. A canonical round-trip pins the segment byte-for-byte.
	if parts[2] != fmt.Sprintf("v=%d", version) {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	if version != argon2.Version {
		return argon2Params{}, nil, nil, errIncompatibleVer
	}

	var params argon2Params
	// Sscanf with this exact format also enforces the mandatory m,t,p order.
	n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallelism)
	if err != nil || n != 3 {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	// Reject non-canonical params segments (trailing chars, leading zeros) for the
	// same reason as the version segment above.
	if parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", params.memory, params.iterations, params.parallelism) {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	// Range guards: reject nonsense / zero costs. argon2 requires t>=1, p>=1,
	// and memory >= 8*p (one of its internal invariants); a zero or sub-minimum
	// value here means a corrupt or hostile string.
	if params.memory < 8*uint32(params.parallelism) || params.iterations < 1 || params.parallelism < 1 {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	// Upper guards: reject absurdly large costs so a tampered/corrupt stored hash
	// cannot force a high-memory / high-CPU argon2 at Verify time (DoS).
	if params.memory > maxMemoryKiB || params.iterations > maxIterations || uint32(params.parallelism) > maxParallelism {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argon2Params{}, nil, nil, errInvalidHashFormat
	}
	params.saltLength = uint32(len(salt))
	params.keyLength = uint32(len(key))

	return params, salt, key, nil
}
