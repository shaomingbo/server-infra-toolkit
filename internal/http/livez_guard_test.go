package http

// livez_guard_test.go pins AC11/AC16 in code: the /livez liveness path (and the
// whole internal/http package) must NOT depend on the database package, and must
// not import db/pgxpool or call Ping in source. This guards the invariant that
// Neon's scale-to-zero can never make /livez fail (NG4/NFR5): a liveness probe
// that touched the DB would 5xx whenever Neon is asleep, and Cloud Run would
// kill the instance. verify.sh enforces dependency direction for the build gate;
// this test makes the same guarantee fail fast under `go test`.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// runGoListDeps returns the full dependency closure of pkg as reported by
// `go list -deps`. It is the same mechanism verify.sh uses for the
// dependency-direction gate, run here so AC11 also fails fast under `go test`.
//
// It uses CombinedOutput so that on failure the returned string carries the
// toolchain/module error from stderr (a missing module, a build error, etc.)
// instead of an empty body, making the test diagnosable.
func runGoListDeps(t *testing.T, pkg string) (string, error) {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	return string(out), err
}

const (
	httpPkg = "github.com/shaomingbo/server-infra-toolkit/internal/http"
	dbPkg   = "github.com/shaomingbo/server-infra-toolkit/internal/platform/db"
)

// TestLivez_NoDBDependency asserts the internal/http dependency closure does not
// contain the db package (AC11: `go list -deps` carries no db dependency). If a
// future change imports the pool into the HTTP layer (e.g. to ping it in /livez)
// this fails immediately.
func TestLivez_NoDBDependency(t *testing.T) {
	out, err := runGoListDeps(t, httpPkg)
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", httpPkg, err, out)
	}
	for _, dep := range strings.Fields(out) {
		if dep == dbPkg || strings.HasPrefix(dep, dbPkg+"/") {
			t.Fatalf("internal/http depends on %s — /livez must carry no DB dependency (AC11)", dep)
		}
	}
}

// TestLivez_NoDBImportsOrPing parses the internal/http source files (AST, so
// comments are ignored — the package legitimately mentions db/pgxpool in
// doc comments) and asserts no file imports the db package or pgxpool, and no
// code calls a .Ping(...) method. This is the AC16 "grep-clean of db/pool import
// or Ping" guarantee, made robust against false positives in prose.
func TestLivez_NoDBImportsOrPing(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		// Exclude this guard file: it intentionally names dbPkg as a constant.
		return fi.Name() != "livez_guard_test.go"
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	forbiddenImports := []string{
		"internal/platform/db",
		"jackc/pgx/v5/pgxpool",
	}

	for _, pkg := range pkgs {
		for fname, file := range pkg.Files {
			// 1. No forbidden imports.
			for _, imp := range file.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range forbiddenImports {
					if path == bad || strings.HasSuffix(path, bad) {
						t.Fatalf("%s imports %q — the HTTP layer must not touch the DB on the liveness path (AC11/AC16)", fname, path)
					}
				}
			}
			// 2. No .Ping( call anywhere in the HTTP layer.
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Ping" {
					t.Fatalf("%s calls a .Ping method — /livez must not probe the DB (AC16/NG4)", fname)
				}
				return true
			})
		}
	}
}
