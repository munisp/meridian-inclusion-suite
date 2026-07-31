package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestVNINFormatAndTTL covers the 16-char token shape and 72h TTL.
func TestVNINFormatAndTTL(t *testing.T) {
	if !VNINValid("AB012345678910YZ") || !VNINValid("ab012345678910yz") {
		t.Fatal("valid vNIN rejected")
	}
	for _, bad := range []string{"", "AB01234567891YZ", "AB0123456789101YZ", "12012345678910YZ"} {
		if VNINValid(bad) {
			t.Fatalf("invalid vNIN accepted: %q", bad)
		}
	}
	if VNINExpired(time.Now().Add(-71 * time.Hour)) {
		t.Fatal("fresh token reported expired")
	}
	if !VNINExpired(time.Now().Add(-73 * time.Hour)) {
		t.Fatal("73h-old token not reported expired")
	}
}

// TestNIMCSimulatorVNIN covers the deterministic vNIN personas.
func TestNIMCSimulatorVNIN(t *testing.T) {
	sim := NIMCSimulator{}
	v, err := sim.VerifyVNIN("AB012345678910YZ", time.Now().Add(-time.Hour))
	if err != nil || !v.Verified || v.CredentialType != "vnin" || v.Source != "simulator" {
		t.Fatalf("unexpected sim result: %+v err=%v", v, err)
	}
	if strings.Contains(v.Detail, "AB012345678910YZ") {
		t.Fatal("raw token leaked into detail")
	}
	exp, _ := sim.VerifyVNIN("CD012345678910YZ", time.Now().Add(-80*time.Hour))
	if exp.Verified || !strings.Contains(exp.Detail, "expired") {
		t.Fatalf("expected expired outcome: %+v", exp)
	}
	expP, _ := sim.VerifyVNIN("EX012345678910YZ", time.Now())
	if expP.Verified || !strings.Contains(expP.Detail, "expired") {
		t.Fatalf("expected EX persona expired: %+v", expP)
	}
	nf, _ := sim.VerifyVNIN("NF012345678910YZ", time.Now())
	if nf.Verified || !strings.Contains(nf.Detail, "not found") {
		t.Fatalf("expected NF persona miss: %+v", nf)
	}
	bad, _ := sim.VerifyVNIN("nope", time.Now())
	if bad.Verified {
		t.Fatal("invalid format verified")
	}
	// legacy raw-NIN path still works and is tagged nin_legacy
	leg, _ := sim.VerifyNIN("12345678901")
	if leg.CredentialType != "nin_legacy" {
		t.Fatalf("legacy path mistagged: %+v", leg)
	}
}

// TestNIMCHTTPAdapterVNIN verifies the real rail contract: distinct outcome
// codes map to Verified:false results (not retries), success maps through.
func TestNIMCHTTPAdapterVNIN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/verify/vnin" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.HasPrefix(req["vnin"], "NF"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(req["vnin"], "EX"):
			w.WriteHeader(http.StatusGone)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verified": true, "first_name": "Ada", "last_name": "Okafor",
			})
		}
	}))
	defer srv.Close()

	a := NewNIMCHTTPAdapter(srv.URL, "k3y")
	ok, err := a.VerifyVNIN("AB012345678910YZ", time.Now().Add(-time.Hour))
	if err != nil || !ok.Verified || ok.FirstName != "Ada" || ok.CredentialType != "vnin" {
		t.Fatalf("unexpected: %+v err=%v", ok, err)
	}
	nf, err := a.VerifyVNIN("NF012345678910YZ", time.Now())
	if err != nil || nf.Verified || !strings.Contains(nf.Detail, "not found") {
		t.Fatalf("404 must be a result, not an error: %+v err=%v", nf, err)
	}
	ex, err := a.VerifyVNIN("EX012345678910YZ", time.Now())
	if err != nil || ex.Verified || !strings.Contains(ex.Detail, "expired") {
		t.Fatalf("410 must map to expired result: %+v err=%v", ex, err)
	}
	// client-side TTL short-circuit: never hits the server
	stale, err := a.VerifyVNIN("AB012345678910YZ", time.Now().Add(-100*time.Hour))
	if err != nil || stale.Verified || !strings.Contains(stale.Detail, "expired") {
		t.Fatalf("TTL short-circuit: %+v err=%v", stale, err)
	}
}

// TestNIMCHTTPAdapterNINNotFound: a 404 on the legacy path is a verification
// outcome (Verified:false), not a retryable rail failure.
func TestNIMCHTTPAdapterNINNotFound(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := NewNIMCHTTPAdapter(srv.URL, "k")
	v, err := a.VerifyNIN("12345678901")
	if err != nil {
		t.Fatalf("404 must not error: %v", err)
	}
	if v.Verified || !strings.Contains(v.Detail, "not found") {
		t.Fatalf("unexpected: %+v", v)
	}
	if calls != 1 {
		t.Fatalf("404 burned retries: %d calls", calls)
	}
}
