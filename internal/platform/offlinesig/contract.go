package offlinesig

// contract.go embeds the vendored v2-signature contract artifacts into the
// binary and enforces a fail-closed sha256 pin at package init. The pin guards
// against LOCAL tampering of the vendored copies: if a pinned file's bytes no
// longer hash to the value recorded in the pin manifest (or a pinned file is
// missing), init panics, which turns `go test` / the build red rather than
// silently signing against a drifted contract. It does NOT detect upstream
// divergence (the app repo changing the spec without a server-side pin bump) —
// that is covered by the NEEDS-SERVER-BUMP cross-repo discipline, not by this
// gate.
//
// Mirrors the fail-closed init pattern in
// internal/modules/observability/schema.go: embedded artifacts compiled/checked
// ONCE at init; any failure is a startup error, never a silent skip.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// contractFS embeds the vendored contract document, golden vectors, interop
// verify cases, and the pin manifest. go:embed compiles them into the binary,
// so deleting any file turns the build red (fail-closed) instead of degrading
// to a runtime read-from-disk that could silently skip the pin check.
//
//go:embed contract/v2-signature-contract.md
//go:embed contract/canonical-payload-vectors.json
//go:embed contract/verify-extended-cases.json
//go:embed contract/offline-package-v2-contract-pin.json
var contractFS embed.FS

// pinManifest is the parsed shape of offline-package-v2-contract-pin.json. Only
// the fields the pin guard consumes are modeled; extra manifest fields
// (source_repo, source_commit, pinned_at, contractVersion) are recorded for
// provenance/review and intentionally ignored here.
type pinManifest struct {
	Files map[string]struct {
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// pin is the parsed contract pin manifest. It is loaded and validated once at
// init; the package-level handle lets the pin-guard test re-assert the
// invariant without re-reading the manifest.
var pin pinManifest

func init() {
	raw, err := contractFS.ReadFile("contract/offline-package-v2-contract-pin.json")
	if err != nil {
		panic(fmt.Sprintf("offlinesig: read pin manifest: %v", err))
	}

	if err := json.Unmarshal(raw, &pin); err != nil {
		panic(fmt.Sprintf("offlinesig: decode pin manifest: %v", err))
	}
	if len(pin.Files) == 0 {
		panic("offlinesig: pin manifest records no files")
	}

	// Every file the manifest pins MUST be embedded and hash to the recorded
	// value. A mismatch or a missing file is fail-closed (panic), so tampering
	// with a vendored artifact cannot pass the build/test.
	for name, entry := range pin.Files {
		got, err := embeddedSHA256("contract/" + name)
		if err != nil {
			panic(fmt.Sprintf("offlinesig: pin file %s: %v", name, err))
		}
		if got != entry.SHA256 {
			panic(fmt.Sprintf(
				"offlinesig: pin sha256 mismatch for %s: embedded=%s pinned=%s",
				name, got, entry.SHA256,
			))
		}
	}
}

// embeddedSHA256 returns the lower-hex sha256 of an embedded file's bytes.
func embeddedSHA256(path string) (string, error) {
	b, err := contractFS.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
