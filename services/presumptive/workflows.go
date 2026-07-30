package main

import (
	"fmt"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// PSMWorkflowRun records a wf-psm-* execution.
type PSMWorkflowRun struct {
	ID        string   `json:"id"`
	Workflow  string   `json:"workflow"`
	Input     any      `json:"input,omitempty"`
	Steps     []string `json:"steps"`
	Status    string   `json:"status"` // completed|failed
	Error     string   `json:"error,omitempty"`
	Result    any      `json:"result,omitempty"`
	StartedAt string   `json:"started_at"`
	EndedAt   string   `json:"ended_at"`
}

// PSMWorkflows is the wf-psm-* registry (dev in-process runner).
type PSMWorkflows struct {
	st     *store.Store
	pay    *PaymentService
	float  *FloatService
	engine *BandEngine
	gates  *GateClient
	lc     ledger.Client
	bus    events.Bus
}

func NewPSMWorkflows(st *store.Store, pay *PaymentService, fl *FloatService, eng *BandEngine, gates *GateClient, lc ledger.Client, bus events.Bus) *PSMWorkflows {
	return &PSMWorkflows{st: st, pay: pay, float: fl, engine: eng, gates: gates, lc: lc, bus: bus}
}

func (w *PSMWorkflows) Names() []string {
	return []string{
		"wf-psm-payment",
		"wf-psm-float-monitor",
		"wf-psm-settlement",
		"wf-psm-simulation",
		"wf-psm-pack-rollout",
		"wf-psm-gate-flip",
	}
}

func (w *PSMWorkflows) record(name string, input any, fn func(run *PSMWorkflowRun) error) PSMWorkflowRun {
	run := PSMWorkflowRun{ID: ids.WithPrefix("run"), Workflow: name, Input: input, StartedAt: nowRFC3339(), Status: "completed"}
	if err := fn(&run); err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	}
	run.EndedAt = nowRFC3339()
	_ = w.st.Put("workflow_runs", run.ID, run)
	return run
}

func (w *PSMWorkflows) step(run *PSMWorkflowRun, format string, args ...any) {
	run.Steps = append(run.Steps, fmt.Sprintf(format, args...))
}

// Run dispatches a workflow by name.
func (w *PSMWorkflows) Run(name string, input map[string]any) (PSMWorkflowRun, error) {
	switch name {
	case "wf-psm-payment":
		return w.PaymentFlow(input), nil
	case "wf-psm-float-monitor":
		return w.FloatMonitor(), nil
	case "wf-psm-settlement":
		return w.Settlement(), nil
	case "wf-psm-simulation":
		return w.Simulate(input), nil
	case "wf-psm-pack-rollout":
		packID, _ := input["pack_id"].(string)
		return w.PackRollout(packID), nil
	case "wf-psm-gate-flip":
		gateID, _ := input["gate_id"].(string)
		open, _ := input["open"].(bool)
		return w.GateFlip(gateID, open), nil
	default:
		return PSMWorkflowRun{}, fmt.Errorf("unknown workflow %q (have %v)", name, w.Names())
	}
}

// PaymentFlow (wf-psm-payment): full saga intent -> authorise -> capture.
func (w *PSMWorkflows) PaymentFlow(input map[string]any) PSMWorkflowRun {
	return w.record("wf-psm-payment", input, func(run *PSMWorkflowRun) error {
		in := IntentRequest{
			TINHash:            strOf(input, "tin_hash"),
			State:              strOf(input, "state"),
			TradeCategory:      strOf(input, "trade_category"),
			AnnualTurnoverKobo: uintOf(input, "annual_turnover_kobo"),
			Period:             strOf(input, "period"),
			Provider:           strOf(input, "provider"),
		}
		p, err := w.pay.CreateIntent(in)
		if err != nil {
			return err
		}
		w.step(run, "intent %s created: %d kobo via %s (pending transfer %s)", p.ID, p.AmountKobo, p.Provider, p.PendingTransferID[:12])
		p, auth, err := w.pay.Authorise(p.ID)
		if err != nil {
			return err
		}
		if auth.Status != "authorised" {
			return fmt.Errorf("authorisation failed: %s", auth.Detail)
		}
		w.step(run, "authorised by %s ref %s", p.Provider, auth.Reference)
		p, cert, err := w.pay.Capture(p.ID)
		if err != nil {
			return err
		}
		w.step(run, "captured; certificate %s issued", cert.Serial)
		run.Result = map[string]any{"payment": p, "certificate": cert}
		return nil
	})
}

