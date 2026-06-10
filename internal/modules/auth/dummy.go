package auth

// dummy.go provides a constant-work password verification path for the
// "user does not exist" case (FR10 / AC4).
//
// THE TIMING LEAK IT CLOSES: if login skips argon2 entirely when the username is
// not found, an attacker can distinguish "no such user" (fast response) from
// "wrong password" (slow response, because argon2 ran). That difference enables
// username enumeration. The fix is to ALWAYS spend an equivalent argon2 cost:
// when the user is absent, run Verify against a fixed dummy PHC string built with
// the current parameters, so the not-found path takes the same wall-clock work
// as a real wrong-password path. The result is discarded — it is always false by
// construction — but the work is real.

import (
	"crypto/rand"
)

// dummyHash is an argon2id PHC at the CURRENT parameters, precomputed ONCE at
// process start. The not-found login path verifies against it so that path costs
// exactly one argon2 — the same as a real wrong-password — WITHOUT building a hash
// on the request path. A lazily-built dummy made the first not-found request after
// each cold start pay two argon2 computations, reopening the enumeration timing
// channel this is meant to close. Deriving it from currentParams (not a hardcoded
// literal) keeps the cost auto-tracking any future parameter upgrade.
var dummyHash = mustBuildDummyHash()

func mustBuildDummyHash() string {
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		// CSPRNG unavailable at startup: a server that cannot generate randomness
		// must not come up serving auth. Fail fast rather than per-request.
		panic("auth: cannot build dummy password hash (CSPRNG unavailable) at startup: " + err.Error())
	}
	h, err := Hash(string(pw))
	if err != nil {
		panic("auth: cannot build dummy password hash at startup: " + err.Error())
	}
	return h
}

// DummyVerify spends exactly one argon2id verification against the precomputed
// dummy hash, then returns false. It never builds a hash on the request path, so
// every not-found login costs the same — closing the enumeration timing channel.
// The result is always false (the dummy password is random); the work is the point.
func DummyVerify(password string) bool {
	_, _ = Verify(password, dummyHash) // result discarded; the argon2 work is what equalizes timing
	return false
}
