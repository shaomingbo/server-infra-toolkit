package auth

// lockout.go holds the account-lockout policy parameters (FR1/NFR3) and the
// account-locked audit event (FR11/D10). The lockout MECHANICS — atomic failure
// counting, the pre-credential lock gate, success reset, DB-clock expiry — live in
// login.go's verifyCredentials; this file owns the tunable constants and the
// security-event log line so both are in one auditable place.

import (
	"log/slog"
	"os"
	"time"
)

// Account-lockout policy (FR1/NFR3). These are PARAMETERS passed into the atomic
// RecordLoginFailure query, never inlined magic numbers.
//
// PROVENANCE — the OWASP Authentication Cheat Sheet (Account Lockout / Login
// Throttling section, verified live 2026-06-09) does NOT prescribe fixed numbers;
// it enumerates the three knobs an account-lockout policy must set — "the number
// of failed attempts before the account is locked out (lockout threshold)", "the
// time period that these attempts must occur within (observation window)", and
// "how long the account is locked out for (lockout duration)" — and is explicit
// that the counter "should be associated with the account itself, rather than the
// source IP address" (which is exactly D2's per-account atomic counter). It leaves
// the values a balance "between security and usability". We therefore ship a
// conservative, widely-cited baseline and mark it for calibration:
//
//   - lockoutThreshold = 5 failed attempts: a common industry baseline that stops
//     online password guessing while tolerating a handful of human typos before a
//     successful login resets the count (FR3/D6).
//   - lockoutWindow = 15 minutes: the duration the account stays locked after the
//     threshold is hit. A short window auto-recovers a typo-locked legitimate user
//     (E5) while making bulk online brute force economically pointless.
//
// CALIBRATION CAVEAT: these are a safe default, NOT a value tuned against this
// deployment's real lockout/false-lock telemetry. Revisit if real users report
// spurious lockouts (loosen) or abuse telemetry shows tolerated guessing (tighten);
// the escalation triggers in PRD D1 (public self-registration / abnormal lockout
// complaints) are the signal to re-evaluate the whole semantics.
const (
	lockoutThreshold = 5
	lockoutWindow    = 15 * time.Minute
)

// lockoutEventLogger emits account-lockout audit events to stdout as structured
// JSON, mirroring internal/platform/log's handler choice (slog JSON to os.Stdout)
// so the line lands in the same Cloud Logging stream as access logs. auth cannot
// import internal/platform/log's Request/Panic helpers for this bespoke event
// shape, and must NOT import internal/http, so it owns this tiny logger directly.
// Declared once at package scope so every lockout event uses the same handler.
var lockoutEventLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// logAccountLocked records ONE account-lockout security event (FR11/AC12/D10):
// event=account_locked with the user id and the request id, and NOTHING ELSE — no
// username, no password, no token. It is emitted exactly when an atomic
// RecordLoginFailure crosses the threshold and sets locked_until, so the count of
// these lines equals the count of lockouts. The source IP is intentionally absent
// (auth cannot import internal/http to obtain it; deferred per D10).
func logAccountLocked(requestID, userID string) {
	lockoutEventLogger.Info("account locked",
		slog.String("event", "account_locked"),
		slog.String("user_id", userID),
		slog.String("request_id", requestID),
	)
}
