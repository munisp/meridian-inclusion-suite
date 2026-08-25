package main

import (
	"fmt"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

// Agent is a first-class field-agent record (audit O5): server-issued
// agent_id, KYC/licence details, vetting status and an optional market
// association link. Operators reference agents by the issued agent_id —
// free-text agent ids ("unknown-agent") are rejected in profile=prod.
type Agent struct {
	ID            string `json:"id"` // ag_xxx, issued server-side
	FullName      string `json:"full_name"`
	Phone         string `json:"phone"`
	LicenseNo     string `json:"license_no,omitempty"` // agent licence / NIN reference
	State         string `json:"state"`                // operating scope
	LGA           string `json:"lga"`
	AssociationID string `json:"association_id,omitempty"` // market association link
	// I6: hierarchy fields. TenantID scopes the agent to a tenant (all
	// hierarchy/commission operations are tenant-isolated); ParentID links
	// the agent to its upline parent ("" = root of its own subtree).
	TenantID      string `json:"tenant_id,omitempty"`
	ParentID      string `json:"parent_id,omitempty"`
	VettingStatus string `json:"vetting_status"` // pending|approved|suspended|rejected
	Notes         string `json:"notes,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// Agent vetting transitions.
var agentVettingTransitions = map[string][]string{
	"pending":   {"approved", "rejected"},
	"approved":  {"suspended"},
	"suspended": {"approved", "rejected"},
	"rejected":  {"pending"},
}

// AgentRegistry is the agent store.
type AgentRegistry struct{ st *store.Store }

func NewAgentRegistry(st *store.Store) *AgentRegistry { return &AgentRegistry{st: st} }

// Register creates an agent in vetting state "pending" with a server-issued id.
func (a *AgentRegistry) Register(in Agent) (Agent, error) {
	if strings.TrimSpace(in.FullName) == "" || strings.TrimSpace(in.Phone) == "" {
		return Agent{}, fmt.Errorf("full_name and phone are required")
	}
	in.ID = ids.WithPrefix("ag")
	if in.TenantID == "" {
		in.TenantID = DefaultTenant
	}
	in.ParentID = "" // hierarchy links are made via Attach (cycle/depth-checked)
	in.VettingStatus = "pending"
	in.CreatedAt = nowRFC3339()
	in.UpdatedAt = in.CreatedAt
	return in, a.st.Put("agents", in.ID, in)
}

func (a *AgentRegistry) Get(id string) (Agent, bool, error) {
	var ag Agent
	ok, err := a.st.Get("agents", id, &ag)
	return ag, ok, err
}

func (a *AgentRegistry) List() ([]Agent, error) {
	var out []Agent
	if err := a.st.List("agents", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetVetting moves an agent through the vetting state machine.
func (a *AgentRegistry) SetVetting(id, to, notes string) (Agent, error) {
	ag, ok, err := a.Get(id)
	if err != nil || !ok {
		return Agent{}, fmt.Errorf("agent %s not found", id)
	}
	allowed := false
	for _, t := range agentVettingTransitions[ag.VettingStatus] {
		if t == to {
			allowed = true
		}
	}
	if !allowed {
		return Agent{}, fmt.Errorf("illegal vetting transition %s -> %s", ag.VettingStatus, to)
	}
	ag.VettingStatus = to
	if notes != "" {
		ag.Notes = notes
	}
	ag.UpdatedAt = nowRFC3339()
	return ag, a.st.Put("agents", ag.ID, ag)
}

// ValidateForCapture checks an agent_id presented on operator capture.
// Rules:
//   - "" / "unknown-agent" / free-text: rejected in profile=prod; allowed in
//     dev with a warning flag (offline capture can precede agent sync).
//   - known agent: must be vetting "approved" (suspended agents cannot
//     capture); unknown id is rejected in prod, allowed+flagged in dev.
func (a *AgentRegistry) ValidateForCapture(agentID string) (warning string, err error) {
	if agentID == "" || agentID == "unknown-agent" {
		if workflowx.IsProdProfile() {
			return "", fmt.Errorf("agent_id is required and must reference a registered agent (unknown-agent rejected in prod profile)")
		}
		return "no registered agent referenced; accepted in dev profile only", nil
	}
	ag, ok, gerr := a.Get(agentID)
	if gerr != nil {
		return "", gerr
	}
	if !ok {
		if workflowx.IsProdProfile() {
			return "", fmt.Errorf("unknown agent_id %q", agentID)
		}
		return "agent_id not in registry; accepted in dev profile only", nil
	}
	if ag.VettingStatus != "approved" {
		return "", fmt.Errorf("agent %s vetting status is %q (must be approved)", agentID, ag.VettingStatus)
	}
	return "", nil
}
