package authx

// B2 #6: the middleware must strip inbound client-supplied identity headers
// (X-Meridian-Caller/Roles, X-Dev-Role, X-Dev-Subject, X-Dev-Agent-Id) on
// EVERY path — including public paths that bypass verification — so a forged
// identity header can never reach a handler.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareStripsIdentityHeadersOnPublicPath(t *testing.T) {
	var seen http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(nil, func(string) bool { return true })(next) // all paths public
	req := httptest.NewRequest("GET", "/healthz", nil)
	for _, k := range []string{"X-Meridian-Caller", "X-Meridian-Roles",
		"X-Dev-Role", "X-Dev-Subject", "X-Dev-Agent-Id"} {
		req.Header.Set(k, "admin")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public path: got %d, want 200", rec.Code)
	}
	for _, k := range []string{"X-Meridian-Caller", "X-Meridian-Roles",
		"X-Dev-Role", "X-Dev-Subject", "X-Dev-Agent-Id"} {
		if v := seen.Get(k); v != "" {
			t.Fatalf("forged %s reached handler: %q", k, v)
		}
	}
}

func TestMiddlewareStripsIdentityHeadersBeforeAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(nil, func(string) bool { return false })(next)
	req := httptest.NewRequest("GET", "/v1/x", nil)
	req.Header.Set("X-Dev-Role", "admin")
	req.Header.Set("X-Meridian-Roles", "admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity headers without Bearer: got %d, want 401", rec.Code)
	}
}
