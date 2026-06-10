package auth_test

// dependency_guard_test.go pins AC17/D10 in code: the auth module — and therefore
// the Bearer middleware it exports — must NOT depend on internal/http. The
// dependency direction is frozen (modules never import the HTTP layer; main is the
// only wiring point), and this guard makes a violation fail fast under `go test`
// in addition to verify.sh's dependency-direction gate. It mirrors the
// `go list -deps` approach of internal/http/livez_guard_test.go.

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	authPkg     = "github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
	httpPkgPath = "github.com/shaomingbo/server-infra-toolkit/internal/http"
)

// TestAuth_NoHTTPDependency asserts the auth package's dependency closure does not
// contain internal/http. If a future change imports the HTTP layer into auth (e.g.
// to reuse apphttp.WriteError instead of the local writeError), this fails
// immediately — the error envelope must be emitted locally to keep the boundary
// one-directional.
func TestAuth_NoHTTPDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", authPkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", authPkg, err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == httpPkgPath || strings.HasPrefix(dep, httpPkgPath+"/") {
			t.Fatalf("internal/modules/auth depends on %s — modules must NOT import internal/http (AC17/D10)", dep)
		}
	}
}
