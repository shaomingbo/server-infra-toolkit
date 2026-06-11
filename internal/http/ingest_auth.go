package http

// ingest_auth.go implements the T5 ingest-token verifier (FR4/D5): the
// "last-mile" authentication for POST /v1/events. It is deliberately an
// ASSEMBLY-LAYER concern — it lives here in internal/http, not in the
// observability module — because the module's dependency guard forbids it from
// importing auth, and duplicating security-sensitive comparison code into the
// business handler would lose testability. main (cmd/api) wraps the
// observability registrar's handler with this verifier, mirroring how the auth
// Bearer middleware is mounted (D5: assembly-layer, same shape as the existing
// BearerMiddleware seam).
//
// SECURITY INVARIANTS this file upholds:
//   - A single generic 401 for EVERY failure (missing header, wrong token). The
//     status and body are byte-for-byte identical so a probe cannot tell which
//     occurred and cannot learn whether a token was ever valid (E7/AC3).
//   - The header value is hashed EXACTLY as config.HashIngestToken defines —
//     lowercase hex of SHA-256 over the raw UTF-8 bytes, with NO trim or case
//     normalization (E5) — and compared in constant time (crypto/subtle) against
//     each configured hash, so neither an early mismatch nor a timing side channel
//     leaks which hash (if any) was close.
//   - Verification runs BEFORE any body read (AC4): a 2 MiB unauthenticated
//     request returns 401, not 413, and the body is never touched.
//   - The accepted-hash set is captured once at assembly time as an immutable
//     slice; the returned handler reads it without any lock (E6: no shared mutable
//     state on the hot path).

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"

	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// ingestRejectLogger emits one structured JSON line per rejected ingest request
// to stdout, mirroring internal/platform/log's handler choice (slog JSON to
// os.Stdout) so the line lands in the same Cloud Logging stream as access logs —
// the SAME package-scope-logger pattern as auth's internalErrorLogger. The event
// carries ONLY the event label and the request id; it NEVER logs the token value,
// the raw header, or any hash (AC5), so the log stream cannot be mined for a
// credential or for which hash was close.
var ingestRejectLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// logIngestAuthRejected records ONE ingest-auth rejection event. requestID comes
// from requestIDMiddleware upstream and is the empty string when the verifier runs
// outside that middleware (e.g. a direct unit test). NOTHING credential-bearing is
// logged — only the event label and the request id (AC5).
func logIngestAuthRejected(requestID string) {
	ingestRejectLogger.Info("ingest auth rejected",
		slog.String("event", "ingest_auth_rejected"),
		slog.String("request_id", requestID),
	)
}

// ingestTokenHeader is the dedicated header carrying the ingest token (D2: a
// dedicated header, NOT Authorization: Bearer, so a client interceptor cannot
// route the ingest token into the auth-refresh path).
const ingestTokenHeader = "X-Ingest-Token"

// ingestUnauthorizedMessage is the single generic message returned for every
// ingest-auth failure. It carries no detail about which failure occurred (E7).
const ingestUnauthorizedMessage = "unauthorized"

// IngestVerifier returns the func(http.Handler) http.Handler middleware that
// gates POST /v1/events on a valid X-Ingest-Token. main (cmd/api) wraps the
// observability handler with it so ONLY the events route is guarded — /livez and
// /v1/auth/* never pass through it (AC8).
//
// acceptedHashes is the immutable 1-2 element slice from config (current[,
// previous]); it is captured by the closure once at assembly time and read
// without a lock (E6). When the slice is empty the verifier rejects every request
// — but the fail-closed config coupling (config.Load) prevents the endpoint from
// ever being mounted with no hashes (AC1), so an empty slice here only arises in a
// misconfiguration the process already refused to start with.
//
// The hashing path is config.HashIngestToken, the SINGLE source of truth for the
// dual-end hash convention (AC9), so the verifier and the configured hashes can
// never drift from each other or from the client.
func IngestVerifier(acceptedHashes []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read the header as raw bytes — no trim, no case folding (E5) — and
			// hash it the canonical way. This happens BEFORE next runs, so the body
			// is never read on the rejection path (AC4).
			presented := config.HashIngestToken(r.Header.Get(ingestTokenHeader))

			if !ingestTokenMatches(presented, acceptedHashes) {
				// One generic 401 for missing-header AND wrong-token alike, byte-for-
				// byte identical (AC3/E7). The requestId comes from context (set by
				// requestIDMiddleware upstream); WriteError is the frozen envelope exit.
				requestID := RequestIDFromContext(r.Context())
				// Server-side observability seam for the rejection (AC5): one structured
				// line carrying only the event label and request id — never the token,
				// header, or hash. The client response stays the generic 401 above.
				logIngestAuthRejected(requestID)
				WriteError(w, http.StatusUnauthorized, CodeUnauthorized, ingestUnauthorizedMessage, requestID)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ingestTokenMatches reports whether presented (the hex hash of the incoming
// token) equals any accepted hash, using a constant-time compare per candidate so
// the duration does not reveal how many leading characters matched. It checks
// EVERY candidate (no short-circuit on the first match) so the time taken does not
// reveal which slot — current vs previous — matched (AC2: either hash passes).
func ingestTokenMatches(presented string, acceptedHashes []string) bool {
	matched := false
	for _, h := range acceptedHashes {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(h)) == 1 {
			matched = true
		}
	}
	return matched
}
