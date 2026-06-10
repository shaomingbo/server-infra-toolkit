package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// TestHash_NoPlaintext (AC3) asserts the produced PHC string never embeds the
// plaintext password — only algorithm, version, parameters, salt, and the derived
// key. A hash that leaked the password would defeat the entire point of hashing.
func TestHash_NoPlaintext(t *testing.T) {
	const password = "correct horse battery staple"
	phc, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strings.Contains(phc, password) {
		t.Fatalf("PHC string contains plaintext password: %q", phc)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC string has unexpected prefix: %q", phc)
	}
}

// TestHash_DistinctSalts (AC3) asserts two hashes of the same password differ,
// because each draws a fresh random salt. Identical outputs would mean a missing
// or fixed salt (rainbow-table exposure).
func TestHash_DistinctSalts(t *testing.T) {
	const password = "same-password"
	a, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash a: %v", err)
	}
	b, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash b: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical (salt not random)")
	}
}

// TestVerify_RoundTrip (AC3) asserts the correct password verifies and a password
// off by a single character does not.
func TestVerify_RoundTrip(t *testing.T) {
	const password = "p@ssw0rd-with-some-length"
	phc, err := Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := Verify(password, phc)
	if err != nil {
		t.Fatalf("Verify correct: %v", err)
	}
	if !ok {
		t.Fatal("correct password failed to verify")
	}

	// Flip the last character.
	wrong := password[:len(password)-1] + "X"
	ok, err = Verify(wrong, phc)
	if err != nil {
		t.Fatalf("Verify wrong: %v", err)
	}
	if ok {
		t.Fatal("one-character-different password incorrectly verified")
	}
}

// TestDecodeHash_SelfDescribing (AC3) asserts a freshly produced PHC string
// round-trips through the parser and the parsed parameters match what Hash used.
func TestDecodeHash_SelfDescribing(t *testing.T) {
	phc, err := Hash("anything")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	params, salt, key, err := decodeHash(phc)
	if err != nil {
		t.Fatalf("decodeHash: %v", err)
	}
	if params.memory != currentParams.memory ||
		params.iterations != currentParams.iterations ||
		params.parallelism != currentParams.parallelism {
		t.Fatalf("parsed cost params %+v != current %+v", params, currentParams)
	}
	if uint32(len(salt)) != currentParams.saltLength {
		t.Fatalf("parsed salt length %d != %d", len(salt), currentParams.saltLength)
	}
	if uint32(len(key)) != currentParams.keyLength {
		t.Fatalf("parsed key length %d != %d", len(key), currentParams.keyLength)
	}
}

// TestPHC_VectorByteForByte pins our hand-written PHC codec, byte-for-byte,
// against the canonical golang.org/x/crypto/argon2 implementation our Hash builds
// on. Password "hunter2", salt gZiV/M1gPc22ElAH/Jh1Hw, params m=65536,t=2,p=1.
// This catches the two classic self-written-PHC mistakes: wrong base64 alphabet
// (must be RawStdEncoding, +/ no padding — NOT URL-safe) and wrong parameter
// order in the PHC string.
//
// EXPECTED-VALUE PROVENANCE — read before changing:
// The task brief supplied a "P-H-C official" expected string whose key segment
// was CWOrkoo7oJBQ/iyh7uJ0LO2aLEfrHwTWllSAxT0zRno. That value is NOT reproducible
// by golang.org/x/crypto/argon2: calling argon2.IDKey directly (bypassing all of
// our code) with the SAME password/salt/params yields
// 9dzn6OYzH4VILTZyq3hAt5wVM0TIkfA4Gxs7W93u26I instead. We confirmed our codec is
// correct independently — the malformed-input suite proves the base64 alphabet
// and parameter order are right, and our encode matches the raw library output
// below exactly. The brief's expected key is therefore inconsistent with the Go
// x/crypto Argon2 we are required to build on (likely transcribed from a
// different implementation, e.g. one that pre-processes the password). We pin
// against the value the canonical library actually produces, so this vector is a
// real, self-verifying byte-level cross-check against x/crypto rather than
// against an unreproducible literal. (Discrepancy reported to the coordinator.)
func TestPHC_VectorByteForByte(t *testing.T) {
	const (
		password   = "hunter2"
		saltB64    = "gZiV/M1gPc22ElAH/Jh1Hw"
		expectFull = "$argon2id$v=19$m=65536,t=2,p=1$gZiV/M1gPc22ElAH/Jh1Hw$9dzn6OYzH4VILTZyq3hAt5wVM0TIkfA4Gxs7W93u26I"
	)
	salt, err := b64.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}

	vectorParams := argon2Params{memory: 65536, iterations: 2, parallelism: 1, saltLength: 16, keyLength: 32}

	// 1) Our encodeHash must produce the full canonical string byte-for-byte.
	got := encodeHash(password, salt, vectorParams)
	if got != expectFull {
		t.Fatalf("encoded PHC mismatch\n got: %s\nwant: %s", got, expectFull)
	}

	// 2) The expected key segment must equal the RAW library output, proving our
	// codec adds no transformation of its own.
	libKey := argon2.IDKey([]byte(password), salt, vectorParams.iterations, vectorParams.memory, vectorParams.parallelism, vectorParams.keyLength)
	wantKey := b64.EncodeToString(libKey)
	if !strings.HasSuffix(expectFull, "$"+wantKey) {
		t.Fatalf("expected key segment %q is not the raw x/crypto output %q", expectFull, wantKey)
	}

	// 3) Full Verify round-trip against the pinned string.
	ok, err := Verify(password, expectFull)
	if err != nil {
		t.Fatalf("Verify(%q): %v", password, err)
	}
	if !ok {
		t.Fatal("Verify rejected the correct password for the pinned vector")
	}
	if ok, _ := Verify("hunter3", expectFull); ok {
		t.Fatal("Verify accepted a wrong password for the pinned vector")
	}
}

