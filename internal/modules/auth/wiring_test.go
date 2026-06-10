package auth_test

// wiring_test.go pins AC8 at the wiring level: mounting the auth module's routes
// into the real HTTP server (exactly as cmd/api/main does) must NOT put the Bearer
// gate — which performs a DB lookup — in front of /livez. The liveness probe has
// to stay reachable and DB-free even with a hostile Authorization header, so
// Neon's scale-to-zero can never make it 5xx.
//
// It builds NewServer with a NIL DB and the auth registrar: if any code path on
// /livez reached the database (e.g. a misplaced BearerMiddleware), the nil DB
// would surface as a non-200. /livez returning 200 with a garbage Bearer header
// is the proof that the auth wiring leaves the probe untouched.
//
// This is an external (_test) package test; importing internal/http here is fine
// because verify.sh's dependency-direction gate inspects the non-test build
// closure (`go list -deps`), which excludes test-only imports.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apphttp "github.com/shaomingbo/server-infra-toolkit/internal/http"
	"github.com/shaomingbo/server-infra-toolkit/internal/modules/auth"
	"github.com/shaomingbo/server-infra-toolkit/internal/platform/config"
)

// TestLivez_NotBehindBearer asserts /livez stays a 200 liveness signal after the
// auth routes are mounted, regardless of the Authorization header, with a nil DB.
func TestLivez_NotBehindBearer(t *testing.T) {
	// nil DB: NewHandler builds the auth handler over it. /livez must never touch
	// it; the login/refresh routes (which would) are not exercised here.
	authHandler := auth.NewHandler(nil)
	srv := apphttp.NewServer(&config.Config{Version: "wiring-test"}, nil, authHandler.RegisterRoutes)

	for _, authz := range []string{"", "Bearer deadbeef", "Bearer not-a-real-token", "garbage"} {
		req := httptest.NewRequest(http.MethodGet, "/livez", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req) // must not hit the (nil) DB
		if rec.Code != http.StatusOK {
			t.Fatalf("/livez with authz=%q: status = %d, want 200 (livez must not sit behind Bearer/DB)", authz, rec.Code)
		}
	}
}
