package main

// hierarchy_test.go — I6: hierarchy invariants (cycles, depth cap) and
// tenant/JWT-bound management authz.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func seedAgent(t *testing.T, reg *AgentRegistry, name, tenant string) Agent {
	t.Helper()
	ag, err := reg.Register(Agent{FullName: name, Phone: "080" + name, TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func mustAttach(t *testing.T, h *Hierarchy, child, parent string) {
	t.Helper()
	if _, err := h.Attach(child, parent); err != nil {
		t.Fatalf("attach %s under %s: %v", child, parent, err)
	}
}

func TestHierarchyCycleRejected(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	h := NewHierarchy(reg)
	a := seedAgent(t, reg, "a", "t1")
	b := seedAgent(t, reg, "b", "t1")
	c := seedAgent(t, reg, "c", "t1")
	mustAttach(t, h, b.ID, a.ID)
	mustAttach(t, h, c.ID, b.ID)

	if _, err := h.Attach(a.ID, a.ID); !errors.Is(err, ErrHierarchyCycle) {
		t.Fatalf("self-attach: want ErrHierarchyCycle, got %v", err)
	}
	if _, err := h.Attach(a.ID, c.ID); !errors.Is(err, ErrHierarchyCycle) {
		t.Fatalf("reattach root under own descendant: want ErrHierarchyCycle, got %v", err)
	}
	if _, err := h.Attach(b.ID, c.ID); !errors.Is(err, ErrHierarchyCycle) {
		t.Fatalf("reattach mid under descendant: want ErrHierarchyCycle, got %v", err)
	}
}

func TestHierarchyDepthCap(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	h := NewHierarchy(reg)
	chain := make([]Agent, 5)
	for i := range chain {
		chain[i] = seedAgent(t, reg, string(rune('a'+i)), "t1")
	}
	// root -> l1 -> l2 -> l3 is the cap (3 edges).
	mustAttach(t, h, chain[1].ID, chain[0].ID)
	mustAttach(t, h, chain[2].ID, chain[1].ID)
	mustAttach(t, h, chain[3].ID, chain[2].ID)
	if d, _ := h.Depth(chain[3].ID); d != 3 {
		t.Fatalf("depth: got %d, want 3", d)
	}
	// level 4 must be rejected, whether attached fresh or via a subtree move.
	if _, err := h.Attach(chain[4].ID, chain[3].ID); !errors.Is(err, ErrDepthCap) {
		t.Fatalf("4th level: want ErrDepthCap, got %v", err)
	}
	other := seedAgent(t, reg, "z", "t1")
	mustAttach(t, h, chain[4].ID, other.ID) // depth-1 subtree
	if _, err := h.Attach(other.ID, chain[2].ID); !errors.Is(err, ErrDepthCap) {
		t.Fatalf("subtree move past cap: want ErrDepthCap, got %v", err)
	}
}

func newHierarchyTestServer(t *testing.T) (http.Handler, *AgentRegistry) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewAgentRegistry(st)
	s := &server{agents: reg, hierarchy: NewHierarchy(reg)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	mux.HandleFunc("GET /v1/agents/{id}/downline", s.agentDownline)
	mux.HandleFunc("POST /v1/agents/{id}/parent", s.attachSubAgent)
	return httpx.Auth(func(string) bool { return false })(mux), reg
}

func hierarchyReq(t *testing.T, h http.Handler, method, path, role, agentID, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if role != "" {
		req.Header.Set("X-Dev-Role", role)
	}
	if agentID != "" {
		req.Header.Set("X-Dev-Agent-Id", agentID)
	}
	if tenant != "" {
		req.Header.Set("X-Dev-Tenant-Id", tenant)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHierarchyTenantIsolation(t *testing.T) {
	h, reg := newHierarchyTestServer(t)
	// Tenant t1: agentA with downline.
	a1 := seedAgent(t, reg, "a1", "t1")
	a2 := seedAgent(t, reg, "a2", "t1")
	mustAttach(t, NewHierarchy(reg), a2.ID, a1.ID)
	// Tenant t2: agentB with downline.
	b1 := seedAgent(t, reg, "b1", "t2")
	b2 := seedAgent(t, reg, "b2", "t2")
	mustAttach(t, NewHierarchy(reg), b2.ID, b1.ID)

	// Same-tenant agent principal reads own downline -> 200 with both agents.
	rec := hierarchyReq(t, h, "GET", "/v1/agents/"+a1.ID+"/downline", "auditor", a1.ID, "t1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("own downline: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Count != 2 {
		t.Fatalf("own downline: want count=2, got %v %v", out.Count, err)
	}

	// Cross-tenant: agent A's principal cannot see agent B's downline -> 404
	// (no existence oracle across tenants).
	if rec := hierarchyReq(t, h, "GET", "/v1/agents/"+b1.ID+"/downline", "auditor", a1.ID, "t1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant downline: got %d, want 404", rec.Code)
	}
	// Cross-tenant back-office is likewise denied.
	if rec := hierarchyReq(t, h, "GET", "/v1/agents/"+b1.ID+"/downline", "admin", "", "t1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant admin: got %d, want 404", rec.Code)
	}
	// Sub-agent a2 manages only its OWN subtree — its upline's downline is
	// outside that subtree -> 403.
	if rec := hierarchyReq(t, h, "GET", "/v1/agents/"+a1.ID+"/downline", "auditor", a2.ID, "t1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("sub-agent reading upline downline: got %d, want 403", rec.Code)
	}
	// a2 cannot manage a1's sibling (nothing outside its own subtree): use
	// an independent t1 root.
	c1 := seedAgent(t, reg, "c1", "t1")
	rec = hierarchyReq(t, h, "GET", "/v1/agents/"+c1.ID, "auditor", a2.ID, "t1", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("same-tenant outside subtree: got %d, want 403", rec.Code)
	}
	// Same-tenant admin manages any agent -> 200.
	if rec := hierarchyReq(t, h, "GET", "/v1/agents/"+c1.ID, "operator", "", "t1", ""); rec.Code != http.StatusOK {
		t.Fatalf("same-tenant operator: got %d, want 200", rec.Code)
	}
}

func TestHierarchyAttachAuthz(t *testing.T) {
	h, reg := newHierarchyTestServer(t)
	hier := NewHierarchy(reg)
	a1 := seedAgent(t, reg, "a1", "t1")
	a2 := seedAgent(t, reg, "a2", "t1")
	x1 := seedAgent(t, reg, "x1", "t1")

	// Agent principal may attach within its own subtree...
	mustAttach(t, hier, a2.ID, a1.ID)
	body := `{"parent_id":"` + a2.ID + `"}`
	// ...but x1 is not in a2's subtree, so a2 cannot manage x1 -> 403.
	if rec := hierarchyReq(t, h, "POST", "/v1/agents/"+x1.ID+"/parent", "auditor", a2.ID, "t1", body); rec.Code != http.StatusForbidden {
		t.Fatalf("attach outside own subtree: got %d, want 403", rec.Code)
	}
	// Admin in the same tenant may attach -> 200, cycle-safe end-to-end.
	if rec := hierarchyReq(t, h, "POST", "/v1/agents/"+x1.ID+"/parent", "admin", "", "t1", body); rec.Code != http.StatusOK {
		t.Fatalf("admin attach: got %d (%s)", rec.Code, rec.Body.String())
	}
	// HTTP-level cycle: attach a1 under its own descendant x1 -> 409.
	anti := `{"parent_id":"` + x1.ID + `"}`
	if rec := hierarchyReq(t, h, "POST", "/v1/agents/"+a1.ID+"/parent", "admin", "", "t1", anti); rec.Code != http.StatusConflict {
		t.Fatalf("cycle via API: got %d, want 409", rec.Code)
	}
}