// TestDecodeHash_RejectsMalformed asserts every class of malformed PHC input
// returns an error (and crucially never panics).
func TestDecodeHash_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":                 "",
		"not a phc string":      "hello world",
		"missing fields":        "$argon2id$v=19$m=19456,t=2,p=1$onlysalt",
		"too many fields":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA$extra",
		"unknown algo":          "$argon2d$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"wrong version":         "$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"bad param order (t,m)": "$argon2id$v=19$t=2,m=19456,p=1$c2FsdA$aGFzaA",
		"zero iterations":       "$argon2id$v=19$m=19456,t=0,p=1$c2FsdA$aGFzaA",
		"zero parallelism":      "$argon2id$v=19$m=19456,t=2,p=0$c2FsdA$aGFzaA",
		"memory below 8*p":      "$argon2id$v=19$m=4,t=2,p=1$c2FsdA$aGFzaA",
		"invalid base64 salt":   "$argon2id$v=19$m=19456,t=2,p=1$!!!notb64!!!$aGFzaA",
		"url-safe base64 (bad)": "$argon2id$v=19$m=19456,t=2,p=1$a-_b$aGFzaA",
		"padded base64 (bad)":   "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA==$aGFzaA",
		"empty salt":            "$argon2id$v=19$m=19456,t=2,p=1$$aGFzaA",
		"empty hash":            "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
		"non-numeric param":     "$argon2id$v=19$m=abc,t=2,p=1$c2FsdA$aGFzaA",
		// Upper-bound guards: a tampered hash must not be able to demand an absurd
		// argon2 cost at Verify time (resource-exhaustion DoS).
		"memory above ceiling":      "$argon2id$v=19$m=2097152,t=2,p=1$c2FsdA$aGFzaA", // 2 GiB
		"iterations above ceiling":  "$argon2id$v=19$m=19456,t=100,p=1$c2FsdA$aGFzaA",
		"parallelism above ceiling": "$argon2id$v=19$m=19456,t=2,p=32$c2FsdA$aGFzaA",
		// Non-canonical PHC: trailing chars / leading zeros must be rejected, not
		// silently truncated by Sscanf.
		"trailing chars in version":  "$argon2id$v=19x$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"trailing chars in params":   "$argon2id$v=19$m=19456,t=2,p=1x$c2FsdA$aGFzaA",
		"non-canonical leading zero": "$argon2id$v=19$m=019456,t=2,p=1$c2FsdA$aGFzaA",
	}
	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := decodeHash(phc); err == nil {
				t.Fatalf("decodeHash(%q) returned nil error; expected rejection", phc)
			}
			// Verify must also surface an error (and not panic) on the same input.
			if _, err := Verify("x", phc); err == nil {
				t.Fatalf("Verify against malformed %q returned nil error", phc)
			}
		})
	}
}

// TestNeedsRehash covers the upgrade-on-login decision.
func TestNeedsRehash(t *testing.T) {
	// A hash at current params does not need a rehash.
	current, err := Hash("pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if NeedsRehash(current) {
		t.Fatal("current-param hash flagged as needing rehash")
	}

	// A hash with weaker memory needs a rehash. Build one with a deliberately
	// lower memory cost via encodeHash + a 16-byte salt.
	weak := encodeHash("pw", make([]byte, 16), argon2Params{
		memory:      8192, // below current 19456
		iterations:  2,
		parallelism: 1,
		saltLength:  16,
		keyLength:   32,
	})
	if !NeedsRehash(weak) {
		t.Fatal("weaker-memory hash not flagged as needing rehash")
	}

	// A malformed/unparseable string needs a rehash (cannot be trusted).
	if !NeedsRehash("garbage") {
		t.Fatal("malformed hash not flagged as needing rehash")
	}
}

// TestDummyVerify_Behaves asserts the constant-work dummy path returns false for
// arbitrary input (its timing-equalizing job is exercised, the boolean is always
// false by construction).
func TestDummyVerify_Behaves(t *testing.T) {
	if DummyVerify("any-password") {
		t.Fatal("DummyVerify returned true; it must always be false")
	}
	// Second call uses the same precomputed dummy hash.
	if DummyVerify("another") {
		t.Fatal("DummyVerify (second call) returned true")
	}
}
