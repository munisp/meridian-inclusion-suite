package main

import (
	"net/http"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
)

// risk.go — agent liquidity/float risk scoring (I17). Computes a 0-100 risk
// score from float utilization, cash-in/out velocity and dormancy, and emits
// low-float alert events. REAL: computed from the float ledger movements.

// FloatRisk is the liquidity risk assessment for one agent.
type FloatRisk struct {
	AgentID         string  `json:"agent_id"`
	BalanceKobo     int64   `json:"balance_kobo"`
	UtilizationPct  float64 `json:"utilization_pct"` // debits / credits lifetime
	Velocity24h     int     `json:"velocity_24h"`    // movements in the last 24h
	DormancyDays    int     `json:"dormancy_days"`   // days since last movement
	Score           int     `json:"score"`           // 0 (healthy) .. 100 (critical)
	Band            string  `json:"band"`            // low|medium|high|critical
	LowFloatAlert   bool    `json:"low_float_alert"`
	RulePackVersion string  `json:"rule_pack_version"`
}

const lowFloatThresholdKobo int64 = 500000 // ₦5,000 (matches the float monitor)

// AssessFloatRisk scores an agent from its float balance + movements.
func AssessFloatRisk(agentID string, bal ledger.Balance, movements []FloatMovement, now time.Time) FloatRisk {
	r := FloatRisk{AgentID: agentID, BalanceKobo: bal.NetPosted(), RulePackVersion: "rp-agent-risk-ng@1.0.0"}
	if bal.CreditsPosted > 0 {
		r.UtilizationPct = float64(bal.DebitsPosted) / float64(bal.CreditsPosted) * 100
	}
	var last time.Time
	for _, m := range movements {
		ts, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			continue
		}
		if now.Sub(ts) < 24*time.Hour {
			r.Velocity24h++
		}
		if ts.After(last) {
			last = ts
		}
	}
	if !last.IsZero() {
		r.DormancyDays = int(now.Sub(last).Hours() / 24)
	} else {
		r.DormancyDays = 999
	}
	// score: utilization 40%, dormancy 40%, low balance 20%
	score := 0.0
	score += min64(r.UtilizationPct, 100) * 0.4
	if r.DormancyDays >= 30 {
		score += 40
	} else {
		score += float64(r.DormancyDays) / 30.0 * 40
	}
	if r.BalanceKobo < lowFloatThresholdKobo {
		score += 20
		r.LowFloatAlert = true
	}
	r.Score = int(score + 0.5)
	switch {
	case r.Score >= 80:
		r.Band = "critical"
	case r.Score >= 60:
		r.Band = "high"
	case r.Score >= 35:
		r.Band = "medium"
	default:
		r.Band = "low"
	}
	return r
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// floatRisk handles GET /v1/float/{agent}/risk and emits a low-float alert
// event when the balance is below threshold or the score is critical.
func (s *server) floatRisk(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	bal, err := s.float.Balance(agent)
	if err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	mv, _ := s.float.Movements(agent)
	risk := AssessFloatRisk(agent, bal, mv, time.Now().UTC())
	if risk.LowFloatAlert || risk.Band == "critical" {
		_ = s.bus.Publish("nrs.psm.float.v1", events.New("nrs.psm.float.v1", serviceName, "", risk.RulePackVersion, map[string]any{
			"agent_id": agent, "event": "low_float_alert", "balance_kobo": risk.BalanceKobo,
			"score": risk.Score, "band": risk.Band,
		}))
	}
	httpx.WriteJSON(w, http.StatusOK, risk)
}
