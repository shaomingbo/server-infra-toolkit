package offlinesig

// deps_guard_test.go pins AC14 (NFR3 / the frozen dependency direction) in code:
// internal/platform/offlinesig lives under internal/platform/*, so its full
// dependency closure must NOT contain internal/http or internal/modules. The
// builder is a pure, no-secret-environment-reusable function; pulling in an HTTP
// or modules dependency would both break the frozen platform->http/modules
// direction (verify.sh step 4 enforces it for the build) and quietly couple the
// signer to the request path. This test makes the same guarantee fail fast under
// `go test`, the same `go list -deps` mechanism verify.sh uses.

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	offlinesigPkg  = "github.com/shaomingbo/server-infra-toolkit/internal/platform/offlinesig"
	httpPkgPrefix  = "github.com/shaomingbo/server-infra-toolkit/internal/http"
	modulesPkgPref = "github.com/shaomingbo/server-infra-toolkit/internal/modules"
)

// TestOfflinesig_NoHTTPOrModulesDependency asserts the offlinesig dependency
// closure contains neither internal/http nor internal/modules (AC14). If a future
// change imports either into the signer, this fails immediately instead of only
// at the verify.sh dependency-direction gate.
func TestOfflinesig_NoHTTPOrModulesDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", offlinesigPkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", offlinesigPkg, err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == httpPkgPrefix || strings.HasPrefix(dep, httpPkgPrefix+"/") {
			t.Fatalf("offlinesig depends on %s — internal/platform/* must not import internal/http (AC14)", dep)
		}
		if dep == modulesPkgPref || strings.HasPrefix(dep, modulesPkgPref+"/") {
			t.Fatalf("offlinesig depends on %s — internal/platform/* must not import internal/modules (AC14)", dep)
		}
	}
}
