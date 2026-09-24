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
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// agentReader is the read surface Attach's validation runs against: the
// AgentRegistry directly (embedded backend) or a Postgres-transaction-backed
// adapter (prod), so cycle/depth checks observe the same snapshot as the
// locked rows and the parent-link write.
type agentReader interface {
	Get(id string) (Agent, bool, error)
	List() ([]Agent, error)
	// ChildrenOf returns the direct children of parentID via the parent_id
	// secondary index (perf H2/H3: subtree walks no longer full-scan the
	// agents table per call).
	ChildrenOf(parentID string) ([]Agent, error)
}

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
//
// mu serialises Attach: the cycle/depth checks and the parent-link write
// must be atomic, otherwise two concurrent re-parents (A->B and B->A) each
// pass the cycle check before either write lands and a 2-cycle is stored
// (R4-S3#3: check-then-act race -> stored cycle -> unbounded Subtree BFS).
type Hierarchy struct {
	agents *AgentRegistry
	mu     sync.Mutex
}

func NewHierarchy(agents *AgentRegistry) *Hierarchy { return &Hierarchy{agents: agents} }

func agentByID(r agentReader, id string) (Agent, error) {
	ag, ok, err := r.Get(id)
	if err != nil {
		return Agent{}, err
	}
	if !ok {
		return Agent{}, fmt.Errorf("agent %s not found", id)
	}
	return ag, nil
}

func (h *Hierarchy) get(id string) (Agent, error) { return agentByID(h.agents, id) }

// Depth returns the number of edges from id up to its subtree root, following
// ParentID links. A corrupted link (missing parent) stops the walk; a stored
// cycle (should never happen — Attach rejects them) is bounded.
func (h *Hierarchy) Depth(id string) (int, error) { return depthOf(h.agents, id) }

