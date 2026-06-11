package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const fakeDSN = "postgres://user:p4ssw0rd-SECRET@ep-fake.neon.tech/db?sslmode=require"

// pinIngestBaseline nails the two ingest env vars to their off/empty baseline for a
// test that calls Load() but does NOT itself exercise ingest config. Without it, a
// local .env that sets EVENTS_INGEST_ENABLED / EVENTS_INGEST_TOKEN_SHA256S would
// leak into the process env and make a non-ingest assertion (e.g. PORT fallback)
// fail-closed or load a hash the test never asked for. t.Setenv restores the prior
// value on cleanup, so this only shadows the baseline for the duration of the test.
func pinIngestBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("EVENTS_INGEST_ENABLED", "")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", "")
}

func TestSecretStringRedacts(t *testing.T) {
	s := Secret(fakeDSN)
	if got := s.String(); got != "[REDACTED]" {
		t.Fatalf("String() = %q, want [REDACTED]", got)
	}
	if strings.Contains(s.String(), "p4ssw0rd") {
		t.Fatalf("String() leaked plaintext: %q", s.String())
	}
}

func TestSecretMarshalJSONRedacts(t *testing.T) {
	cfg := Config{Port: "8080", Version: "test", DSN: Secret(fakeDSN)}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out := string(b)
	if strings.Contains(out, "p4ssw0rd") || strings.Contains(out, "SECRET") || strings.Contains(out, "neon.tech") {
		t.Fatalf("marshaled config leaked DSN plaintext: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("marshaled config missing [REDACTED]: %s", out)
	}
}

func TestSecretSlogRedacts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	secret := Secret(fakeDSN)
	cfg := Config{Port: "8080", Version: "test", DSN: secret}

	// Log the Secret directly and as a nested attribute via the whole Config.
	logger.Info("test", slog.Any("dsn", secret), slog.Any("config", cfg))

	out := buf.String()
	if strings.Contains(out, "p4ssw0rd") || strings.Contains(out, "SECRET") || strings.Contains(out, "neon.tech") {
		t.Fatalf("slog output leaked DSN plaintext: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("slog output missing [REDACTED]: %s", out)
	}
}

func TestSecretReveal(t *testing.T) {
	s := Secret(fakeDSN)
	if got := s.Reveal(); got != fakeDSN {
		t.Fatalf("Reveal() = %q, want %q", got, fakeDSN)
	}
}

func TestLoadPortFallback(t *testing.T) {
	pinIngestBaseline(t)
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want fallback 8080", cfg.Port)
	}
}

func TestLoadPortFromEnv(t *testing.T) {
	pinIngestBaseline(t)
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("PORT", "9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "9090" {
		t.Fatalf("Port = %q, want 9090", cfg.Port)
	}
	if cfg.DSN.Reveal() != fakeDSN {
		t.Fatalf("DSN not loaded from env")
	}
}

func TestLoadMissingDSN(t *testing.T) {
	pinIngestBaseline(t)
	t.Setenv("PORT", "8080")
	t.Setenv("NEON_DSN", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load: expected error when NEON_DSN unset, got nil")
	}
}

// validHash is a syntactically valid lowercase 64-char hex SHA-256 digest used
// to exercise config parsing without standing in for any real token.
const validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestLoadIngestEnabledMissingHashes covers AC1/FR3: flag on + no hashes must
// fail Load.
func TestLoadIngestEnabledMissingHashes(t *testing.T) {
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "true")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load: expected error when EVENTS_INGEST_ENABLED=true and hashes empty, got nil")
	}
}

// TestLoadIngestEnabledWithHash covers AC1: flag on + a valid hash loads
// successfully and the hash is parsed into the slice.
func TestLoadIngestEnabledWithHash(t *testing.T) {
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "true")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", validHash)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EventsIngestEnabled {
		t.Fatal("EventsIngestEnabled = false, want true")
	}
	if len(cfg.EventsIngestTokenSHA256s) != 1 || cfg.EventsIngestTokenSHA256s[0] != validHash {
		t.Fatalf("EventsIngestTokenSHA256s = %v, want [%s]", cfg.EventsIngestTokenSHA256s, validHash)
	}
}

// TestLoadIngestDisabledMissingHashes covers AC1/E1: flag off + no hashes loads
// successfully (pre-configuration is allowed; the endpoint stays 404).
func TestLoadIngestDisabledMissingHashes(t *testing.T) {
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "false")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EventsIngestEnabled {
		t.Fatal("EventsIngestEnabled = true, want false")
	}
	if len(cfg.EventsIngestTokenSHA256s) != 0 {
		t.Fatalf("EventsIngestTokenSHA256s = %v, want empty", cfg.EventsIngestTokenSHA256s)
	}
}

