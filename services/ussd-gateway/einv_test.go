package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubEInvoicing is a minimal einvoicing-service double honouring the NRS
// contract: create with Idempotency-Key (replay on same key), and
// status-by-IRN via resubmission of a payload carrying only the IRN.
func stubEInvoicing(t *testing.T, fail bool) (*httptest.Server, *int32, *[]string) {
	t.Helper()
	var calls int32
	idemKeys := []string{}
	type rec struct{ irn, status, payment string }
	byIRN := map[string]rec{}
	byIdem := map[string]string{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(500)
			w.Write([]byte(`{"title":"upstream down"}`))
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/invoices/nrs" {
			w.WriteHeader(404)
			return
		}
		idemKeys = append(idemKeys, r.Header.Get("Idempotency-Key"))
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		// status-by-IRN resubmission
		if irn, _ := in["irn"].(string); irn != "" {
			if rec, ok := byIRN[irn]; ok {
				w.WriteHeader(200)
				json.NewEncoder(w).Encode(map[string]any{
					"irn": rec.irn, "status": rec.status, "payment_status": rec.payment,
					"idempotent_replay": true,
				})
			} else {
				w.WriteHeader(422)
				w.Write([]byte(`{"title":"NRS schema validation failed"}`))
			}
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if prior, ok := byIdem[key]; ok {
			rec := byIRN[prior]
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"irn": rec.irn, "status": rec.status, "payment_status": rec.payment,
				"idempotent_replay": true,
			})
			return
		}
		irn := "IRN-0001-TEST"
		byIRN[irn] = rec{irn: irn, status: "CONFIRMED", payment: "PENDING"}
		byIdem[key] = irn
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{
			"irn": irn, "status": "CONFIRMED", "payment_status": "PENDING", "invoice_id": "inv-1",
		})
	})), &calls, &idemKeys
}

func TestEInvMenuNavigation(t *testing.T) {
	t.Setenv("EINVOICING_BASE_URL", "")
	t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	sess, _ := runFlow(t, eng, store, "5")
	if !strings.Contains(sess, "E-invoice services") {
		t.Fatalf("home option 5 should render einv_home, got: %s", sess)
	}
	out, _ := runFlow(t, eng, store, "5", "1")
	if !strings.Contains(out, "KOBO") {
		t.Fatalf("issue path should prompt for kobo amount, got: %s", out)
	}
	out2, _ := runFlow(t, eng, store, "5", "2")
	if !strings.Contains(out2, "IRN") {
		t.Fatalf("status path should prompt for IRN, got: %s", out2)
	}
}

func TestEInvAmountValidation(t *testing.T) {
	t.Setenv("EINVOICING_BASE_URL", "")
	t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	// negative / decimal amounts rejected by the menu regex
	out, _ := runFlow(t, eng, store, "5", "1", "-500")
	if !strings.Contains(out, "Invalid amount") {
		t.Fatalf("negative amount should be rejected, got: %s", out)
	}
	out, _ = runFlow(t, eng, store, "5", "1", "10.50")
	if !strings.Contains(out, "Invalid amount") {
		t.Fatalf("decimal amount should be rejected, got: %s", out)
	}
	// zero rejected
	out, _ = runFlow(t, eng, store, "5", "1", "0")
	if !strings.Contains(out, "Invalid amount") {
		t.Fatalf("zero amount should be rejected, got: %s", out)
	}
	// absurd amount passes the regex but is rejected by the action bound
	out, _ = runFlow(t, eng, store, "5", "1", "999999999999", "12345678901")
	if !strings.Contains(out, "exceeds the USSD limit") {
		t.Fatalf("absurd amount should be rejected by action, got: %s", out)
	}
	if !strings.Contains(out, "No invoice was issued") {
		t.Fatalf("rejection must be honest (no fake success), got: %s", out)
	}
}

