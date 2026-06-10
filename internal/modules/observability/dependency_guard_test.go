package observability_test

// dependency_guard_test.go pins AC8/NFR3 in code: the observability module must
// NOT depend on internal/http NOR internal/modules/auth. The dependency direction
// is frozen (modules never import the HTTP layer; the authentication hook lives
// only in cmd/api, so a module never imports auth), and this guard makes a
// violation fail fast under `go test` in addition to verify.sh's
// dependency-direction gate. It mirrors the `go list -deps` approach of
// internal/http/livez_guard_test.go and auth/dependency_guard_test.go.

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	observabilityPkg = "github.com/shaomingbo/server-infra-toolkit/internal/modules/observability"
	httpPkgPath      = "github.com/shaomingbo/server-infra-toolkit/internal/http"
	authPkgPath      = "github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
)

// TestObservability_NoForbiddenDependency asserts the observability package's
// production dependency closure contains neither internal/http (the error
// envelope must be rendered locally to keep the boundary one-directional) nor
// internal/modules/auth (the authentication hook is mounted only in cmd/api, so
// a module never imports another module). If a future change imports either, this
// fails immediately (AC8/NFR3).
func TestObservability_NoForbiddenDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", observabilityPkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", observabilityPkg, err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == httpPkgPath || strings.HasPrefix(dep, httpPkgPath+"/") {
			t.Fatalf("internal/modules/observability depends on %s — modules must NOT import internal/http (AC8/NFR3)", dep)
		}
		if dep == authPkgPath || strings.HasPrefix(dep, authPkgPath+"/") {
			t.Fatalf("internal/modules/observability depends on %s — modules must NOT import internal/modules/auth (auth hook lives only in cmd/api; AC8/NFR3)", dep)
		}
	}
}