// FloatMonitor (wf-psm-float-monitor): alerts on low float balances.
func (w *PSMWorkflows) FloatMonitor() PSMWorkflowRun {
	return w.record("wf-psm-float-monitor", nil, func(run *PSMWorkflowRun) error {
		var accounts []FloatAccount
		if err := w.st.List("float_accounts", &accounts); err != nil {
			return err
		}
		alerts := 0
		for _, fa := range accounts {
			bal, err := w.lc.Balance(fa.AccountID)
			if err != nil {
				return err
			}
			available := bal.NetPosted()
			w.step(run, "agent %s float: net posted %d kobo", fa.AgentID, available)
			if available < int64(floatLowThresholdKobo) {
				alerts++
				w.bus.Publish("nrs.psm.float.low.v1", events.New("nrs.psm.float.low.v1", serviceName, "", "", map[string]any{
					"agent_id": fa.AgentID, "available_kobo": available, "threshold_kobo": floatLowThresholdKobo,
				}))
			}
		}
		run.Result = map[string]any{"agents_checked": len(accounts), "low_float_alerts": alerts}
		return nil
	})
}

// Settlement (wf-psm-settlement): reconcile captured payments vs the ledger
// collections account and report settled totals (3-way recon input feed).
func (w *PSMWorkflows) Settlement() PSMWorkflowRun {
	return w.record("wf-psm-settlement", nil, func(run *PSMWorkflowRun) error {
		payments, err := w.pay.List()
		if err != nil {
			return err
		}
		var capturedTotal, voidedTotal uint64
		captured := 0
		for _, p := range payments {
			switch p.Status {
			case "captured":
				captured++
				capturedTotal += p.AmountKobo
			case "voided", "failed":
				voidedTotal += p.AmountKobo
			}
		}
		collections, err := w.pay.collectionsAccountID()
		if err != nil {
			return err
		}
		bal, err := w.lc.Balance(collections)
		if err != nil {
			return err
		}
		ledgerPosted := bal.CreditsPosted
		breaks := int64(capturedTotal) - int64(ledgerPosted)
		w.step(run, "captured %d payments totalling %d kobo; ledger collections credits_posted %d kobo; breaks %d",
			captured, capturedTotal, ledgerPosted, breaks)
		res := map[string]any{
			"captured_count": captured, "captured_kobo": capturedTotal,
			"voided_kobo": voidedTotal, "ledger_posted_kobo": ledgerPosted,
			"recon_breaks_kobo": breaks,
		}
		w.bus.Publish("nrs.psm.settlement.v1", events.New("nrs.psm.settlement.v1", serviceName, "", "", res))
		run.Result = res
		return nil
	})
}

