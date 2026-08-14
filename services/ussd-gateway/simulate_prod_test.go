package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformhttpx "github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// F-3: /v1/simulate must not be reachable in prod.
// Prod profile + no credentials -> 401 from the auth middleware (the route is
// no longer auth-exempt); an authenticated prod caller -> 404 from the
// fail-closed handler. Dev profile keeps the old unauthenticated simulator.
func TestSimulateProdNoTokenUnauthorized(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	defer t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	srv := &server{graph: eng.graph, engine: eng, store: store}
	handler := platformhttpx.Auth(publicPath)(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/v1/simulate", strings.NewReader(`{"phone":"+2348011111111","inputs":["1"]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("prod + no token: expected 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSimulateProdAuthenticatedNotFound(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	defer t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	srv := &server{graph: eng.graph, engine: eng, store: store}
	handler := platformhttpx.Auth(publicPath)(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/v1/simulate", strings.NewReader(`{"phone":"+2348011111111","inputs":["1"]}`))
	req.Header.Set("X-Dev-Role", "admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("prod + authenticated: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSimulateDevStillOpen(t *testing.T) {
	t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	srv := &server{graph: eng.graph, engine: eng, store: store}
	handler := platformhttpx.Auth(publicPath)(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/v1/simulate", strings.NewReader(`{"phone":"+2348011111111","inputs":[]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev profile: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}
