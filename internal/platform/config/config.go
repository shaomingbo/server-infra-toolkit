// Package config loads runtime configuration from environment variables.
//
// The only secret in T0 is the Neon DSN, modeled as the Secret type so it can
// never leak into logs or serialized output.
package config

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
)

// Secret wraps a sensitive string value. Both String and MarshalJSON return a
// redaction placeholder so the value cannot accidentally reach logs or any
// JSON encoder. Use Reveal to obtain the real value where it is genuinely
// needed (e.g. opening a database connection).
type Secret string

const redacted = "[REDACTED]"

// String implements fmt.Stringer and hides the underlying value.
func (s Secret) String() string { return redacted }

// MarshalJSON hides the underlying value when encoded as JSON. It marshals the
// redaction placeholder through encoding/json so the output is always valid JSON
// regardless of what redacted contains, rather than hand-building the string.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// LogValue implements slog.LogValuer so log/slog never reflects over the
// underlying string. Without it slog bypasses String()/MarshalJSON() and would
// print the plaintext secret.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the real underlying secret value.
func (s Secret) Reveal() string { return string(s) }

// Config holds the runtime configuration. It intentionally stays minimal,
// reading only from environment variables (frozen contract: os.Getenv + .env,
// no flag/config-file/remote source).
type Config struct {
	Port    string
	Version string
	DSN     Secret

	// EventsIngestEnabled gates the T5 observability event-ingestion endpoint
	// (POST /v1/events). It defaults to FALSE: the endpoint stays off the public
	// router until the client integrates (D2, seam-first — exposing a public
	// write-to-DB endpoint with no client traffic is an attack surface with no
	// upside). main only mounts the observability registrar when this is true; a
	// false value means the route is never registered and a request hits the
	// catch-all 404.
	EventsIngestEnabled bool
}

// Load reads configuration from environment variables. For local development it
// first attempts to load a .env file; a missing .env is not an error because
// production injects environment variables directly.
//
// PORT falls back to "8080" when unset. NEON_DSN is required. Version is not
// read here; the caller (main) injects it from the ldflags-provided value.
func Load() (*Config, error) {
	// Best-effort local .env load. Absence is fine; only missing required env
	// vars below are treated as errors.
	_ = godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dsn := os.Getenv("NEON_DSN")
	if dsn == "" {
		return nil, errors.New("NEON_DSN 未设置:本地请 cp .env.example .env 并填入 Neon 连接串")
	}

	return &Config{
		Port:                port,
		DSN:                 Secret(dsn),
		EventsIngestEnabled: os.Getenv("EVENTS_INGEST_ENABLED") == "true",
	}, nil
}
