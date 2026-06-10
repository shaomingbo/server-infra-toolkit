// Package log provides structured JSON logging to stdout for the service.
//
// It is a thin wrapper over log/slog. The HTTP access-log middleware calls
// Request to emit one line per handled request with a fixed set of fields.
package log

import (
	"log/slog"
	"os"
	"time"
)

var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// Request emits a single structured access-log line. The fields are fixed so
// the output schema stays stable for downstream log processing.
func Request(requestID, method, path string, status int, latency time.Duration, version string) {
	logger.Info("request",
		slog.String("request_id", requestID),
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.Int64("latency_ms", latency.Milliseconds()),
		slog.String("version", version),
	)
}

// Panic records a recovered panic at error level. It is emitted by the recover
// middleware in addition to the access-log line so the panic value is captured
// regardless of whether a response had already been committed.
func Panic(requestID, method, path string, value any) {
	logger.Error("panic recovered",
		slog.String("request_id", requestID),
		slog.String("method", method),
		slog.String("path", path),
		slog.Any("panic", value),
	)
}