func depthOf(r agentReader, id string) (int, error) {
	depth := 0
	cur, err := agentByID(r, id)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{id: true}
	for cur.ParentID != "" {
		depth++
		if depth > maxAgentDepth+1 { // belt-and-braces bound
			return depth, ErrHierarchyCycle
		}
		parent, err := agentByID(r, cur.ParentID)
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
func (h *Hierarchy) Subtree(id string) ([]Agent, error) { return subtreeOf(h.agents, id) }

func subtreeOf(r agentReader, id string) ([]Agent, error) {
	root, err := agentByID(r, id)
	if err != nil {
		return nil, err
	}
	out := []Agent{root}
	// R4-S3#3 defense-in-depth: a visited set makes the BFS terminate even
	// if a cycle were ever stored (e.g. written before the Attach mutex
	// existed); without it the queue never drains and `out` grows until the
	// service OOMs on every /downline read.
	visited := map[string]bool{root.ID: true}
	queue := []Agent{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		// Perf H2: per-node indexed child lookup (parent_id secondary index)
		// instead of one full agents-table scan per Subtree call (measured
		// 93.6 ms @10k agents before).
		children, err := r.ChildrenOf(cur.ID)
		if err != nil {
			return nil, err
		}
		for _, ch := range children {
			if visited[ch.ID] {
				continue
			}
			visited[ch.ID] = true
			out = append(out, ch)
			queue = append(queue, ch)
		}
	}
	return out, nil
}

// Attach links childID under parentID (or detaches when parentID == "").
// Checks: both agents exist, same tenant, no cycle, depth cap.
//
// R4-S3#3: the whole check-then-act (cycle check via Subtree, depth check
// via Depth, then the Put) runs under h.mu so a concurrent Attach can never
// interleave between the checks and the write — a cycle can no longer be
// stored by racing re-parents.
//
// R4-9b: h.mu is per-PROCESS — replicas sharing one Postgres could still
// interleave. On the Postgres backend Attach therefore runs inside a single
// DB transaction that row-locks the child and parent agent rows with
// SELECT ... FOR UPDATE in id-sorted (deadlock-free) order and performs all
// cycle/depth validation against the same transaction snapshot before
// writing. Concurrent cross-attaches (A→B while B→A) now serialise at the
// database: the loser re-reads the winner's committed links and its cycle
// check rejects the move. The mutex stays as the in-process fast path and
// as the enforcer on the embedded dev backend.
func (h *Hierarchy) Attach(childID, parentID string) (Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.agents.st.IsPostgres() {
		return h.attachPostgres(childID, parentID)
	}
	return h.attachChecked(h.agents, childID, parentID,
		func(ag Agent) error { return h.agents.st.Put("agents", ag.ID, ag) })
}

// attachChecked is the shared check-then-act: tenant, cycle and depth
// validation against reader r, then the parent-link write via put. r and
// put MUST be atomic with respect to other attaches (in-process mutex on
// the embedded backend; one DB transaction with locked rows on Postgres).
func (h *Hierarchy) attachChecked(r agentReader, childID, parentID string, put func(Agent) error) (Agent, error) {
	child, err := agentByID(r, childID)
	if err != nil {
		return Agent{}, err
	}
	if parentID == "" {
		child.ParentID = ""
		child.UpdatedAt = nowRFC3339()
		return child, put(child)
	}
	parent, err := agentByID(r, parentID)
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
	sub, err := subtreeOf(r, childID)
	if err != nil {
		return Agent{}, err
	}
	for _, d := range sub {
		if d.ID == parentID {
			return Agent{}, ErrHierarchyCycle
		}
	}
	// Depth check: parent depth + child subtree height must stay within cap.
	parentDepth, err := depthOf(r, parentID)
	if err != nil {
		return Agent{}, err
	}
	height := subtreeHeight(child.ID, sub)
	if parentDepth+1+height > maxAgentDepth {
		return Agent{}, ErrDepthCap
	}
	child.ParentID = parentID
	child.UpdatedAt = nowRFC3339()
	return child, put(child)
}

// pgAgentReader adapts a store.PgTx to agentReader so Attach validation
// reads inside the same transaction (and snapshot) as the row locks.
type pgAgentReader struct {
	ctx context.Context
	tx  *store.PgTx
}

func (p pgAgentReader) Get(id string) (Agent, bool, error) {
	var ag Agent
	ok, err := p.tx.Get(p.ctx, "agents", id, &ag)
	return ag, ok, err
}

func (p pgAgentReader) List() ([]Agent, error) {
	var out []Agent
	if err := p.tx.List(p.ctx, "agents", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ChildrenOf reads a node's direct children inside the Attach transaction
// via the meridian_agents_parent_id expression index (perf H3: the cycle/
// depth validation no longer full-scans the agents table while holding the
// FOR UPDATE row locks, so lock hold time no longer grows with table size).
func (p pgAgentReader) ChildrenOf(parentID string) ([]Agent, error) {
	var out []Agent
	if err := p.tx.ListWhere(p.ctx, "agents", "parent_id", parentID, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachPostgres runs Attach inside one DB transaction with the involved
// agent rows locked FOR UPDATE in id-sorted order (R4-9b). Under READ
// COMMITTED a blocked FOR UPDATE re-reads the winner's committed row
// versions once the lock is granted, and every later statement sees the
// latest committed data — so the losing cross-attach validates against the
// post-commit hierarchy and its cycle check fails instead of storing a
// 2-cycle.
func (h *Hierarchy) attachPostgres(childID, parentID string) (Agent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := h.agents.st.BeginTx(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer tx.Rollback()
	// Lock the involved rows in deterministic (id-sorted) order: every
	// Attach transaction requests the same locks in the same order, so
	// concurrent cross-attaches serialise instead of deadlocking.
	lockIDs := []string{childID}
	if parentID != "" && parentID != childID {
		lockIDs = append(lockIDs, parentID)
	}
	sort.Strings(lockIDs)
	for _, id := range lockIDs {
		var ag Agent
		ok, err := tx.GetForUpdate(ctx, "agents", id, &ag)
		if err != nil {
			return Agent{}, err
		}
		if !ok {
			return Agent{}, fmt.Errorf("agent %s not found", id)
		}
	}
	reader := pgAgentReader{ctx: ctx, tx: tx}
	out, err := h.attachChecked(reader, childID, parentID,
		func(ag Agent) error { return tx.Put(ctx, "agents", ag.ID, ag) })
	if err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Agent{}, err
	}
	return out, nil
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
	// R4-S3#3 defense-in-depth (mirrors the Subtree BFS visited set): the
	// recursion must terminate even on a PRE-EXISTING stored cycle (written
	// before the Attach mutex existed); without a guard a legacy cycle
	// recurses forever here and overflows the stack on the next Attach.
	// `onPath` cuts the current DFS path at a repeated node (returning a
	// bounded height) while still allowing shared descendants in a diamond
	// DAG to be counted from every path — so the height stays exact for
	// acyclic data and merely bounded for corrupted data.
	onPath := map[string]bool{}
	var walk func(id string) int
	walk = func(id string) int {
		if onPath[id] {
			return 0 // stored cycle: stop this path instead of overflowing
		}
		onPath[id] = true
		max := 0
		for _, ch := range children[id] {
			if d := walk(ch) + 1; d > max {
				max = d
			}
		}
		delete(onPath, id)
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
