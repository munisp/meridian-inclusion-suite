package main

// B2 regression tests (findings #6, #16, #18):
//
// #6: review approve/reject read the forgeable X-Dev-Role header directly
//     and let the read-only "auditor" role approve; roles now come from the
//     verified JWT (httpx.RequestRoles), auditor cannot approve, and forged
//     X-Meridian-Roles headers are stripped by the auth middleware.
// #16: provisionTIN/verifyNIN/verifyTIN accepted client-supplied
//     operator_id/NIN with no role or ownership check.
// #18: reviewDecision had no creator!=reviewer segregation of duties.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func newGateTestServer(t *testing.T) (http.Handler, *Registry) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{},
		NewConsentService(st), ledger.NewDevClient(), events.NewInprocBus())
	s := &server{registry: reg, verifier: NIMCSimulator{},
		provisioner: LocalTINProvisioner{}, workflows: wf}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/review/{id}/approve", s.reviewApprove)
	mux.HandleFunc("POST /v1/review/{id}/reject", s.reviewReject)
	mux.HandleFunc("POST /v1/tin/provision", s.provisionTIN)
	mux.HandleFunc("POST /v1/verify/nin", s.verifyNIN)
	mux.HandleFunc("POST /v1/verify/tin", s.verifyTIN)
	// dev auth middleware (strips forged X-Meridian-* headers, B2 #6)
	return httpx.Auth(func(string) bool { return false })(mux), reg
}

func gateReq(t *testing.T, h http.Handler, path, role, agentID, forgedRoles, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if role != "" {
		req.Header.Set("X-Dev-Role", role)
	}
	if agentID != "" {
		req.Header.Set("X-Dev-Agent-Id", agentID)
	}
	if forgedRoles != "" {
		req.Header.Set("X-Meridian-Roles", forgedRoles)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pendingOp(t *testing.T, reg *Registry, agentID string) Operator {
	t.Helper()
	op := Operator{NINHash: NINHash("12345678901"), FullName: "Gate Test",
		AgentID: agentID, CapturedAt: nowRFC3339(), ReviewStatus: "pending"}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if err := reg.Transition(&op, "pending_review", "capture:test"); err != nil {
		t.Fatal(err)
	}
	return op
}

// #6: auditor (read-only) must not approve — pre-fix X-Dev-Role: auditor passed.
func TestReviewAuditorCannotApprove(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	rec := gateReq(t, h, "/v1/review/"+op.ID+"/approve", "auditor", "", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("auditor approve: got %d, want 403", rec.Code)
	}
}

// #6: forged X-Meridian-Roles header must be stripped by the middleware —
// an auditor forging admin roles still gets 403.
func TestReviewForgedMeridianRolesStripped(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	rec := gateReq(t, h, "/v1/review/"+op.ID+"/approve", "auditor", "", "admin", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged X-Meridian-Roles approve: got %d, want 403", rec.Code)
	}
}

// #6: legitimate operator role approves.
func TestReviewOperatorApproves(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	rec := gateReq(t, h, "/v1/review/"+op.ID+"/approve", "operator", "agent-2", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("operator approve: got %d, want 200: %s", rec.Code, rec.Body)
	}
}

// #18: the capturing agent cannot review their own record, even with a
// back-office role.
func TestReviewSelfApprovalDenied(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	rec := gateReq(t, h, "/v1/review/"+op.ID+"/approve", "operator", "agent-1", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval: got %d, want 403", rec.Code)
	}
	got, _, _ := reg.Get(op.ID)
	if got.ReviewStatus == "approved" {
		t.Fatal("self-approval took effect despite 403")
	}
}

// #16: provisionTIN requires a back-office role and NIN ownership.
func TestProvisionTINRequiresRole(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	body := `{"operator_id":"` + op.ID + `","nin":"12345678901"}`
	rec := gateReq(t, h, "/v1/tin/provision", "auditor", "", "", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("auditor provision: got %d, want 403", rec.Code)
	}
}

func TestProvisionTINOwnershipMismatch(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	// NIN does not match the operator's registered NIN hash
	body := `{"operator_id":"` + op.ID + `","nin":"99999999999"}`
	rec := gateReq(t, h, "/v1/tin/provision", "admin", "", "", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nin mismatch provision: got %d, want 403", rec.Code)
	}
}

func TestProvisionTINUnknownOperator(t *testing.T) {
	h, _ := newGateTestServer(t)
	rec := gateReq(t, h, "/v1/tin/provision", "admin", "", "",
		`{"operator_id":"op-nope","nin":"12345678901"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown operator provision: got %d, want 404", rec.Code)
	}
}

func TestProvisionTINHappyPath(t *testing.T) {
	h, reg := newGateTestServer(t)
	op := pendingOp(t, reg, "agent-1")
	body := `{"operator_id":"` + op.ID + `","nin":"12345678901"}`
	rec := gateReq(t, h, "/v1/tin/provision", "operator", "agent-2", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator provision: got %d, want 200: %s", rec.Code, rec.Body)
	}
}

// #16: verifyNIN / verifyTIN restricted to back-office roles.
func TestVerifyNINTINRoleGated(t *testing.T) {
	h, _ := newGateTestServer(t)
	if rec := gateReq(t, h, "/v1/verify/nin", "auditor", "", "", `{"nin":"12345678901"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor verifyNIN: got %d, want 403", rec.Code)
	}
	if rec := gateReq(t, h, "/v1/verify/tin", "auditor", "", "", `{"tin":"12345678-0001"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor verifyTIN: got %d, want 403", rec.Code)
	}
	if rec := gateReq(t, h, "/v1/verify/nin", "operator", "", "", `{"nin":"12345678901"}`); rec.Code != http.StatusOK {
		t.Fatalf("operator verifyNIN: got %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, "/v1/verify/tin", "operator", "", "", `{"tin":"12345678-0001"}`); rec.Code != http.StatusOK {
		t.Fatalf("operator verifyTIN: got %d, want 200: %s", rec.Code, rec.Body)
	}
}
