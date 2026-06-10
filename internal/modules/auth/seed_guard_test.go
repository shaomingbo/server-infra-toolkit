package auth_test

// seed_guard_test.go enforces AC19: no plaintext seed password may live in the
// source tree or in any migration. The seed credential must come only from the
// environment (SEED_USERNAME / SEED_PASSWORD) at run time. This is a structural
// guard — it greps the repository — so a future change that hardcodes a seed
// password, gives it a default, or bakes a user INSERT into a migration fails the
// build instead of silently shipping a backdoor.
//
// It lives in the external _test package and walks up to the repo root from this
// file's directory, so it does not depend on the test's working directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the current directory until it finds go.mod, returning
// the module root. The auth package sits at internal/modules/auth, so a few
// parents up is the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found walking up)")
		}
		dir = parent
	}
}

// TestSeed_ReadsFromEnv asserts the seed command sources its credentials from the
// environment and never assigns a default password. If someone replaces the env
// lookup with a literal default, this catches it.
func TestSeed_ReadsFromEnv(t *testing.T) {
	root := repoRoot(t)
	mainPath := filepath.Join(root, "cmd", "api", "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, `os.Getenv("SEED_PASSWORD")`) {
		t.Error("seed must read the password from os.Getenv(\"SEED_PASSWORD\")")
	}
	if !strings.Contains(text, `os.Getenv("SEED_USERNAME")`) {
		t.Error("seed must read the username from os.Getenv(\"SEED_USERNAME\")")
	}
	// A defaulted password (assigning a literal when the env var is empty) is the
	// exact anti-pattern AC19 forbids. Flag any literal default assignment to the
	// password variable.
	if strings.Contains(text, `password = "`) {
		t.Error("seed assigns a literal default password; AC19 forbids any hardcoded/default seed password")
	}
}

// TestNoPlaintextSeedInMigrations asserts no migration inserts a user with a
// literal password or password_hash. Seeding belongs to the run-time command
// (env-sourced), never to a committed migration.
func TestNoPlaintextSeedInMigrations(t *testing.T) {
	root := repoRoot(t)
	migDir := filepath.Join(root, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		lower := strings.ToLower(string(b))
		// An INSERT into users in a migration is the smell we forbid: it would
		// have to carry a literal password_hash (and the plaintext to produce it
		// would have lived somewhere in the repo or an operator's shell history).
		if strings.Contains(lower, "insert into users") {
			t.Errorf("%s contains INSERT INTO users; seeding must be the env-sourced run-time command, not a migration", e.Name())
		}
	}
}
