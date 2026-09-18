package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// R4-S1b#5 regression: X-Service-Token (MERIDIAN_SERVICE_TOKEN) authenticates
// service-to-service callers — constant-time, fail-closed when unconfigured.
func TestServiceTokenAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Dev-Role"); got != "operator" {
			t.Errorf("service token must stamp operator scope, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := Auth(nil)(ok)

	// 1) configured token: exact match passes and stamps operator scope.
	t.Setenv("MERIDIAN_SERVICE_TOKEN", "s3cr3t-shared-token")
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(ServiceTokenHeader, "s3cr3t-shared-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid service token rejected: %d", rec.Code)
	}

	// 2) wrong token: 401.
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(ServiceTokenHeader, "s3cr3t-shared-tokem")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong service token accepted: %d", rec.Code)
	}

	// 3) prefix/truncated token: 401 (no prefix matching).
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(ServiceTokenHeader, "s3cr3t")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("truncated service token accepted: %d", rec.Code)
	}
}

// When MERIDIAN_SERVICE_TOKEN is NOT configured, no presented token may ever
// authenticate (fail closed).
func TestServiceTokenFailClosedWhenUnconfigured(t *testing.T) {
	h := Auth(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached with unconfigured service token")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(ServiceTokenHeader, "anything-at-all")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token accepted with no configured secret: %d", rec.Code)
	}
}
