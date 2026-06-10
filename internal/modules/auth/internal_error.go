package auth

import (
	"log/slog"
	"os"
)

// internalErrorLogger emits server-side internal-error diagnostics to stdout as
// structured JSON, mirroring internal/platform/log's handler choice (slog JSON to
// os.Stdout) so the line lands in the same Cloud Logging stream as access logs.
// auth cannot import internal/platform/log's helpers for this bespoke event shape,
// and must NOT import internal/http, so it owns this tiny logger directly — the
// SAME pattern as lockoutEventLogger (lockout.go) and the db package's retryLogger.
// Declared once at package scope so every internal-error event uses one handler.
var internalErrorLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// logInternalError records ONE server-side internal-error event behind a generic
// 500. The client response stays a credential/cause-free generic 500 (anti-
// enumeration, frozen envelope); this server-side line is the observability seam
// that makes the underlying cause visible in Cloud Logging. The error string is
// logged IN FULL — for a DB failure it carries pgx's SQLSTATE, the detail needed to
// diagnose e.g. a Neon cold-start failure. NOTHING credential-bearing is logged: no
// username, no password, no token, no selector/verifier — only the op label, the
// request id, and the (credential-free) error string.
func logInternalError(op, requestID string, err error) {
	internalErrorLogger.Error("auth internal error",
		slog.String("event", "auth_internal_error"),
		slog.String("op", op),
		slog.String("request_id", requestID),
		slog.String("error", err.Error()),
	)
}
