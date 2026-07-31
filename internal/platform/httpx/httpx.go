// Package httpx implements the Meridian §1.3 service conventions:
// health endpoints, RFC7807 problem+json errors, CORS and dev-mode auth.
package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/authx"
)

// Problem is an RFC7807 problem+json body.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// Healthz implements §1.3 GET /healthz.
func Healthz(service, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"service": service,
			"version": version,
		})
	}
}

// Readyz implements §1.3 GET /readyz. check may be nil.
func Readyz(check func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			if err := check(); err != nil {
				WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "error": err.Error()})
				return
			}
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// CORS allows browser PWAs to call the services in dev.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Dev-Role,X-Dev-Agent-Id,Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func devSecret() string {
	if s := os.Getenv("MERIDIAN_DEV_JWT_SECRET"); s != "" {
		return s
	}
	return "meridian-dev-secret"
}

// validateHS256 validates a dev HS256 JWT and returns its claims.
func validateHS256(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !strings.Contains(string(head), "HS256") {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(devSecret()))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac.Sum(nil), sig) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

// Auth implements §1.3 auth. When AUTH_MODE=keycloak it delegates to the
// authx RS256/JWKS verifier (H2); otherwise (AUTH_MODE=dev, default) accepts
// X-Dev-Role: admin|operator|auditor OR a Bearer HS256 dev JWT.
// Public paths (healthz/readyz and explicitly public routes) bypass it.
func Auth(publicPath func(string) bool) func(http.Handler) http.Handler {
	if os.Getenv("AUTH_MODE") == "keycloak" {
		log.Printf("profile=prod component=auth (keycloak issuer=%s)", os.Getenv("KEYCLOAK_ISSUER"))
		return authx.Middleware(authx.NewVerifier(authx.ConfigFromEnv()), publicPath)
	}
	log.Printf("profile=dev component=auth")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if publicPath != nil && publicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if role := r.Header.Get("X-Dev-Role"); role == "admin" || role == "operator" || role == "auditor" {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				if _, ok := validateHS256(strings.TrimPrefix(auth, "Bearer ")); ok {
					next.ServeHTTP(w, r)
					return
				}
			}
			WriteProblem(w, http.StatusUnauthorized, "unauthorized", "provide X-Dev-Role header or Bearer HS256 dev JWT (AUTH_MODE=dev)")
		})
	}
}

// RequestIdentity returns the authenticated caller identity for identity-
// keyed endpoints (device enrolment, commissions): the JWT `sub` claim from
// an already-validated Bearer token, or — ONLY in AUTH_MODE=dev — the
// X-Dev-Agent-Id header (dev stand-in for a per-agent principal). Returns ""
// when no identity can be established.
func RequestIdentity(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if claims, ok := validateHS256(strings.TrimPrefix(auth, "Bearer ")); ok {
			if sub, _ := claims["sub"].(string); sub != "" {
				return sub
			}
		}
	}
	if os.Getenv("AUTH_MODE") != "keycloak" {
		if id := r.Header.Get("X-Dev-Agent-Id"); id != "" {
			return id // dev-only stand-in; never honoured in prod
		}
	}
	return ""
}
