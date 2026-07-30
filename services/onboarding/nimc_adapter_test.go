package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestNIMCHTTPAdapter verifies HMAC signing, response mapping, and retry
// against a fake NIMC endpoint.
func TestNIMCHTTPAdapter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/verify" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if got := r.Header.Get("X-NIMC-Signature"); got != hmacSHA256Hex("k3y", string(body)) {
			t.Errorf("bad signature header")
		}
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		if req["nin"] == "" {
			t.Errorf("missing nin")
		}
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway) // force retries
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"verified": true, "first_name": "Ada", "last_name": "Okafor", "reference": "NIMC-1",
		})
	}))
	defer srv.Close()

	a := NewNIMCHTTPAdapter(srv.URL, "k3y")
	v, err := a.VerifyNIN("12345678901")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Verified || v.Source != "nimc_api" || v.FirstName != "Ada" {
		t.Fatalf("unexpected result: %+v", v)
	}
	if v.NINHash != NINHash("12345678901") {
		t.Fatalf("nin_hash mismatch")
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("expected 3 attempts (2 retries), got %d", calls)
	}
}

// TestNIMCHTTPAdapterCircuitBreaker checks the breaker opens after 5
// consecutive failures and short-circuits further calls.
func TestNIMCHTTPAdapterCircuitBreaker(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := NewNIMCHTTPAdapter(srv.URL, "k")
	if _, err := a.VerifyNIN("12345678901"); err == nil {
		t.Fatal("expected failure")
	}
	before := atomic.LoadInt32(&calls) // 3 attempts (breaker counts each)
	// keep failing until the breaker opens
	for i := 0; i < 3; i++ {
		_, _ = a.VerifyNIN("12345678901")
	}
	after := atomic.LoadInt32(&calls)
	// once open, VerifyNIN must fail without touching the server
	last := atomic.LoadInt32(&calls)
	if _, err := a.VerifyNIN("12345678901"); err == nil {
		t.Fatal("expected circuit-open error")
	}
	if atomic.LoadInt32(&calls) != last {
		t.Fatal("breaker open but server was still called")
	}
	if after <= before {
		t.Fatalf("expected more calls before opening (%d -> %d)", before, after)
	}
}
