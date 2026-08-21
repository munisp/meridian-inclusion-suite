// Package httpx implements the Meridian §1.3 service conventions:
// health endpoints, RFC7807 problem+json errors, CORS and dev-mode auth.
package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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
	// M-6: never decode unbounded bodies — cap at the configured limit even
	// when the MaxBody middleware is not in the chain.
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes()+1))
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

// DefaultMaxBodyBytes is the default request-body cap (2 MiB); override
// with HTTPX_MAX_BODY_BYTES (audit M-6: unbounded body reads = memory-DoS).
const DefaultMaxBodyBytes = 2 << 20

func maxBodyBytes() int64 {
	if v := os.Getenv("HTTPX_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxBodyBytes
}

// MaxBody caps request bodies (Content-Length and streamed reads) at
// HTTPX_MAX_BODY_BYTES (default 2 MiB) and answers 413 RFC7807 when the
// declared length exceeds the cap; oversized streamed bodies error out of
// DecodeJSON with a 400/413 from the handler. Apply it outermost, before
// auth, so unauthenticated junk can't consume memory either.
func MaxBody(next http.Handler) http.Handler {
	limit := maxBodyBytes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			WriteProblem(w, http.StatusRequestEntityTooLarge, "body_too_large",
				"request body exceeds the configured limit")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigins parses CORS_ALLOWED_ORIGINS (comma-separated).
// Audit M-7: wildcard `*` together with credentialed headers
// (Authorization) is forbidden. Semantics:
//   - unset: dev default — reflect the request Origin (dev PWAs on random
//     localhost ports); in PROFILE=prod this fails closed (deny all).
//   - set: exact-match allowlist; "*" is honoured only outside prod.
func allowedOrigins() []string {
	v := os.Getenv("CORS_ALLOWED_ORIGINS")
	if v == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(v, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func prodProfile() bool {
	p := os.Getenv("PROFILE")
	return p == "prod" || p == "production"
}

// CORS applies the §1.3 CORS policy (audit M-7): origins come from
// CORS_ALLOWED_ORIGINS; no wildcard with Authorization in prod; prod with
// no configured origins denies cross-origin browser calls entirely.
func CORS(next http.Handler) http.Handler {
	origins := allowedOrigins()
	if prodProfile() && len(origins) == 0 {
		log.Printf("profile=prod component=cors CORS_ALLOWED_ORIGINS unset: FAILING CLOSED (no cross-origin browser access)")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := ""
		switch {
		case len(origins) > 0:
			for _, o := range origins {
				if o == "*" && !prodProfile() {
					allow = "*"
					break
				}
				if o == origin && origin != "" {
					allow = origin
					break
				}
			}
		case !prodProfile() && origin != "":
			allow = origin // dev convenience: reflect localhost dev origins
		}
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,PUT,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Dev-Role,X-Dev-Agent-Id,Idempotency-Key")
		}
		if r.Method == http.MethodOptions {
			if allow == "" && origin != "" {
				WriteProblem(w, http.StatusForbidden, "cors_denied", "origin not allowed")
				return
			}
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

// ProdAuthMisconfigured reports whether PROFILE=prod is combined with
// forgeable dev authentication: AUTH_MODE != keycloak, or the dev JWT
// secret missing/still the built-in default ("meridian-dev-secret").
// Callers must fail closed when this is true.
func ProdAuthMisconfigured() bool {
	if os.Getenv("PROFILE") != "prod" {
		return false
	}
	if os.Getenv("AUTH_MODE") != "keycloak" {
		return true
	}
	return devSecret() == "meridian-dev-secret"
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
	// A1-10: PROFILE=prod with dev-mode auth or the default/missing dev
	// secret must never serve — both are fully forgeable. Fail closed.
	if ProdAuthMisconfigured() {
		log.Printf("profile=prod component=auth FAIL-CLOSED: PROFILE=prod with dev AUTH_MODE or default/missing MERIDIAN_DEV_JWT_SECRET; all requests denied")
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				WriteProblem(w, http.StatusServiceUnavailable, "auth_misconfigured",
					"PROFILE=prod refuses dev auth / default JWT secret; refusing all requests (fail closed)")
			})
		}
	}
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

// CallerIdentity returns the authenticated caller's subject across both
// auth modes: in keycloak mode the authx middleware propagates the verified
// subject via the X-Meridian-Caller header; otherwise it falls back to
// RequestIdentity (dev JWT sub / X-Dev-Agent-Id).
func CallerIdentity(r *http.Request) string {
	if sub := r.Header.Get("X-Meridian-Caller"); sub != "" {
		return sub
	}
	return RequestIdentity(r)
}

// RequestRoles returns the authenticated caller's roles across both auth
// modes (audit H-5: object-level authz needs role visibility inside
// handlers). In keycloak mode the authx middleware propagates the verified
// roles via the X-Meridian-Roles header (comma-joined); in dev mode the
// X-Dev-Role header and the HS256 Bearer `roles` claim are honoured.
func RequestRoles(r *http.Request) []string {
	if h := r.Header.Get("X-Meridian-Roles"); h != "" {
		return strings.Split(h, ",")
	}
	if os.Getenv("AUTH_MODE") == "keycloak" {
		return nil
	}
	var roles []string
	if dr := r.Header.Get("X-Dev-Role"); dr != "" {
		roles = append(roles, dr)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if claims, ok := validateHS256(strings.TrimPrefix(auth, "Bearer ")); ok {
			if arr, ok := claims["roles"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						roles = append(roles, s)
					}
				}
			}
		}
	}
	return roles
}

// HasRole reports whether the caller holds the given role.
func HasRole(r *http.Request, role string) bool {
	for _, got := range RequestRoles(r) {
		if got == role {
			return true
		}
	}
	return false
}
