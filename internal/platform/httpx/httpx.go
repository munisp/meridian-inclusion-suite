// Package httpx implements the Meridian §1.3 service conventions:
// health endpoints, RFC7807 problem+json errors, CORS and dev-mode auth.
package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
				WriteProblem(w, http.StatusServiceUnavailable, "not_ready", err.Error())
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
func allowedOrigins() map[string]bool {
	out := map[string]bool{}
	for _, o := range strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			out[o] = true
		}
	}
	return out
}

// CORS reflects allow-listed origins from CORS_ALLOWED_ORIGINS. When unset
// (default), no cross-origin access is allowed — same-origin calls do not
// need CORS at all. Credentials are only allowed for explicit origins
// (never "*").
func CORS(next http.Handler) http.Handler {
	allowed := allowedOrigins()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, traceparent, baggage")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StripIdentityHeaders removes every client-controllable header that could
// smuggle a tenant or user identity past auth. Called BEFORE any public-path
// bypass so spoofed headers never reach a handler even on unauthenticated
// routes (A-4: tenant header spoofing).
func StripIdentityHeaders(r *http.Request) {
	r.Header.Del("X-Meridian-Tenant")
	r.Header.Del("X-Tenant-ID")
	r.Header.Del("X-Dev-Tenant-Id")
	r.Header.Del("X-Meridian-User")
	r.Header.Del("X-User-ID")
	r.Header.Del("X-Forwarded-User")
}

// TenantFromRequest resolves the tenant for this request from the
// authenticated principal ONLY (authx.TenantKey context value set by
// authx.Middleware after verifying the bearer token). Client-supplied tenant
// headers are never honoured here — they were stripped upstream. When no
// authenticated tenant is present, the defaultTenant fallback is used and
// stamped, so downstream code sees a consistent, server-controlled value.
func TenantFromRequest(r *http.Request, defaultTenant string) string {
	if t, ok := r.Context().Value(authx.TenantKey).(string); ok && t != "" {
		return t
	}
	if defaultTenant == "" {
		return "default"
	}
	return defaultTenant
}

// Auth composes the platform auth middleware: strip spoofable identity
// headers, authenticate via keycloak JWKS (prod) or the dev HMAC issuer
// (dev), then stamp the server-verified tenant onto X-Meridian-Tenant for
// downstream handlers. Public paths (healthz/readyz) bypass the token check
// but still get header stripping, so a forged tenant header can never reach
// any handler.
func Auth(next http.Handler, publicPaths ...string) http.Handler {
	public := map[string]bool{}
	for _, p := range publicPaths {
		public[p] = true
	}
	authn := authx.Middleware()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StripIdentityHeaders(r)
		if public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		authn(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := TenantFromRequest(r, os.Getenv("MERIDIAN_DEFAULT_TENANT"))
			r.Header.Set("X-Meridian-Tenant", tenant)
			next.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})
}

// RequestID stamps/propagates X-Request-ID for log correlation.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = fmt.Sprintf("req-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, id)))
	})
}

type ctxKeyRequestID struct{}

// RequestIDFromContext returns the request id stamped by RequestID.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}

// Logging is a minimal access log (method, path, status, duration).
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s rid=%s", r.Method, r.URL.Path, sw.status, time.Since(start), RequestIDFromContext(r.Context()))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Shutdown installs SIGINT/SIGTERM handling that gracefully stops srv.
func Shutdown(srv *http.Server) {
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
}

// DevHMACToken mints a dev-mode HMAC token (mirrors authx dev issuer) so
// smoke tests and local tooling can call authenticated endpoints. PROD
// builds refuse to mint when MERIDIAN_ENV=prod.
func DevHMACToken(subject, tenant string) (string, error) {
	if os.Getenv("MERIDIAN_ENV") == "prod" {
		return "", fmt.Errorf("dev token minting disabled in prod")
	}
	secret := os.Getenv("DEV_AUTH_SECRET")
	if secret == "" {
		secret = "dev-only-insecure-secret"
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"sub":%q,"tenant_id":%q,"exp":%d}`, subject, tenant, time.Now().Add(time.Hour).Unix())))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig, nil
}
