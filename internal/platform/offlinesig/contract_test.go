package offlinesig

import (
	"path"
	"testing"
)

// pinManifestFile is the pin manifest's own filename. It is the one embedded
// contract artifact that is NOT self-pinned (a file cannot record its own hash
// without a chicken-and-egg loop), so the coverage test below excludes it.
const pinManifestFile = "offline-package-v2-contract-pin.json"

// TestPinGuard_EmbeddedMatchesManifest asserts the fail-closed pin is live: each
// file recorded in the embedded pin manifest is itself embedded and hashes to
// the recorded sha256. Package init already panics on mismatch (so a tampered
// vendored file turns the whole package red), but this test pins that invariant
// explicitly and surfaces a readable diff if a hash drifts.
func TestPinGuard_EmbeddedMatchesManifest(t *testing.T) {
	if len(pin.Files) == 0 {
		t.Fatal("pin manifest records no files")
	}
	for name, entry := range pin.Files {
		got, err := embeddedSHA256("contract/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if got != entry.SHA256 {
			t.Errorf("sha256 mismatch for %s:\n  embedded=%s\n  pinned  =%s", name, got, entry.SHA256)
		}
	}
}

// TestPinGuard_EveryEmbeddedArtifactIsPinned is the reverse direction of the pin
// invariant: every *.json / *.md file embedded under contract/ (except the pin
// manifest itself, which cannot self-pin) MUST appear in pin.Files. Without this,
// a new artifact could be embedded — and become a trusted oracle — while slipping
// past the sha256 guard because the guard only iterates files the manifest already
// lists. This test fails fast on any embedded-but-unpinned artifact (e.g. the
// interop oracle verify-extended-cases.json must be pinned, not just the vectors).
func TestPinGuard_EveryEmbeddedArtifactIsPinned(t *testing.T) {
	entries, err := contractFS.ReadDir("contract")
	if err != nil {
		t.Fatalf("read embedded contract/ dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := path.Ext(name)
		if ext != ".json" && ext != ".md" {
			continue
		}
		if name == pinManifestFile {
			continue
		}
		if _, ok := pin.Files[name]; !ok {
			t.Errorf("embedded contract artifact %q is not recorded in the pin manifest; "+
				"every embedded *.json/*.md (except %s) must be sha256-pinned",
				name, pinManifestFile)
		}
	}
}