// TestLoadIngestDisabledWithHash covers E1: flag off + a valid hash is legal
// (pre-configured for a later flag flip).
func TestLoadIngestDisabledWithHash(t *testing.T) {
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "false")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", validHash)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EventsIngestEnabled {
		t.Fatal("EventsIngestEnabled = true, want false")
	}
	if len(cfg.EventsIngestTokenSHA256s) != 1 || cfg.EventsIngestTokenSHA256s[0] != validHash {
		t.Fatalf("EventsIngestTokenSHA256s = %v, want [%s]", cfg.EventsIngestTokenSHA256s, validHash)
	}
}

// TestLoadIngestTwoHashes confirms current+previous dual-hash parsing.
func TestLoadIngestTwoHashes(t *testing.T) {
	second := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "true")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", validHash+","+second)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{validHash, second}
	if len(cfg.EventsIngestTokenSHA256s) != 2 ||
		cfg.EventsIngestTokenSHA256s[0] != want[0] ||
		cfg.EventsIngestTokenSHA256s[1] != want[1] {
		t.Fatalf("EventsIngestTokenSHA256s = %v, want %v", cfg.EventsIngestTokenSHA256s, want)
	}
}

// TestLoadIngestHashesInvalid covers E3: malformed hash lists must fail Load
// regardless of the flag (a configured value must be valid).
func TestLoadIngestHashesInvalid(t *testing.T) {
	upper := "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		raw  string
	}{
		{"three hashes", validHash + "," + validHash + "," + validHash},
		{"non-hex char", strings.Repeat("g", 64)},
		{"too short", strings.Repeat("a", 63)},
		{"too long", strings.Repeat("a", 65)},
		{"uppercase", upper},
		// The SHA-256 of empty input is syntactically a valid lowercase 64-hex
		// digest, so it clears the regex — it must be rejected by the explicit
		// empty-input guard, not the format check.
		{"empty-input hash", emptyInputSHA256},
		// And rejected even when paired with a real hash (no smuggling it in as the
		// previous slot).
		{"empty-input as previous", validHash + "," + emptyInputSHA256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NEON_DSN", fakeDSN)
			t.Setenv("EVENTS_INGEST_ENABLED", "false")
			t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", tc.raw)

			if _, err := Load(); err == nil {
				t.Fatalf("Load: expected error for %s (%q), got nil", tc.name, tc.raw)
			}
		})
	}
}

// TestLoadIngestRejectsEmptyInputHash covers the finding-2 fix: the SHA-256 of
// empty input (produced when `printf '%s' "$TOKEN" | shasum` runs with $TOKEN
// unset) must be refused, because configuring it would authenticate a request that
// omits the X-Ingest-Token header (the verifier hashes the absent header's empty
// string to the same digest). The error message must name the empty-input reason
// so an operator can see why their pasted hash was rejected.
func TestLoadIngestRejectsEmptyInputHash(t *testing.T) {
	t.Setenv("NEON_DSN", fakeDSN)
	t.Setenv("EVENTS_INGEST_ENABLED", "true")
	t.Setenv("EVENTS_INGEST_TOKEN_SHA256S", emptyInputSHA256)

	_, err := Load()
	if err == nil {
		t.Fatal("Load: expected error for the empty-input SHA-256, got nil")
	}
	if !strings.Contains(err.Error(), "空输入") {
		t.Fatalf("error message should explain the empty-input cause, got: %v", err)
	}
}

// TestHashIngestTokenAnchor is the AC9 hash-convention anchor: a fixed sample
// token must hash to a fixed expected digest under hex(sha256(utf8_bytes(token))).
// Both values are written verbatim — the expected digest is NOT computed at
// runtime — so a convention drift (e.g. raw bytes vs utf8 text) is caught here.
// The same pair is mirrored into client-handoff §1 for the dual-end contract.
func TestHashIngestTokenAnchor(t *testing.T) {
	const sample = "sample-ingest-token-for-hash-anchoring-not-a-credential"
	const want = "828317362e384876e0e300262ec1c0e05c1d77ee3f5bf15763f647b67f64c84b"

	if got := HashIngestToken(sample); got != want {
		t.Fatalf("HashIngestToken(%q) = %q, want %q", sample, got, want)
	}
}
