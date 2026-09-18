package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// R4-S1b#5 regression: the USSD gateway authenticates to onboarding/
// presumptive with the shared X-Service-Token — and FAILS CLOSED in prod
// profile when no token is configured (the old code hardcoded
// X-Dev-Role: operator unconditionally, honoured in any mode).

func TestServiceAuthTokenSent(t *testing.T) {
	t.Setenv("MERIDIAN_SERVICE_TOKEN", "tok-abc")
	var gotToken, gotName, gotDevRole string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Service-Token")
		gotName = r.Header.Get("X-Service-Name")
		gotDevRole = r.Header.Get("X-Dev-Role")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "op_1"})
	}))
	defer upstream.Close()

	if _, err := onboardingPost(upstream.URL, "/v1/operators", map[string]string{"nin": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if gotToken != "tok-abc" {
		t.Fatalf("X-Service-Token = %q, want tok-abc", gotToken)
	}
	if gotName == "" {
		t.Fatal("X-Service-Name not set")
	}
	if gotDevRole != "" {
		t.Fatalf("X-Dev-Role spoof still sent alongside the token: %q", gotDevRole)
	}
}

func TestServiceAuthFailsClosedInProd(t *testing.T) {
	// prod profile, no token configured: the call must error before any
	// request leaves the gateway — never fall back to X-Dev-Role.
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("AUTH_MODE", "keycloak")
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
	}))
	defer upstream.Close()

	if _, err := onboardingPost(upstream.URL, "/v1/operators", map[string]string{"nin": "x"}, nil); err == nil {
		t.Fatal("prod without service token must fail closed, got nil error")
	} else if !strings.Contains(err.Error(), "fail closed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if upstreamHit {
		t.Fatal("request reached upstream despite missing service token")
	}
}

func TestServiceAuthDevFallbackOnlyInDev(t *testing.T) {
	// dev profile, no token: the X-Dev-Role fallback is used (dev only).
	t.Setenv("APP_PROFILE", "dev")
	t.Setenv("AUTH_MODE", "dev")
	var gotDevRole string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDevRole = r.Header.Get("X-Dev-Role")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "op_1"})
	}))
	defer upstream.Close()
	if _, err := onboardingPost(upstream.URL, "/v1/operators", map[string]string{"nin": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if gotDevRole != "operator" {
		t.Fatalf("dev fallback X-Dev-Role = %q, want operator", gotDevRole)
	}
}
