package main

// hierarchy.go — I6 agent hierarchy: parent -> sub-agent tree over the O5
// agent registry. Invariants:
//   - depth cap: a subtree is at most maxAgentDepth edges deep from its root
//     (root at depth 0; attaching under an agent already at depth 3 is
//     rejected);
//   - cycle-safe: a parent can never be the agent itself or one of its own
//     descendants (re-attach moves are checked against the child's subtree);
//   - tenant-scoped: parent and child must share a TenantID; every read of
//     another agent's subtree is tenant-checked, so tenant A can never
//     enumerate tenant B's downline.
//
// Management is JWT-bound (authx-stamped X-Meridian-Caller / X-Meridian-Roles
// in keycloak mode; dev JWT sub / X-Dev-* only in AUTH_MODE=dev):
//   - admin/operator roles manage any agent inside their own tenant;
//   - any other authenticated caller manages only the subtree rooted at the
//     agent whose id equals their authenticated identity.

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// DefaultTenant is assigned to agents registered without an explicit tenant.
const DefaultTenant = "default"

// maxAgentDepth caps the hierarchy at 3 edges from a subtree root:
// root(0) -> sub-agent(1) -> sub-sub-agent(2) -> level-3 agent(3).
const maxAgentDepth = 3

var (
	// ErrDepthCap is returned when an attach would exceed maxAgentDepth.
	ErrDepthCap = errors.New("agent hierarchy depth cap exceeded (max 3 levels)")
	// ErrHierarchyCycle is returned when an attach would create a cycle.
	ErrHierarchyCycle = errors.New("agent hierarchy cycle rejected: parent is the agent or its descendant")
	// ErrTenantMismatch is returned on cross-tenant hierarchy operations.
	ErrTenantMismatch = errors.New("tenant mismatch: parent and child must share a tenant")
)

// Hierarchy manages parent -> sub-agent links over the AgentRegistry.
type Hierarchy struct{ agents *AgentRegistry }

func NewHierarchy(agents *AgentRegistry) *Hierarchy { return &Hierarchy{agents: agents} }

func (h *Hierarchy) get(id string) (Agent, error) {
	ag, ok, err := h.agents.Get(id)
	if err != nil {
		return Agent{}, err
	}
	if !ok {
		return Agent{}, fmt.Errorf("agent %s not found", id)
	}
	return ag, nil
}

// Depth returns the number of edges from id up to its subtree root, following
// ParentID links. A corrupted link (missing parent) stops the walk; a stored
// cycle (should never happen — Attach rejects them) is bounded.
func (h *Hierarchy) Depth(id string) (int, error) {
	depth := 0
	cur, err := h.get(id)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{id: true}
	for cur.ParentID != "" {
		depth++
		if depth > maxAgentDepth+1 { // belt-and-braces bound
			return depth, ErrHierarchyCycle
		}
		parent, err := h.get(cur.ParentID)
		if err != nil {
			return depth, nil // dangling parent link: treat as root
		}
		if seen[parent.ID] {
			return depth, ErrHierarchyCycle
		}
		seen[parent.ID] = true
		cur = parent
	}
	return depth, nil
}

// Ancestors returns the upline chain from id (parent first, root last),
// bounded at maxAgentDepth entries.
func (h *Hierarchy) Ancestors(id string) ([]Agent, error) {
	var out []Agent
	cur, err := h.get(id)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{id: true}
	for cur.ParentID != "" && len(out) < maxAgentDepth {
		parent, err := h.get(cur.ParentID)
		if err != nil {
			break // dangling parent: stop
		}
		if seen[parent.ID] {
			return nil, ErrHierarchyCycle
		}
		seen[parent.ID] = true
		out = append(out, parent)
		cur = parent
	}
	return out, nil
}

// Subtree returns id plus all of its descendants (tenant-consistent by
// construction — Attach enforces a single tenant per subtree).
func (h *Hierarchy) Subtree(id string) ([]Agent, error) {
	root, err := h.get(id)
	if err != nil {
		return nil, err
	}
	all, err := h.agents.List()
	if err != nil {
		return nil, err
	}
	children := map[string][]Agent{}
	for _, ag := range all {
		if ag.ParentID != "" {
			children[ag.ParentID] = append(children[ag.ParentID], ag)
		}
	}
	out := []Agent{root}
	queue := []Agent{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ch := range children[cur.ID] {
			out = append(out, ch)
			queue = append(queue, ch)
		}
	}
	return out, nil
}