// Simulate (wf-psm-simulation): runs band scenarios over operator cohorts and
// persists the results.
func (w *PSMWorkflows) Simulate(input map[string]any) PSMWorkflowRun {
	return w.record("wf-psm-simulation", input, func(run *PSMWorkflowRun) error {
		type cohortMember struct {
			OperatorRef        string `json:"operator_ref"`
			State              string `json:"state"`
			TradeCategory      string `json:"trade_category"`
			AnnualTurnoverKobo uint64 `json:"annual_turnover_kobo"`
		}
		var cohort []cohortMember
		raw, ok := input["cohort"]
		if !ok {
			return fmt.Errorf("input.cohort (array of {operator_ref,state,trade_category,annual_turnover_kobo}) is required")
		}
		b, _ := jsonMarshal(raw)
		if err := jsonUnmarshal(b, &cohort); err != nil {
			return fmt.Errorf("cohort parse: %w", err)
		}
		if len(cohort) == 0 {
			return fmt.Errorf("cohort must not be empty")
		}
		sim := Simulation{ID: ids.WithPrefix("sim"), Totals: map[string]uint64{}, CreatedAt: nowRFC3339()}
		var totalLevy uint64
		for _, m := range cohort {
			eval := w.engine.Evaluate(m.State, m.TradeCategory, m.AnnualTurnoverKobo, false, 0)
			row := SimulationRow{
				OperatorRef: m.OperatorRef, State: m.State, TradeCategory: m.TradeCategory,
				TurnoverKobo: m.AnnualTurnoverKobo, Band: eval.Band, AnnualLevyKobo: eval.AnnualLevyKobo,
				Exempt: eval.Exempt, PackID: eval.PackID,
			}
			sim.Results = append(sim.Results, row)
			sim.Totals[eval.PackID] += eval.AnnualLevyKobo
			totalLevy += eval.AnnualLevyKobo
			w.step(run, "%s: %s/%s turnover %d -> band %s levy %d kobo (%s)",
				m.OperatorRef, m.State, m.TradeCategory, m.AnnualTurnoverKobo, eval.Band, eval.AnnualLevyKobo, eval.PackID)
		}
		sim.Scenarios = len(sim.Results)
		sim.Totals["grand_total"] = totalLevy
		if err := w.st.Put("simulations", sim.ID, sim); err != nil {
			return err
		}
		w.bus.Publish("nrs.psm.simulation.v1", events.New("nrs.psm.simulation.v1", serviceName, "", "", map[string]any{
			"simulation_id": sim.ID, "scenarios": sim.Scenarios, "total_levy_kobo": totalLevy,
		}))
		run.Result = sim
		return nil
	})
}

// PackRollout (wf-psm-pack-rollout): validates a pack is loaded + effective
// and records the rollout decision (simulation status gate).
func (w *PSMWorkflows) PackRollout(packID string) PSMWorkflowRun {
	return w.record("wf-psm-pack-rollout", map[string]string{"pack_id": packID}, func(run *PSMWorkflowRun) error {
		packs := w.engine.Packs()
		for _, p := range packs {
			if p.ID == packID {
				if p.Status != "published" {
					return fmt.Errorf("pack %s status is %q; only published packs roll out", p.ID, p.Status)
				}
				w.step(run, "pack %s@%s effective %s -> active (subject_to_regazette=%v)",
					p.ID, p.Version, p.EffectiveFrom, p.SubjectToRegazette)
				w.bus.Publish("nrs.psm.pack.rolledout.v1", events.New("nrs.psm.pack.rolledout.v1", serviceName, "", p.ID+"@"+p.Version, map[string]any{
					"pack_id": p.ID, "version": p.Version,
				}))
				run.Result = map[string]any{"pack_id": p.ID, "version": p.Version, "rolled_out": true}
				return nil
			}
		}
		return fmt.Errorf("pack %q not loaded", packID)
	})
}

// GateFlip (wf-psm-gate-flip): flips a reg-watch gate (board-authorized dev).
func (w *PSMWorkflows) GateFlip(gateID string, open bool) PSMWorkflowRun {
	return w.record("wf-psm-gate-flip", map[string]any{"gate_id": gateID, "open": open}, func(run *PSMWorkflowRun) error {
		if gateID == "" {
			gateID = presumptiveGateID
		}
		gs, err := w.gates.Flip(gateID, open)
		if err != nil {
			return err
		}
		w.step(run, "gate %s -> open=%v (source %s)", gs.ID, gs.Open, gs.Source)
		w.bus.Publish("nrs.psm.gate.flipped.v1", events.New("nrs.psm.gate.flipped.v1", serviceName, "", "", map[string]any{
			"gate_id": gs.ID, "open": gs.Open,
		}))
		run.Result = gs
		return nil
	})
}

func strOf(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func uintOf(m map[string]any, k string) uint64 {
	switch v := m[k].(type) {
	case float64:
		return uint64(v)
	case uint64:
		return v
	case int64:
		return uint64(v)
	}
	return 0
}
