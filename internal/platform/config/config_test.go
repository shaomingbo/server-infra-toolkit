package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const fakeDSN = "postgres://user:p4ssw0rd-SECRET@ep-fake.neon.tech/db?sslmode=require"

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
	t.Setenv("PORT", "8080")
	t.Setenv("NEON_DSN", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load: expected error when NEON_DSN unset, got nil")
	}
}
