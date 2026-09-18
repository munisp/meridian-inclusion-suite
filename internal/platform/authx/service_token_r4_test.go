package authx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// R4-S1b#5 regression: in keycloak mode the shared service token
// authenticates service-to-service callers (stamped as service:<name> with
// operator scope) — never when unconfigured, never with a wrong token.
func TestMiddlewareServiceToken(t *testing.T) {
	v := NewVerifier(Config{})
	seen := make(chan [2]string, 2)
	h := Middleware(v, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- [2]string{r.Header.Get(ClaimsKey), r.Header.Get(RolesKey)}
		w.WriteHeader(http.StatusOK)
	}))

	t.Setenv("MERIDIAN_SERVICE_TOKEN", "shared-token-xyz")

	// 1) valid token, no JWT: passes with the service identity stamped.
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/intent", nil)
	req.Header.Set("X-Service-Token", "shared-token-xyz")
	req.Header.Set("X-Service-Name", "ussd-gateway")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid service token rejected in keycloak mode: %d", rec.Code)
	}
	id := <-seen
	if id[0] != "service:ussd-gateway" || id[1] != "operator" {
		t.Fatalf("stamped identity = %v, want [service:ussd-gateway operator]", id)
	}

	// 2) wrong token without a JWT: 401.
	req = httptest.NewRequest(http.MethodPost, "/v1/payments/intent", nil)
	req.Header.Set("X-Service-Token", "shared-token-xyy")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong service token accepted: %d", rec.Code)
	}
}

// Unconfigured token: any presented token must 401 (fail closed).
func TestMiddlewareServiceTokenUnconfigured(t *testing.T) {
	v := NewVerifier(Config{})
	h := Middleware(v, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set("X-Service-Token", "anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("service token accepted with no configured secret: %d", rec.Code)
	}
}
