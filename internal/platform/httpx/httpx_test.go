package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaxBody(t *testing.T) {
	t.Setenv("HTTPX_MAX_BODY_BYTES", "16")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if err := DecodeJSON(r, &v); err != nil {
			WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, v)
	})
	h := MaxBody(next)

	// declared oversize -> 413
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"aaaaaaaaaaaaaaaaaaaa":"b"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: got %d, want 413", rec.Code)
	}
	// within limit -> 200
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"a":1}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("within limit: got %d, want 200", rec.Code)
	}
	// streamed oversize (unknown length) -> decode error, not OOM
	req = httptest.NewRequest("POST", "/", io9Reader(`{"aaaaaaaaaaaaaaaaaaaa":"b"}`))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("streamed oversize decoded successfully")
	}
}

// io9Reader returns a body without a known Content-Length.
func io9Reader(s string) *strings.Reader { return strings.NewReader(s) }

func TestCORSOrigins(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	get := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// dev default: reflect origin (no wildcard)
	t.Setenv("PROFILE", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	if got := get(CORS(next), "http://localhost:3000").Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("dev reflect: got %q", got)
	}

	// allowlist: match allowed, others denied
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://pwa.example.gov,https://admin.example.gov")
	h := CORS(next)
	if got := get(h, "https://pwa.example.gov").Header().Get("Access-Control-Allow-Origin"); got != "https://pwa.example.gov" {
		t.Fatalf("allowlisted: got %q", got)
	}
	if got := get(h, "https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("non-allowlisted origin got ACAO %q", got)
	}

	// prod fail-closed: no origins configured -> nothing allowed
	t.Setenv("PROFILE", "prod")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	if got := get(CORS(next), "http://localhost:3000").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("prod unset origins must fail closed, got %q", got)
	}
	// wildcard in prod is ignored
	t.Setenv("CORS_ALLOWED_ORIGINS", "*")
	if got := get(CORS(next), "https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("wildcard must not be honoured in prod, got %q", got)
	}
	// preflight from denied origin -> 403
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://pwa.example.gov")
	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	CORS(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied preflight: got %d, want 403", rec.Code)
	}
}
