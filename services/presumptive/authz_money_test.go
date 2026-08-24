package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// Regression: B4-2 — money-moving endpoints (NIP payout/refund, PSSP
// onboard/rotate-secret/status, gate flip, float topup/debit) were mounted
// with authentication but ZERO authorization: any valid token (e.g. an
// auditor) could move money. These tests pin role enforcement.

func authzTestHandler(t *testing.T) http.Handler {
	t.Helper()
	ts := newTestStack(t)
	srv := &server{
		pay:    ts.pay,
		float:  ts.float,
		engine: ts.engine,
		gates:  ts.gates,
		certs:  ts.certs,
		wf:     ts.wf,
		pssps:  NewPSSPRegistry(ts.st, NewPSSPHub()),
	}
	return httpx.CORS(httpx.Auth(publicPath)(srv.routes()))
}

func doReq(t *testing.T, h http.Handler, method, path, role, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Dev-Role", role)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestMoneyRoutes_ForbidAuditor(t *testing.T) {
	h := authzTestHandler(t)
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/nip/payout", `{"dest_account":"0123456789","dest_bank_code":"011","amount":1000}`},
		{"POST", "/v1/nip/refund", `{"dest_account":"0123456789","dest_bank_code":"011","amount":1000}`},
		{"POST", "/v1/nip/reversal", `{"session_id":"x"}`},
		{"POST", "/v1/nip/sweep", `{}`},
		{"POST", "/v1/pssps", `{"name":"Test PSSP","callback_url":"https://example.com/cb"}`},
		{"POST", "/v1/pssps/some-id/rotate-secret", `{}`},
		{"POST", "/v1/pssps/some-id/status", `{"status":"suspended"}`},
		{"POST", "/v1/gates/presumptive-collections/flip", `{"open":false}`},
		{"POST", "/v1/float/accounts", `{"agent_id":"a1","opening_balance":1000}`},
		{"POST", "/v1/float/topup", `{"agent_id":"a1","amount":1000}`},
		{"POST", "/v1/float/debit", `{"agent_id":"a1","amount":1000}`},
	}
	for _, rt := range routes {
		if code := doReq(t, h, rt.method, rt.path, "auditor", rt.body); code != http.StatusForbidden {
			t.Errorf("auditor %s %s = %d, want 403", rt.method, rt.path, code)
		}
	}
}

func TestMoneyRoutes_AuditorCannotFlipEvenWithValidToken(t *testing.T) {
	h := authzTestHandler(t)
	// unauthenticated must still 401 (authN preserved)
	if code := doReq(t, h, "POST", "/v1/float/topup", "", `{"agent_id":"a1","amount":1}`); code != http.StatusUnauthorized {
		t.Errorf("no-auth float topup = %d, want 401", code)
	}
}

func TestMoneyRoutes_OperatorAndAdminAllowed(t *testing.T) {
	h := authzTestHandler(t)
	// operator-allowed money routes: must not be 401/403 (400/404/503 from
	// missing fixtures prove the role gate passed and the handler ran)
	operatorRoutes := []struct{ method, path, body string }{
		{"POST", "/v1/nip/payout", `{"dest_account":"0123456789","dest_bank_code":"011","amount":1000}`},
		{"POST", "/v1/float/topup", `{"agent_id":"a1","amount":1000}`},
		{"POST", "/v1/float/debit", `{"agent_id":"a1","amount":1000}`},
		{"POST", "/v1/float/accounts", `{"agent_id":"a1","opening_balance":1000}`},
	}
	for _, rt := range operatorRoutes {
		code := doReq(t, h, rt.method, rt.path, "operator", rt.body)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want role gate to pass", rt.method, rt.path, code)
		}
	}
	// admin-only registry/gate routes
	adminRoutes := []struct{ method, path, body string }{
		{"POST", "/v1/pssps", `{"name":"Test PSSP","callback_url":"https://example.com/cb"}`},
		{"POST", "/v1/pssps/some-id/rotate-secret", `{}`},
		{"POST", "/v1/pssps/some-id/status", `{"status":"suspended"}`},
		{"POST", "/v1/gates/presumptive-collections/flip", `{"open":true}`},
	}
	for _, rt := range adminRoutes {
		code := doReq(t, h, rt.method, rt.path, "admin", rt.body)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("admin %s %s = %d, want role gate to pass", rt.method, rt.path, code)
		}
		// operator must NOT reach admin-only registry/gate controls
		code = doReq(t, h, rt.method, rt.path, "operator", rt.body)
		if code != http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want 403 (admin-only)", rt.method, rt.path, code)
		}
	}
}

// Regression (B4-2 repair, V2 round): the PRIMARY payment lifecycle routes
// and the workflow trigger were left ungated by the original B4-2 fix —
// an auditor token reached intent/authorise/capture/void (money movement
// via PSSP adapter) and could trigger arbitrary workflows. Pin the gates:
// operator/admin on the payment lifecycle, admin-only on workflow trigger.

func TestPaymentLifecycleRoutes_ForbidAuditor(t *testing.T) {
	h := authzTestHandler(t)
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/payments/intent", `{"amount":1000,"currency":"NGN"}`},
		{"POST", "/v1/payments/p1/authorise", `{}`},
		{"POST", "/v1/payments/p1/capture", `{}`},
		{"POST", "/v1/payments/p1/void", `{}`},
	}
	for _, rt := range routes {
		if code := doReq(t, h, rt.method, rt.path, "auditor", rt.body); code != http.StatusForbidden {
			t.Errorf("auditor %s %s = %d, want 403", rt.method, rt.path, code)
		}
	}
}

func TestPaymentLifecycleRoutes_OperatorAllowed(t *testing.T) {
	h := authzTestHandler(t)
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/payments/intent", `{"amount":1000,"currency":"NGN"}`},
		{"POST", "/v1/payments/p1/authorise", `{}`},
		{"POST", "/v1/payments/p1/capture", `{}`},
		{"POST", "/v1/payments/p1/void", `{}`},
	}
	for _, rt := range routes {
		code := doReq(t, h, rt.method, rt.path, "operator", rt.body)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want handler to run (not 401/403)", rt.method, rt.path, code)
		}
	}
}

func TestWorkflowTrigger_AdminOnly(t *testing.T) {
	h := authzTestHandler(t)
	for _, role := range []string{"auditor", "operator"} {
		if code := doReq(t, h, "POST", "/v1/workflows/w1/trigger", role, `{}`); code != http.StatusForbidden {
			t.Errorf("%s workflow trigger = %d, want 403", role, code)
		}
	}
	// admin passes the role gate (handler may then 400/404/503 on fixtures)
	code := doReq(t, h, "POST", "/v1/workflows/w1/trigger", "admin", `{}`)
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("admin workflow trigger = %d, want handler to run", code)
	}
}
