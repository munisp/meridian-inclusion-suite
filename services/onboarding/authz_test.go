// authz_test.go — audit H-5: object-level authz on operator PII endpoints.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func newAuthzTestServer(t *testing.T) (http.Handler, *Registry) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	s := &server{registry: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operators", s.listOperators)
	mux.HandleFunc("GET /v1/operators/{id}", s.getOperator)
	mux.HandleFunc("PATCH /v1/operators/{id}", s.patchOperator)
	return httpx.Auth(func(string) bool { return false })(mux), reg
}

func seedOp(t *testing.T, reg *Registry, name, phone, agent string) Operator {
	t.Helper()
	op := Operator{NINHash: NINHash("12345678901"), FullName: name, Phone: phone, AgentID: agent, CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	return op
}

func doAuthzReq(t *testing.T, h http.Handler, method, path, role, agentID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if role != "" {
		req.Header.Set("X-Dev-Role", role)
	}
	if agentID != "" {
		req.Header.Set("X-Dev-Agent-Id", agentID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGetOperatorObjectLevelAuthz(t *testing.T) {
	h, reg := newAuthzTestServer(t)
	own := seedOp(t, reg, "Alice", "08030000001", "agent-1")
	other := seedOp(t, reg, "Bob", "08030000002", "agent-2")

	// own record (via agent id) -> 200
	if rec := doAuthzReq(t, h, "GET", "/v1/operators/"+own.ID, "operator", "agent-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("own record: got %d, want 200", rec.Code)
	}
	// cross-operator read -> 403 RFC7807
	rec := doAuthzReq(t, h, "GET", "/v1/operators/"+other.ID, "operator", "agent-1", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-operator read: got %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("want RFC7807 problem+json, got %q", ct)
	}
	// admin -> 200 on any record
	if rec := doAuthzReq(t, h, "GET", "/v1/operators/"+other.ID, "admin", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin read: got %d, want 200", rec.Code)
	}
	// unauthenticated -> 401
	if rec := doAuthzReq(t, h, "GET", "/v1/operators/"+own.ID, "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: got %d, want 401", rec.Code)
	}
}

func TestPatchOperatorObjectLevelAuthz(t *testing.T) {
	h, reg := newAuthzTestServer(t)
	other := seedOp(t, reg, "Bob", "08030000002", "agent-2")

	// cross-operator patch -> 403 and no mutation
	rec := doAuthzReq(t, h, "PATCH", "/v1/operators/"+other.ID, "operator", "agent-1", `{"full_name":"Hacked"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-operator patch: got %d, want 403", rec.Code)
	}
	op, _, _ := reg.Get(other.ID)
	if op.FullName != "Bob" {
		t.Fatalf("record mutated despite 403: %+v", op)
	}
	// own patch -> 200
	rec = doAuthzReq(t, h, "PATCH", "/v1/operators/"+other.ID, "operator", "agent-2", `{"full_name":"Bobby"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("own patch: got %d, want 200", rec.Code)
	}
}

func TestListOperatorsScopedForNonAdmin(t *testing.T) {
	h, reg := newAuthzTestServer(t)
	seedOp(t, reg, "Alice", "08030000001", "agent-1")
	seedOp(t, reg, "Bob", "08030000002", "agent-2")

	var body struct {
		Operators []Operator `json:"operators"`
		Count     int        `json:"count"`
	}
	// non-admin sees only own records
	rec := doAuthzReq(t, h, "GET", "/v1/operators", "operator", "agent-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Operators[0].AgentID != "agent-1" {
		t.Fatalf("non-admin list leaked other records: %+v", body)
	}
	// admin sees all
	rec = doAuthzReq(t, h, "GET", "/v1/operators", "admin", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Count != 2 {
		t.Fatalf("admin list: got %d, want 2", body.Count)
	}
}
