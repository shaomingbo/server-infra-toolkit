package auth

// middleware.go implements the Bearer-token authentication middleware (FR5): it
// verifies the opaque access token presented on the Authorization header and, on
// success, attaches the authenticated user id to the request context for
// downstream handlers. It is the gate for every PROTECTED route.
//
// SECURITY INVARIANTS this file upholds:
//   - A single generic 401 for EVERY failure (missing/malformed header, undecodable
//     token, unknown token, expired, revoked, disabled user). The status and body
//     are identical so the client cannot tell which one occurred, and no token or
//     user detail ever reaches a log or an error value.
//   - Revocation and disablement are IMMEDIATE (AC7/AC20): the token is looked up
//     and its revoked_at / expiry AND the owning user's status are checked on EVERY
//     request, so a revoked token or a disabled user is rejected on the very next
//     call (no cached session to outlive the revocation).
//   - It NEVER guards /livez (AC8): main mounts it only on protected routes, nested
//     inside request-id/access-log; the liveness probe never reaches it and so the
//     probe path carries no DB call.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"time"
)

// userStatusActive is the only user status that may authenticate. Any other value
// (e.g. "disabled") is treated as a credential failure on both the Bearer path and
// the refresh path (FR13/AC20).
const userStatusActive = "active"

// bearerPrefix is the case-sensitive scheme prefix of the Authorization header
// value ("Bearer <token>"). The single space separator is part of the prefix.
const bearerPrefix = "Bearer "

// ctxKey is an unexported context-key type so the authenticated-user value cannot
// collide with keys set by other packages (the standard context-key idiom).
type ctxKey int

const userIDKey ctxKey = 0

// UserIDFromContext returns the authenticated user's id (the canonical
// 8-4-4-4-12 hyphenated string) stored by BearerMiddleware, or "" if the request
// did not pass through it. Protected handlers read the caller's identity from here.
func UserIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

// BearerMiddleware returns the Bearer-token authentication middleware. It extracts
// the token from the Authorization header, hashes it the SAME way token.go stored
// it, looks it up, and rejects anything that is missing, malformed, unknown,
// revoked, expired, or owned by a non-active user — all with one generic 401. On
// success it attaches the user id to the request context and calls next.
//
// It is a method on *Handler so it has the DB seam without re-plumbing
// dependencies, and returns the standard func(http.Handler) http.Handler shape so
// main can wrap protected routes with it. It is NEVER applied to /livez (AC8).
func (h *Handler) BearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := w.Header().Get(requestIDHeader)

		if !h.authenticate(w, r) {
			// authenticate already returns false for every failure class; emit the
			// single generic 401 here so the body is identical across them.
			writeError(w, http.StatusUnauthorized, codeUnauthorized, unauthorizedMessage, requestID)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate verifies the request's Bearer token and, on success, returns true
// having attached the user id to the request's context (via r being replaced in
// place through *r = *r.WithContext(...)). It returns false for EVERY failure
// class — the caller maps all of them to one generic 401, so this function never
// distinguishes them in its return value, and a genuine server error (DB failure)
// is also a false (fail closed: a token cannot authenticate if the DB cannot
// confirm it). It never panics and never logs the token.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) bool {
	// (1) Extract "Bearer <token>". A missing header or a value that does not carry
	// the exact case-sensitive scheme prefix is a 401.
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return false
	}
	token := strings.TrimPrefix(authz, bearerPrefix)
	if token == "" {
		return false
	}

	// (2) Decode the opaque token to its raw bytes and SHA-256 it the SAME way
	// token.go stored access_tokens.token_hash. A token that does not decode is a
	// 401 (not a valid credential).
	raw, err := tokenEnc.DecodeString(token)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(raw)

	// (3) Look the token up by hash. Unknown token → 401; a genuine DB error → also
	// false (fail closed).
	at, err := h.q.GetAccessToken(r.Context(), sum[:])
	if err != nil {
		// pgx.ErrNoRows (unknown token) and any other DB error both fail closed.
		// We do not distinguish: an unverifiable token does not authenticate.
		return false
	}

	now := time.Now()

	// (4) Revoked (AC7: immediate) or expired → 401.
	if at.RevokedAt.Valid {
		return false
	}
	if at.ExpiresAt.Valid && !at.ExpiresAt.Time.After(now) {
		return false
	}

	// (5) User-status gate (FR13/AC20): the owning user must be active. A disabled
	// user is rejected even with a live, unrevoked token; a vanished user row is
	// likewise a 401.
	user, err := h.q.GetUserByID(r.Context(), at.UserID)
	if err != nil {
		return false
	}
	if user.Status != userStatusActive {
		return false
	}

	// (6) Authenticated. Attach the user id to the context for downstream handlers.
	// Replacing *r in place lets next.ServeHTTP see the enriched context without
	// the caller having to thread a new *http.Request back out.
	ctx := context.WithValue(r.Context(), userIDKey, uuidToString(at.UserID))
	*r = *r.WithContext(ctx)
	return true
}
