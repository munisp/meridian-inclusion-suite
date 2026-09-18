package authx

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// R4-S3#1 regression: X-Meridian-Tenant must be stripped inbound and
// re-stamped ONLY from the verified JWT tenant_id claim. Before the fix the
// header passed through untouched, so an authenticated tenant-A operator
// could assert tenant-B and manage/accrue commissions cross-tenant.
func TestMiddlewareStripsAndStampsTenant(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &key.PublicKey, "k1")
	defer srv.Close()
	v := NewVerifier(Config{JWKSURL: srv.URL})

	seen := make(chan string, 4)
	h := Middleware(v, func(p string) bool { return p == "/healthz" })(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen <- r.Header.Get(TenantKey)
			w.WriteHeader(http.StatusOK)
		}))

	// 1) Authenticated tenant-A caller forging tenant-B: the downstream
	//    handler must see tenant-A, never the forged value.
	tok := makeToken(t, key, "k1", map[string]any{
		"sub": "op-a", "tenant_id": "tenant-a",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(TenantKey, "tenant-b")
	req.Header.Set("X-Tenant-ID", "tenant-b")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got != "tenant-a" {
		t.Fatalf("forged tenant passed through: downstream saw %q, want tenant-a", got)
	}

	// 2) Token without a tenant claim + forged header: downstream sees ""
	//    (handler falls back to the default tenant), never the forged value.
	tokNoTenant := makeToken(t, key, "k1", map[string]any{
		"sub": "op-none", "exp": time.Now().Add(time.Hour).Unix(),
	})
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tokNoTenant)
	req.Header.Set(TenantKey, "tenant-b")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got != "" {
		t.Fatalf("tenant header not stripped for claimless token: saw %q", got)
	}

	// 3) Public path: stripped too (a forged tenant must never reach any
	//    handler, authenticated route or not).
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(TenantKey, "tenant-b")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got != "" {
		t.Fatalf("tenant header reached public path: saw %q", got)
	}

	// 4) Unauthenticated request is rejected before any header matters.
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set(TenantKey, "tenant-b")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
}
