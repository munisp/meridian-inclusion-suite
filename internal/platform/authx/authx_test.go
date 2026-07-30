package authx

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// makeToken signs an RS256 token with the given claims.
func makeToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	body := b64(head) + "." + b64(payload)
	digest := sha256.Sum256([]byte(body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return body + "." + b64(sig)
}

func jwksServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

func TestVerifyRS256(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &key.PublicKey, "k1")
	defer srv.Close()

	v := NewVerifier(Config{Issuer: "https://kc/realms/meridian", Audience: "meridian-services", JWKSURL: srv.URL})
	claims := map[string]any{
		"sub": "agent-1", "iss": "https://kc/realms/meridian",
		"aud": "meridian-services", "exp": time.Now().Add(time.Hour).Unix(),
		"realm_access": map[string]any{"roles": []any{"agent", "operator"}},
	}
	tok := makeToken(t, key, "k1", claims)
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Subject != "agent-1" {
		t.Fatalf("sub = %q", c.Subject)
	}
	if len(c.Roles) != 2 || c.Roles[0] != "agent" {
		t.Fatalf("roles = %v", c.Roles)
	}
}

func TestVerifyRejects(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &key.PublicKey, "k1")
	defer srv.Close()
	v := NewVerifier(Config{Issuer: "https://kc/realms/meridian", Audience: "meridian-services", JWKSURL: srv.URL})
	base := map[string]any{
		"sub": "a", "iss": "https://kc/realms/meridian",
		"aud": "meridian-services", "exp": time.Now().Add(time.Hour).Unix(),
	}

	// wrong signing key
	bad := makeToken(t, other, "k1", base)
	if _, err := v.Verify(bad); err == nil {
		t.Fatal("expected signature failure")
	}
	// expired
	expired := map[string]any{}
	for k, val := range base {
		expired[k] = val
	}
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, err := v.Verify(makeToken(t, key, "k1", expired)); err == nil {
		t.Fatal("expected expiry failure")
	}
	// wrong audience
	wrongAud := map[string]any{}
	for k, val := range base {
		wrongAud[k] = val
	}
	wrongAud["aud"] = "someone-else"
	if _, err := v.Verify(makeToken(t, key, "k1", wrongAud)); err == nil {
		t.Fatal("expected audience failure")
	}
	// unknown kid
	if _, err := v.Verify(makeToken(t, key, "nope", base)); err == nil {
		t.Fatal("expected unknown kid failure")
	}
}

func TestMiddlewareKeycloakFlow(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &key.PublicKey, "k1")
	defer srv.Close()
	v := NewVerifier(Config{JWKSURL: srv.URL}) // no iss/aud pinning

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/x" && r.Header.Get(ClaimsKey) != "u1" {
			t.Errorf("caller header = %q", r.Header.Get(ClaimsKey))
		}
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(v, func(p string) bool { return p == "/healthz" })(ok)

	// public path bypasses
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public path: %d", rec.Code)
	}
	// no token -> 401
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	// valid token -> 200
	tok := makeToken(t, key, "k1", map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
	req := httptest.NewRequest("GET", "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: %d", rec.Code)
	}
}
