// Package config loads runtime configuration from environment variables.
//
// The only secret in T0 is the Neon DSN, modeled as the Secret type so it can
// never leak into logs or serialized output.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

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

	// EventsIngestTokenSHA256s holds the accepted ingest-token SHA-256 hashes
	// (current[,previous]) for POST /v1/events authentication. The env only ever
	// stores hashes, never the plaintext token (FR2/D3). Values are validated as
	// lowercase hex at Load time (an uppercase digest is rejected, NOT down-cased —
	// the config must already be canonical so it matches the verifier's lowercase
	// hash output); the verifier (assembled in cmd/api) compares the hash of an
	// incoming X-Ingest-Token against this immutable slice. These are hashes, not
	// plaintext credentials, so they are NOT wrapped in Secret, but they are never
	// logged.
	EventsIngestTokenSHA256s []string
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

	ingestEnabled := os.Getenv("EVENTS_INGEST_ENABLED") == "true"

	ingestHashes, err := parseIngestTokenHashes(os.Getenv("EVENTS_INGEST_TOKEN_SHA256S"))
	if err != nil {
		return nil, err
	}

	// fail-closed coupling: enabling the endpoint without any accepted hash would
	// expose an unauthenticated write-to-DB path. Refuse to start (AC1/FR3/E2).
	if ingestEnabled && len(ingestHashes) == 0 {
		return nil, errors.New("EVENTS_INGEST_ENABLED=true 但 EVENTS_INGEST_TOKEN_SHA256S 为空:拒绝以无认证开放态启动")
	}

	return &Config{
		Port:                     port,
		DSN:                      Secret(dsn),
		EventsIngestEnabled:      ingestEnabled,
		EventsIngestTokenSHA256s: ingestHashes,
	}, nil
}

// sha256HexRe matches a single lowercase hex SHA-256 digest (64 hex chars).
var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// emptyInputSHA256 is the SHA-256 of the empty input. We reject it as a configured
// ingest-token hash: an operator hashing a token with `printf '%s' "$TOKEN" | shasum`
// while $TOKEN is unset produces exactly this digest, and configuring it would let
// a request with NO X-Ingest-Token (header absent -> empty string hashed the same
// way by the verifier) authenticate. Treating it as a misconfiguration fails closed.
const emptyInputSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// parseIngestTokenHashes parses the comma-separated EVENTS_INGEST_TOKEN_SHA256S
// value into 1-2 validated hashes (current[,previous]). An empty/unset value
// yields a nil slice with no error (the fail-closed coupling against the flag is
// checked by the caller). A non-empty value must be well-formed: 1-2 entries,
// each a lowercase 64-char hex digest. Uppercase, non-hex, or >2 entries are
// rejected (E3) regardless of the enabled flag — a configured value must be
// valid. The SHA-256 of empty input is rejected too: configuring it would accept
// a request that omits the header (the verifier hashes the absent header's empty
// string to the same digest), so it is treated as a misconfiguration.
func parseIngestTokenHashes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) > 2 {
		return nil, fmt.Errorf("EVENTS_INGEST_TOKEN_SHA256S 至多 2 个哈希(current[,previous]),实得 %d 个", len(parts))
	}

	hashes := make([]string, 0, len(parts))
	for _, p := range parts {
		h := strings.TrimSpace(p)
		if !sha256HexRe.MatchString(h) {
			return nil, errors.New("EVENTS_INGEST_TOKEN_SHA256S 含非法哈希:每个必须为小写 64 字符 hex(^[0-9a-f]{64}$)")
		}
		if h == emptyInputSHA256 {
			return nil, errors.New("EVENTS_INGEST_TOKEN_SHA256S 含空输入的 SHA-256(e3b0c442...7852b855):疑似生成哈希时 token 变量为空,配置它会让缺 header 的请求通过认证,拒绝")
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

// HashIngestToken computes the canonical ingest-token hash: the lowercase hex
// encoding of SHA-256 over the token's UTF-8 bytes. This is the single source of
// truth for the dual-end hash convention — the verifier (cmd/api) hashes the
// incoming X-Ingest-Token through this function and the client computes the
// configured hash the same way (AC9). Do not change the convention without
// updating both ends and the anchored test.
func HashIngestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
