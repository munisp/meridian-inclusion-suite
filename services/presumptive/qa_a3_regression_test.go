package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ---- QA-20: PSSP simulators must not be selectable under PROFILE=prod ----

// In prod with a real PSSP_API_URL configured, the hub registers ONLY the
// real adapter; the simulators (remita/etranzact/flutterwave) are absent.
func TestPSSPSimulatorsAbsentInProd(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("PSSP_API_URL", "http://127.0.0.1:1")
	t.Setenv("PSSP_API_KEY", "k")
	hub := NewPSSPHub()
	for _, sim := range []string{"remita", "etranzact", "flutterwave"} {
		if _, err := hub.Adapter(sim); err == nil {
			t.Fatalf("simulator provider %q must not be selectable under PROFILE=prod", sim)
		}
	}
	if _, err := hub.Adapter("pssp"); err != nil {
		t.Fatalf("real pssp adapter must be registered in prod: %v", err)
	}
}

// In dev the simulators remain available.
func TestPSSPSimulatorsPresentInDev(t *testing.T) {
	t.Setenv("APP_PROFILE", "")
	t.Setenv("PSSP_API_URL", "")
	hub := NewPSSPHub()
	for _, sim := range []string{"remita", "etranzact", "flutterwave"} {
		if _, err := hub.Adapter(sim); err != nil {
			t.Fatalf("simulator provider %q must be available in dev: %v", sim, err)
		}
	}
}

// Prod without PSSP_API_URL hard-fails (log.Fatal -> non-zero exit).
func TestPSSPProdWithoutURLFatals(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		t.Setenv("APP_PROFILE", "prod")
		os.Setenv("APP_PROFILE", "prod")
		os.Setenv("PSSP_API_URL", "")
		NewPSSPHub()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestPSSPProdWithoutURLFatals")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "APP_PROFILE=prod", "PSSP_API_URL=")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit (log.Fatal) for prod without PSSP_API_URL")
	}
}

// ---- QA-21: NIP rail fails closed under PROFILE=prod ----

func TestNIPProdWithoutRailFailsClosed(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("NIP_RAIL", "")
	t.Setenv("NIP_API_URL", "")
	if _, err := NewNIPRailFromEnv(); err == nil {
		t.Fatal("PROFILE=prod without NIP_API_URL must fail closed")
	} else if !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
}

// ---- QA-08: listPayments pagination (default 50, max 500) ----

func seedPayments(t *testing.T, ts *testStack, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ts.mkIntent(t, fmt.Sprintf("tin-page-%d", i))
	}
}

func listPaymentsRequest(t *testing.T, ts *testStack, query string) map[string]any {
	t.Helper()
	s := &server{pay: ts.pay}
	req := httptest.NewRequest("GET", "/v1/payments"+query, nil)
	rec := httptest.NewRecorder()
	s.listPayments(rec, req)
	if rec.Code != 200 {
		t.Fatalf("listPayments %s: status %d body %s", query, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListPaymentsDefaultLimit(t *testing.T) {
	ts := newTestStack(t)
	seedPayments(t, ts, 60)
	out := listPaymentsRequest(t, ts, "")
	if got := int(out["count"].(float64)); got != defaultListPaymentsLimit {
		t.Fatalf("default page size = %d, want %d", got, defaultListPaymentsLimit)
	}
	if got := int(out["total"].(float64)); got != 60 {
		t.Fatalf("total = %d, want 60", got)
	}
}

func TestListPaymentsMaxClamp(t *testing.T) {
	ts := newTestStack(t)
	seedPayments(t, ts, 3)
	out := listPaymentsRequest(t, ts, "?limit=1000000")
	if got := int(out["limit"].(float64)); got != maxListPaymentsLimit {
		t.Fatalf("limit = %d, want clamped %d", got, maxListPaymentsLimit)
	}
	if got := int(out["count"].(float64)); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
}

func TestListPaymentsOffsetWindow(t *testing.T) {
	ts := newTestStack(t)
	seedPayments(t, ts, 7)
	out := listPaymentsRequest(t, ts, "?limit=3&offset=5")
	if got := int(out["count"].(float64)); got != 2 {
		t.Fatalf("count = %d, want 2 (tail window)", got)
	}
	out = listPaymentsRequest(t, ts, "?limit=3&offset=100")
	if got := int(out["count"].(float64)); got != 0 {
		t.Fatalf("count = %d, want 0 (offset past end)", got)
	}
}