// Attach links childID under parentID (or detaches when parentID == "").
// Checks: both agents exist, same tenant, no cycle, depth cap.
func (h *Hierarchy) Attach(childID, parentID string) (Agent, error) {
	child, err := h.get(childID)
	if err != nil {
		return Agent{}, err
	}
	if parentID == "" {
		child.ParentID = ""
		child.UpdatedAt = nowRFC3339()
		return child, h.agents.st.Put("agents", child.ID, child)
	}
	parent, err := h.get(parentID)
	if err != nil {
		return Agent{}, err
	}
	if child.TenantID != parent.TenantID {
		return Agent{}, ErrTenantMismatch
	}
	if parentID == childID {
		return Agent{}, ErrHierarchyCycle
	}
	// Cycle check: the new parent must not sit inside the child's subtree.
	sub, err := h.Subtree(childID)
	if err != nil {
		return Agent{}, err
	}
	for _, d := range sub {
		if d.ID == parentID {
			return Agent{}, ErrHierarchyCycle
		}
	}
	// Depth check: parent depth + child subtree height must stay within cap.
	parentDepth, err := h.Depth(parentID)
	if err != nil {
		return Agent{}, err
	}
	height := subtreeHeight(child.ID, sub)
	if parentDepth+1+height > maxAgentDepth {
		return Agent{}, ErrDepthCap
	}
	child.ParentID = parentID
	child.UpdatedAt = nowRFC3339()
	return child, h.agents.st.Put("agents", child.ID, child)
}

// subtreeHeight returns the height (in edges) of the subtree rooted at root,
// given the already-computed subtree listing.
func subtreeHeight(rootID string, sub []Agent) int {
	children := map[string][]string{}
	for _, ag := range sub {
		if ag.ParentID != "" {
			children[ag.ParentID] = append(children[ag.ParentID], ag.ID)
		}
	}
	var walk func(id string) int
	walk = func(id string) int {
		max := 0
		for _, ch := range children[id] {
			if d := walk(ch) + 1; d > max {
				max = d
			}
		}
		return max
	}
	return walk(rootID)
}

// InSubtree reports whether candidateID is id itself or one of its
// descendants (used for subtree-scoped management authz).
func (h *Hierarchy) InSubtree(id, candidateID string) bool {
	if id == candidateID {
		return true
	}
	sub, err := h.Subtree(id)
	if err != nil {
		return false
	}
	for _, ag := range sub {
		if ag.ID == candidateID {
			return true
		}
	}
	return false
}

// requestTenant resolves the caller's tenant: the authx-propagated
// X-Meridian-Tenant header in keycloak mode, the dev X-Dev-Tenant-Id stand-in
// otherwise; empty means the default tenant.
func requestTenant(r *http.Request) string {
	if t := r.Header.Get("X-Meridian-Tenant"); t != "" {
		return t
	}
	if os.Getenv("AUTH_MODE") != "keycloak" {
		if t := r.Header.Get("X-Dev-Tenant-Id"); t != "" {
			return t
		}
	}
	return DefaultTenant
}

// canManageAgent enforces JWT-bound, tenant-scoped hierarchy management:
// admin/operator roles manage any agent in their own tenant; any other
// caller manages only the subtree rooted at the agent matching their
// authenticated identity. Returns false when access must be denied.
func (h *Hierarchy) canManageAgent(r *http.Request, target Agent) bool {
	tenant := requestTenant(r)
	backOffice := false
	for _, role := range httpx.RequestRoles(r) {
		if role == "admin" || role == "operator" {
			backOffice = true
		}
	}
	if backOffice {
		return target.TenantID == "" || target.TenantID == tenant
	}
	id := httpx.CallerIdentity(r)
	if id == "" {
		return false
	}
	// An agent principal manages only its own subtree, within its tenant.
	self, ok, err := h.agents.Get(id)
	if err != nil || !ok {
		return false
	}
	if self.TenantID != target.TenantID {
		return false
	}
	return h.InSubtree(self.ID, target.ID)
}

// visibleAgent resolves r.PathValue("id") with tenant + subtree authz.
// Returns the agent, or writes the appropriate problem (404 for unknown or
// cross-tenant ids — no existence oracle; 403 for same-tenant records
// outside the caller's subtree).
func (h *Hierarchy) visibleAgent(w http.ResponseWriter, r *http.Request, id string) (Agent, bool) {
	ag, ok, err := h.agents.Get(id)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return Agent{}, false
	}
	if !ok || (requestTenant(r) != DefaultTenant && ag.TenantID != requestTenant(r)) {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "agent not found")
		return Agent{}, false
	}
	if !h.canManageAgent(r, ag) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"agents manage only their own subtree")
		return Agent{}, false
	}
	return ag, true
}

// subtreeIDs is a convenience for commission rollups: the id set of a subtree.
func (h *Hierarchy) subtreeIDs(id string) (map[string]bool, error) {
	sub, err := h.Subtree(id)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, ag := range sub {
		out[ag.ID] = true
	}
	return out, nil
}
