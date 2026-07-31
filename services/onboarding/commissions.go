package main

import (
	"net/http"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// commissions.go — server-side commission computation (audit HIGH #2 fix:
// commissions were computed in the browser against a spoofable localStorage
// agent id). The rule rate comes from the service (rule pack constant), the
// population from the server registry, and the agent identity from the
// authenticated principal — never from client-supplied localStorage.

// CommissionSummary is the server-computed commission position of one agent.
type CommissionSummary struct {
	AgentID         string `json:"agent_id"`
	Captured        int    `json:"captured"`
	Verified        int    `json:"verified"`
	AccruedKobo     uint64 `json:"accrued_kobo"`
	RateKobo        uint64 `json:"rate_kobo"`
	RulePackVersion string `json:"rule_pack_version"`
}

// CommissionSummaryFor computes the summary for agentID from the registry:
// verified operators (nin_verified|tin_provisioned|graduated) x the pack rate.
func CommissionSummaryFor(reg *Registry, agentID string) (CommissionSummary, error) {
	ops, err := reg.List()
	if err != nil {
		return CommissionSummary{}, err
	}
	sum := CommissionSummary{AgentID: agentID, RateKobo: commissionPerVerifiedKobo, RulePackVersion: "rp-commissions-ng@1.0.0"}
	for _, op := range ops {
		if op.AgentID != agentID {
			continue
		}
		sum.Captured++
		switch op.Status {
		case "nin_verified", "tin_provisioned", "graduated":
			sum.Verified++
		}
	}
	sum.AccruedKobo = uint64(sum.Verified) * sum.RateKobo
	return sum, nil
}

// commissionSummary handles GET /v1/commissions/summary. The agent identity
// comes from the authenticated request (JWT sub in prod; X-Dev-Agent-Id only
// in AUTH_MODE=dev) — a caller can never read another agent's commissions by
// editing localStorage.
func (s *server) commissionSummary(w http.ResponseWriter, r *http.Request) {
	agentID := httpx.RequestIdentity(r)
	if agentID == "" {
		httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", "agent identity required (Bearer sub or dev X-Dev-Agent-Id)")
		return
	}
	sum, err := CommissionSummaryFor(s.registry, agentID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}
