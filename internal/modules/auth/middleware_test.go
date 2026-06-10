package auth

// middleware_test.go covers the DB-INDEPENDENT Bearer paths: a request that fails
// before any token lookup (missing header, wrong scheme, undecodable token) must
// be a 401 without ever touching the database, and a route NOT wrapped by the
// middleware must pass through untouched. The DB-backed paths (valid / revoked /
// expired / disabled token) are exercised by the TEST_DATABASE_URL-gated
// integration test, since they require a real Postgres.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bearerHandlerNoDB builds a Handler whose q is nil. That is safe ONLY for the
// pre-lookup failure cases below: each returns 401 before authenticate calls
// h.q.GetAccessToken, so the nil queries are never dereferenced. Any case that
// reached the DB would panic, which is itself a useful assertion that these paths
// short-circuit.
func bearerHandlerNoDB() *Handler {
	return &Handler{}
}

// okHandler is the downstream handler the middleware guards; it records that it
// ran and writes 200. UserIDFromContext is read so a passing test can assert the
// authenticated id is attached (used by the integration test; here it is empty on
// the pass-through route).
func okHandler(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		_, _ = w.Write([]byte("ok:" + UserIDFromContext(r.Context())))
	})
}

// serveBearer wraps okHandler with the middleware, sets the request-id header the
// way the middleware chain would, applies authz (when non-empty), and returns the
// recorder plus whether the downstream handler ran.
func serveBearer(h *Handler, authz string) (*httptest.ResponseRecorder, bool) {
	var ran bool
	mw := h.BearerMiddleware(okHandler(&ran))
	req := httptest.NewRequest(http.MethodGet, "/v1/protected", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set(requestIDHeader, "req-test-bearer")
	mw.ServeHTTP(rec, req)
	return rec, ran
}

// TestBearer_PreLookupRejections (FR5) asserts that a missing header, a wrong
// scheme, an empty token, and an undecodable token each yield a 401 WITHOUT
// reaching the downstream handler and WITHOUT touching the DB (nil queries would
// panic if a path tried to look the token up).
func TestBearer_PreLookupRejections(t *testing.T) {
	h := bearerHandlerNoDB()

	cases := map[string]string{
		"missing header":       "",
		"wrong scheme":         "Basic abc123",
		"lowercase scheme":     "bearer abc123",
		"scheme only":          "Bearer ",
		"scheme only no space": "Bearer",
		"undecodable token":    "Bearer not base64!! padding==",
	}
	for name, authz := range cases {
		t.Run(name, func(t *testing.T) {
			rec, ran := serveBearer(h, authz) // must not panic, must not hit DB
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401; body=%s", name, rec.Code, rec.Body.String())
			}
			if ran {
				t.Fatalf("%s: downstream handler ran on a rejected request", name)
			}
			// The body must be the single generic unauthorized envelope, not a hint.
			if !strings.Contains(rec.Body.String(), unauthorizedMessage) {
				t.Fatalf("%s: 401 body is not the generic message: %s", name, rec.Body.String())
			}
		})
	}
}

// TestBearer_PassThroughRouteUnaffected (AC8) asserts a route that is NOT wrapped
// by BearerMiddleware serves normally regardless of the Authorization header — the
// middleware only guards routes it is explicitly mounted on. This mirrors how
// /livez (never wrapped) stays reachable.
func TestBearer_PassThroughRouteUnaffected(t *testing.T) {
	var ran bool
	unguarded := okHandler(&ran)

	for _, authz := range []string{"", "garbage", "Bearer nonsense"} {
		ran = false
		req := httptest.NewRequest(http.MethodGet, "/livez", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		unguarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unguarded route with authz=%q: status = %d, want 200", authz, rec.Code)
		}
		if !ran {
			t.Fatalf("unguarded route with authz=%q: handler did not run", authz)
		}
	}
}

// TestUserIDFromContext_Empty asserts the accessor returns "" for a context that
// never passed through the middleware (no panic, no spurious value).
func TestUserIDFromContext_Empty(t *testing.T) {
	if got := UserIDFromContext(context.Background()); got != "" {
		t.Fatalf("UserIDFromContext(empty) = %q, want \"\"", got)
	}
}