func TestEInvIssueSuccessAndRedialNoDoubleIssue(t *testing.T) {
	stub, calls, idemKeys := stubEInvoicing(t, false)
	defer stub.Close()
	t.Setenv("EINVOICING_BASE_URL", stub.URL)
	t.Setenv("APP_PROFILE", "")
	t.Setenv("EINVOICING_SERVICE_TOKEN", "tok-1")
	eng, store, _ := newTestEngine(t)

	out, sess := runFlow(t, eng, store, "5", "1", "150000", "12345678901")
	if !strings.Contains(out, "Invoice issued") || !strings.Contains(out, "IRN-0001-TEST") {
		t.Fatalf("expected issued invoice, got: %s", out)
	}
	if !strings.Contains(out, "N1500.00") {
		t.Fatalf("expected naira rendering, got: %s", out)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", *calls)
	}
	key1 := sess.Data["einv_idem_key"]
	if key1 == "" || !strings.HasPrefix(key1, "ussd:t1:") {
		t.Fatalf("durable idempotency key missing: %q", key1)
	}

	// Redial/resume: session re-keyed to a new sessionId, data carried over.
	sess2 := &Session{ID: "t2-redial", Phone: sess.Phone, Menu: sess.Menu, Data: sess.Data, CreatedAt: sess.CreatedAt}
	// simulate the resume path re-running the issue step
	sess2.Menu = "einv_issue"
	delete(sess2.Data, "irn")
	out2, cont, err := eng.renderCurrent(sess2, "")
	if err != nil || cont {
		t.Fatalf("redial render: err=%v cont=%v", err, cont)
	}
	if !strings.Contains(out2, "IRN-0001-TEST") {
		t.Fatalf("redial should replay the issued invoice, got: %s", out2)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("redial must not double-issue: upstream calls = %d", *calls)
	}
	if sess2.Data["einv_idem_key"] != key1 {
		t.Fatalf("idempotency key must survive redial: %q vs %q", sess2.Data["einv_idem_key"], key1)
	}
	// every upstream issuance call carried the durable key
	for _, k := range *idemKeys {
		if k == "" {
			t.Fatalf("issuance call missing Idempotency-Key header")
		}
	}
}

func TestEInvUpstreamFailureHonest(t *testing.T) {
	stub, _, _ := stubEInvoicing(t, true)
	defer stub.Close()
	t.Setenv("EINVOICING_BASE_URL", stub.URL)
	t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)
	out, _ := runFlow(t, eng, store, "5", "1", "150000", "12345678901")
	if strings.Contains(out, "Invoice issued") {
		t.Fatalf("upstream failure must never fake success, got: %s", out)
	}
	if !strings.Contains(out, "temporarily unavailable") || !strings.Contains(out, "No invoice was issued") {
		t.Fatalf("expected honest failure message, got: %s", out)
	}
}

func TestEInvStatusByIRN(t *testing.T) {
	stub, _, _ := stubEInvoicing(t, false)
	defer stub.Close()
	t.Setenv("EINVOICING_BASE_URL", stub.URL)
	t.Setenv("APP_PROFILE", "")
	eng, store, _ := newTestEngine(t)

	// issue first so the stub knows the IRN
	_, sess := runFlow(t, eng, store, "5", "1", "150000", "12345678901")
	irn := sess.Data["einv_issued_irn"]
	if irn == "" {
		t.Fatalf("setup: issue failed")
	}
	out, _ := runFlow(t, eng, store, "5", "2", irn)
	if !strings.Contains(out, "Status: CONFIRMED") || !strings.Contains(out, "Payment: PENDING") {
		t.Fatalf("status lookup failed, got: %s", out)
	}
	// unknown IRN -> honest not-found
	out, _ = runFlow(t, eng, store, "5", "2", "IRN-9999-XXXX")
	if !strings.Contains(out, "no invoice found") {
		t.Fatalf("unknown IRN should be reported honestly, got: %s", out)
	}
}

func TestEInvProdGate(t *testing.T) {
	t.Setenv("EINVOICING_BASE_URL", "")
	t.Setenv("APP_PROFILE", "prod")
	defer t.Setenv("APP_PROFILE", "")
	if _, err := einvConfigFromEnv(); err == nil {
		t.Fatal("prod without EINVOICING_BASE_URL must fail closed")
	}
	t.Setenv("EINVOICING_BASE_URL", "http://einv.internal:8110")
	base, err := einvConfigFromEnv()
	if err != nil || base != "http://einv.internal:8110" {
		t.Fatalf("prod with base URL should pass: %v %q", err, base)
	}
	// dev tolerates missing base URL
	t.Setenv("APP_PROFILE", "")
	t.Setenv("EINVOICING_BASE_URL", "")
	if _, err := einvConfigFromEnv(); err != nil {
		t.Fatalf("dev should not require EINVOICING_BASE_URL: %v", err)
	}
}
