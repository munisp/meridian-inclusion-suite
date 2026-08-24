// Package authx implements the prod (AUTH_MODE=keycloak) OIDC verifier:
// RS256 Bearer tokens validated against a Keycloak JWKS endpoint, per
// HARDENING.md H2. Dev mode (HS256 + X-Dev-Role) lives in httpx; services
// switch purely via the AUTH_MODE env var.
package authx

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Config holds Keycloak OIDC settings (H1 env names).
type Config struct {
	Issuer   string // KEYCLOAK_ISSUER
	Audience string // KEYCLOAK_AUDIENCE (optional: skip aud check when empty)
	JWKSURL  string // KEYCLOAK_JWKS_URL (defaults to {issuer}/protocol/openid-connect/certs)
}

// ConfigFromEnv reads the H1 Keycloak env vars.
func ConfigFromEnv() Config {
	c := Config{
		Issuer:   os.Getenv("KEYCLOAK_ISSUER"),
		Audience: os.Getenv("KEYCLOAK_AUDIENCE"),
		JWKSURL:  os.Getenv("KEYCLOAK_JWKS_URL"),
	}
	if c.JWKSURL == "" && c.Issuer != "" {
		c.JWKSURL = strings.TrimRight(c.Issuer, "/") + "/protocol/openid-connect/certs"
	}
	return c
}

// Claims is the validated token claim set.
type Claims struct {
	Subject  string   `json:"sub"`
	Issuer   string   `json:"iss"`
	Audience any      `json:"aud"`
	Expiry   int64    `json:"exp"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id"`
	Raw      map[string]any
}

type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDoc struct {
	Keys []jwksKey `json:"keys"`
}

// Verifier validates RS256 JWTs against a JWKS endpoint with a 5-minute
// cache and refresh-on-unknown-kid (H2).
type Verifier struct {
	cfg Config
	hc  *http.Client

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewVerifier builds a Verifier from ConfigFromEnv.
func NewVerifier(cfg Config) *Verifier {
	return &Verifier{cfg: cfg, hc: &http.Client{Timeout: 10 * time.Second}, keys: map[string]*rsa.PublicKey{}}
}

func (v *Verifier) fetchJWKS() error {
	if v.cfg.JWKSURL == "" {
		return errors.New("authx: KEYCLOAK_JWKS_URL/KEYCLOAK_ISSUER unset")
	}
	resp, err := v.hc.Get(v.cfg.JWKSURL)
	if err != nil {
		return fmt.Errorf("authx: jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authx: jwks fetch: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var doc jwksDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("authx: jwks parse: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, x := range eb {
			e = e<<8 | int(x)
		}
		if e == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(keys) == 0 {
		return errors.New("authx: jwks contained no usable RSA keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}

// keyFor returns the RSA key for kid, refreshing the cache when the kid is
// unknown or the cache is older than 5 minutes.
func (v *Verifier) keyFor(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	stale := time.Since(v.fetched) > 5*time.Minute
	v.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}
	if err := v.fetchJWKS(); err != nil {
		if ok { // serve stale key rather than failing outright
			return k, nil
		}
		return nil, err
	}
	v.mu.RLock()
	k, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("authx: unknown kid %q", kid)
	}
	return k, nil
}

func audMatches(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// realmRoles maps Keycloak realm_access.roles (+ resource_access.<aud>.roles)
// into the flat §1.3 `roles` claim.
func realmRoles(claims map[string]any, audience string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(v any) {
		if m, ok := v.(map[string]any); ok {
			if arr, ok := m["roles"].([]any); ok {
				for _, r := range arr {
					if s, ok := r.(string); ok && !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
				}
			}
		}
	}
	add(claims["realm_access"])
	if ra, ok := claims["resource_access"].(map[string]any); ok {
		for client, v := range ra {
			if audience == "" || client == audience {
				add(v)
			}
		}
	}
	return out
}

// Verify validates an RS256 Bearer token and returns its claims.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("authx: malformed token")
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("authx: bad header")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(head, &hdr); err != nil || hdr.Alg != "RS256" {
		return nil, errors.New("authx: unsupported token header (RS256 required)")
	}
	key, err := v.keyFor(hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("authx: bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("authx: invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("authx: bad payload encoding")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("authx: bad payload json")
	}
	c := &Claims{Raw: claims}
	if s, ok := claims["sub"].(string); ok {
		c.Subject = s
	}
	if s, ok := claims["iss"].(string); ok {
		c.Issuer = s
	}
	c.Audience = claims["aud"]
	if f, ok := claims["exp"].(float64); ok {
		c.Expiry = int64(f)
	}
	if s, ok := claims["tenant_id"].(string); ok {
		c.TenantID = s
	}
	if v.cfg.Issuer != "" && c.Issuer != v.cfg.Issuer {
		return nil, fmt.Errorf("authx: issuer mismatch")
	}
	if c.Expiry == 0 || time.Now().Unix() >= c.Expiry {
		return nil, errors.New("authx: token expired or missing exp")
	}
	if v.cfg.Audience != "" && !audMatches(c.Audience, v.cfg.Audience) {
		return nil, errors.New("authx: audience mismatch")
	}
	c.Roles = realmRoles(claims, v.cfg.Audience)
	return c, nil
}

// WriteProblem mirrors httpx.WriteProblem (kept local to avoid an import
// cycle; RFC7807 problem+json).
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status, "detail": detail,
	})
}

// ClaimsKey is the request-header the middleware stamps with the caller sub
// for downstream logging/tracing.
const ClaimsKey = "X-Meridian-Caller"

// RolesKey is the header the middleware sets to the caller's verified,
// comma-joined roles (mirrors ClaimsKey for the subject).
const RolesKey = "X-Meridian-Roles"

// identityHeaders are request headers that carry authentication/identity
// meaning inside the platform. They are ONLY ever set by this middleware
// (from a verified token) — never accepted from the client. B2 #6: strip
// them inbound on EVERY path (including public paths) so a forged
// X-Meridian-Roles / X-Dev-Role cannot reach a handler.
var identityHeaders = []string{
	ClaimsKey, RolesKey,
	"X-Dev-Role", "X-Dev-Subject", "X-Dev-Agent-Id",
}

// StripIdentityHeaders removes inbound client-supplied identity headers.
func StripIdentityHeaders(r *http.Request) {
	for _, h := range identityHeaders {
		r.Header.Del(h)
	}
}

// Middleware returns §1.3 auth middleware for AUTH_MODE=keycloak. publicPath
// bypasses auth exactly like the dev verifier.
func Middleware(v *Verifier, publicPath func(string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// B2 #6: never trust client-supplied identity headers — they are
			// re-stamped from the verified token below (or stay empty).
			StripIdentityHeaders(r)
			if publicPath != nil && publicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				WriteProblem(w, http.StatusUnauthorized, "unauthorized", "Bearer RS256 Keycloak token required (AUTH_MODE=keycloak)")
				return
			}
			claims, err := v.Verify(strings.TrimPrefix(auth, "Bearer "))
			if err != nil {
				log.Printf("authx: reject: %v", err)
				WriteProblem(w, http.StatusUnauthorized, "unauthorized", "invalid token")
				return
			}
			r.Header.Set(ClaimsKey, claims.Subject)
			// Propagate verified roles so handlers can enforce object- and
			// role-level authz (audit H-5) without re-verifying the token.
			r.Header.Set(RolesKey, strings.Join(claims.Roles, ","))
			next.ServeHTTP(w, r)
		})
	}
}
