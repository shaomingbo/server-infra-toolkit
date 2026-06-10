package auth

// expires_unit_guard_test.go is the AST source-level half of the T3 ms↔s
// time-unit guard. The conformance test (contract_conformance_test.go) pins the
// wire's VALUE shape against the JSON Schema, but expiresAt's milliseconds vs
// seconds unit is computed inline in issueSession/rotateRefresh via time.Now()
// and cannot be driven by a fixed-time injection through the real call path
// (the time source is not injectable). So this guard reads the source directly
// and asserts that the loginSession.ExpiresAt field in BOTH issuance functions
// is built from a .UnixMilli() call — never .Unix() (seconds) or .UnixNano().
//
// It follows the same AST-guard pattern as internal/http/livez_guard_test.go.
//
// Fail-closed by construction:
//   (a) if someone changes .UnixMilli() to .Unix()/.UnixNano(), the method-name
//       assertion fails;
//   (b) the guard requires AT LEAST ONE loginSession.ExpiresAt assignment per
//       function and checks every one it finds; if the field assignment (or the
//       whole loginSession literal) disappears, the "found none" check fails —
//       a silent pass is impossible;
//   (c) the failure message names this as the wire-contract time-unit guard and
//       says that an intentional change must update the JSON Schemas and this
//       guard together.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// expiresSource locates, within a single function, the method call that produces
// the loginSession.ExpiresAt wire field. It records the selector method name
// (e.g. "UnixMilli") so the test can assert the time unit.
type expiresSource struct {
	methodName string // the Sel.Name of the X.Method() call feeding ExpiresAt
	rawValue   string // best-effort rendering of the value expr, for diagnostics
}

// wantExpiresMethod is the ONLY method allowed to feed loginSession.ExpiresAt:
// Unix milliseconds. .Unix() (seconds) and .UnixNano() would both put a
// wrong-magnitude value on the wire (off by 1000x / 1_000_000x) and silently
// break every client that reads expiresAt as ms (T2 D13/NFR7).
const wantExpiresMethod = "UnixMilli"

// findLoginSessionExpiresSource walks a function body and returns the source of
// the ExpiresAt field of every loginSession composite literal it constructs. It
// deliberately restricts to literals of type loginSession so that the unrelated
// InsertAccessTokenParams / InsertRefreshTokenParams literals in the SAME
// function (which also have an ExpiresAt field, but a pgtype.Timestamptz one)
// are never mistaken for the wire field.
func findLoginSessionExpiresSource(fn *ast.FuncDecl) []expiresSource {
	var out []expiresSource

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// Only loginSession{...} literals — identified by the type name, which is
		// an unqualified Ident in this package.
		ident, ok := cl.Type.(*ast.Ident)
		if !ok || ident.Name != "loginSession" {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "ExpiresAt" {
				continue
			}
			src := expiresSource{rawValue: renderExpr(kv.Value)}
			// The value must be a method call X.Method(); record the method name.
			if call, ok := kv.Value.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					src.methodName = sel.Sel.Name
				}
			}
			out = append(out, src)
		}
		return true
	})

	return out
}

// renderExpr is a tiny best-effort stringifier for diagnostics (not a full
// printer): enough to show "accessExpiry.UnixMilli()" in a failure message.
func renderExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.CallExpr:
		return renderExpr(v.Fun) + "()"
	case *ast.SelectorExpr:
		return renderExpr(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	default:
		return "<expr>"
	}
}

// findFunc returns the top-level FuncDecl named name from a parsed file, or nil.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// TestExpiresAtUnitGuard asserts that loginSession.ExpiresAt is produced by
// .UnixMilli() in both token-issuance functions. See the file header for why the
// wire unit cannot be verified through the real call path and must be guarded at
// the source level.
func TestExpiresAtUnitGuard(t *testing.T) {
	cases := []struct {
		file string
		fn   string
	}{
		{file: "login.go", fn: "issueSession"},
		{file: "refresh.go", fn: "rotateRefresh"},
	}

	fset := token.NewFileSet()
	for _, tc := range cases {
		file, err := parser.ParseFile(fset, tc.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.file, err)
		}
		fn := findFunc(file, tc.fn)
		if fn == nil {
			t.Fatalf("%s: function %s not found — the wire-contract time-unit guard cannot locate the token-issuance code; if it was renamed, update this guard and the JSON Schemas in contract/", tc.file, tc.fn)
		}

		sources := findLoginSessionExpiresSource(fn)
		if len(sources) == 0 {
			// Fail-closed: a missing loginSession.ExpiresAt assignment means we
			// CANNOT confirm the unit. Never pass silently.
			t.Fatalf("%s: no loginSession.ExpiresAt assignment found in %s — wire-contract time-unit guard cannot verify the unit (fail-closed); an intentional change must update this guard AND contract/*.schema.json", tc.file, tc.fn)
		}

		for _, src := range sources {
			if src.methodName != wantExpiresMethod {
				t.Errorf("%s: %s builds loginSession.ExpiresAt from %q (method %q), want a .%s() call — expiresAt is a Unix MILLISECOND wire field; .Unix() (seconds) or .UnixNano() would break the frozen client contract (T2 D13/NFR7). An intentional unit change must update contract/*.schema.json and this guard together.",
					tc.file, tc.fn, src.rawValue, src.methodName, wantExpiresMethod)
			}
		}
	}
}
