package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A1-10 regression: PROFILE=prod must never serve forgeable dev auth
// (AUTH_MODE=dev X-Dev-Role / default MERIDIAN_DEV_JWT_SECRET). Pre-fix,
// prod happily honoured X-Dev-Role.
func TestProdRefusesDevAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	// prod + AUTH_MODE=dev: fail closed even for a presented X-Dev-Role.
	t.Setenv("PROFILE", "prod")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("MERIDIAN_DEV_JWT_SECRET", "strong-prod-secret-0123456789abcdef")
	h := Auth(nil)(next)
	req := httptest.NewRequest("GET", "/v1/x", nil)
	req.Header.Set("X-Dev-Role", "admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("prod AUTH_MODE=dev must fail closed, got %d", rec.Code)
	}

	// prod + keycloak mode but the default dev secret still configured:
	// fail closed (defense in depth for RequestIdentity's HS256 path).
	t.Setenv("AUTH_MODE", "keycloak")
	t.Setenv("KEYCLOAK_ISSUER", "https://idp.example/realms/m")
	t.Setenv("MERIDIAN_DEV_JWT_SECRET", "")
	h = Auth(nil)(next)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("prod with default dev secret must fail closed, got %d", rec.Code)
	}

	// dev profile keeps working.
	t.Setenv("PROFILE", "dev")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("MERIDIAN_DEV_JWT_SECRET", "")
	h = Auth(nil)(next)
	req = httptest.NewRequest("GET", "/v1/x", nil)
	req.Header.Set("X-Dev-Role", "admin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dev X-Dev-Role must still work, got %d", rec.Code)
	}
}
