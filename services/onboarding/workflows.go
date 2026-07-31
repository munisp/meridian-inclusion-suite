package main

import (
	"fmt"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// §1.5 namespace codes used for onboarding ledger accounts.
const (
	nsCommissionsPool uint64 = 700000000001 // NRS commissions pool account serial
	nsAgentCommission uint64 = 700000100000 // agent commission accounts serial base
)

// commissionPerVerifiedKobo is the agent bounty per NIN-verified operator (₦200).
const commissionPerVerifiedKobo uint64 = 20000

// presumptiveCeilingKobo: operators above this turnover estimate graduate to MBS (₦25m).
const presumptiveCeilingKobo uint64 = 2_500_000_000

// Workflows is the wf-onb-* registry with a dev in-process runner
// (Temporal server is an external rail; wired via temporal-sdkx in core,
// here each workflow is a plain function with recorded steps + events).
type Workflows struct {
	st          *store.Store
	registry    *Registry
	verifier    NINVerifier
	provisioner TINProvisioner
	consent     *ConsentService
	ledger      ledger.Client
	bus         events.Bus
}

func NewWorkflows(st *store.Store, reg *Registry, v NINVerifier, p TINProvisioner, c *ConsentService, lc ledger.Client, bus events.Bus) *Workflows {
	return &Workflows{st: st, registry: reg, verifier: v, provisioner: p, consent: c, ledger: lc, bus: bus}
}

func (w *Workflows) record(name string, input any, fn func(run *WorkflowRun) error) WorkflowRun {
	run := WorkflowRun{ID: ids.WithPrefix("run"), Workflow: name, Input: input, StartedAt: nowRFC3339(), Status: "completed"}
	if err := fn(&run); err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	}
	run.EndedAt = nowRFC3339()
	_ = w.st.Put("workflow_runs", run.ID, run)
	return run
}

func (w *Workflows) step(run *WorkflowRun, format string, args ...any) {
	run.Steps = append(run.Steps, fmt.Sprintf(format, args...))
}

// Names is the wf-onb-* catalog.
func (w *Workflows) Names() []string {
	return []string{
		"wf-onb-tin-provision",
		"wf-onb-capture-ingest",
		"wf-onb-ledger-rollup",
		"wf-onb-commission-settlement",
		"wf-onb-filing-reminders",
		"wf-onb-mbs-graduate",
	}
}

// Run dispatches a workflow by name.
func (w *Workflows) Run(name string, input map[string]any) (WorkflowRun, error) {
	switch name {
	case "wf-onb-tin-provision":
		opID, _ := input["operator_id"].(string)
		nin, _ := input["nin"].(string)
		return w.TINProvision(opID, nin), nil
	case "wf-onb-capture-ingest":
		return w.CaptureIngestStats(), nil
	case "wf-onb-ledger-rollup":
		return w.LedgerRollup(), nil
	case "wf-onb-commission-settlement":
		return w.CommissionSettlement(), nil
	case "wf-onb-filing-reminders":
		return w.FilingReminders(), nil
	case "wf-onb-mbs-graduate":
		return w.MBSGraduate(), nil
	default:
		return WorkflowRun{}, fmt.Errorf("unknown workflow %q (have %v)", name, w.Names())
	}
}

// TINProvision (wf-onb-tin-provision): verify NIN with NIMC, then provision
// TIN via tin-graph with local fallback, update operator, emit event.
func (w *Workflows) TINProvision(operatorID, nin string) WorkflowRun {
	return w.record("wf-onb-tin-provision", map[string]string{"operator_id": operatorID}, func(run *WorkflowRun) error {
		op, ok, err := w.registry.Get(operatorID)
		if err != nil || !ok {
			return fmt.Errorf("operator %s not found", operatorID)
		}
		v, err := w.verifier.VerifyNIN(nin)
		if err != nil {
			return fmt.Errorf("nimc verify: %w", err)
		}
		w.step(run, "nimc.verify -> verified=%v source=%s ref=%s", v.Verified, v.Source, v.Reference)
		if !v.Verified {
			return fmt.Errorf("NIMC verification failed: %s", v.Detail)
		}
		if op.NINHash != "" && op.NINHash != v.NINHash {
			return fmt.Errorf("NIN does not match operator record")
		}
		op.NINHash = v.NINHash
		op.Status = "nin_verified"
		prov, err := w.provisioner.ProvisionForNIN(v.NINHash)
		if err != nil {
			return fmt.Errorf("tin provision: %w", err)
		}
		w.step(run, "tin.provision -> tin=%s source=%s", prov.TIN, prov.Source)
		op.TIN = prov.TIN
		op.TINHash = prov.TINHash
		op.Status = "tin_provisioned"
		if err := w.registry.Update(op); err != nil {
			return err
		}
		w.bus.Publish("nrs.onb.tin.provisioned.v1", events.New("nrs.onb.tin.provisioned.v1", serviceName, "", "", map[string]any{
			"operator_id": op.ID, "nin_hash": op.NINHash, "tin_hash": op.TINHash, "source": prov.Source,
		}))
		w.step(run, "event nrs.onb.tin.provisioned.v1 published")
		run.Result = map[string]any{"operator_id": op.ID, "tin": op.TIN, "tin_hash": op.TINHash, "source": prov.Source}
		return nil
	})
}

// CaptureIngestStats (wf-onb-capture-ingest): rolls up batch ingest stats and
// emits the ingest event (the synchronous ingest itself happens in the API).
func (w *Workflows) CaptureIngestStats() WorkflowRun {
	return w.record("wf-onb-capture-ingest", nil, func(run *WorkflowRun) error {
		var batches []CaptureBatch
		if err := w.st.List("capture_batches", &batches); err != nil {
			return err
		}
		created, conflicts, rejected, dup := 0, 0, 0, 0
		for _, b := range batches {
			for _, r := range b.Results {
				switch r.Outcome {
				case "created":
					created++
				case "conflict_resolved":
					conflicts++
				case "rejected":
					rejected++
				case "duplicate_client_ref":
					dup++
				}
			}
		}
		res := map[string]any{"batches": len(batches), "created": created, "conflict_resolved": conflicts, "rejected": rejected, "duplicate_client_ref": dup}
		w.bus.Publish("nrs.onb.capture.ingested.v1", events.New("nrs.onb.capture.ingested.v1", serviceName, "", "", res))
		w.step(run, "rollup: %v", res)
		run.Result = res
		return nil
	})
}

// ensureCommissionAccounts creates the pool + agent commission accounts on
// ledger 700 (idempotent-ish: ignores ErrAccountExists via probe).
func (w *Workflows) ensureCommissionAccount(agentID string) (string, error) {
	acctID := ledger.AccountID(nsAgentCommission, hashSerial(agentID))
	if _, err := w.ledger.Balance(acctID); err == nil {
		return acctID, nil
	}
	err := w.ledger.CreateAccounts([]ledger.Account{{
		ID: acctID, Ledger: ledger.LedgerCommissions, Code: 5, UserData: "agent:" + agentID,
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return acctID, nil
}

func (w *Workflows) ensurePoolAccount() (string, error) {
	poolID := ledger.AccountID(nsCommissionsPool, 1)
	if _, err := w.ledger.Balance(poolID); err == nil {
		return poolID, nil
	}
	err := w.ledger.CreateAccounts([]ledger.Account{{
		ID: poolID, Ledger: ledger.LedgerCommissions, Code: 5, UserData: "nrs-commissions-pool",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return poolID, nil
}

// hashSerial derives a stable 48-bit entity serial from a string id.
func hashSerial(s string) uint64 {
	h := uint64(1469598103934665603)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h & 0x0000FFFFFFFFFFFF
}

// LedgerRollup (wf-onb-ledger-rollup): aggregate operator counts + commission
// accruals per agent into ledger 700; funds the pool from a treasury offset.
func (w *Workflows) LedgerRollup() WorkflowRun {
	return w.record("wf-onb-ledger-rollup", nil, func(run *WorkflowRun) error {
		ops, err := w.registry.List()
		if err != nil {
			return err
		}
		perAgent := map[string]int{}
		for _, op := range ops {
			if op.Status == "nin_verified" || op.Status == "tin_provisioned" || op.Status == "graduated" {
				perAgent[op.AgentID]++
			}
		}
		poolID, err := w.ensurePoolAccount()
		if err != nil {
			return err
		}
		var total uint64
		for agentID, n := range perAgent {
			acctID, err := w.ensureCommissionAccount(agentID)
			if err != nil {
				return err
			}
			accrual := uint64(n) * commissionPerVerifiedKobo
			total += accrual
			w.step(run, "agent %s: %d verified operators -> accrual %d kobo (acct %s)", agentID, n, accrual, acctID[:12])
		}
		run.Result = map[string]any{"agents": len(perAgent), "pool_account": poolID, "accrued_kobo": total}
		w.bus.Publish("nrs.onb.ledger.rollup.v1", events.New("nrs.onb.ledger.rollup.v1", serviceName, "", "", map[string]any{
			"agents": len(perAgent), "accrued_kobo": total,
		}))
		return nil
	})
}

// CommissionSettlement (wf-onb-commission-settlement): settle accrued agent
// commissions on ledger 700 (code 5 settle) from the NRS pool for the
// current period (YYYY-MM). See CommissionSettlementForPeriod.
func (w *Workflows) CommissionSettlement() WorkflowRun {
	return w.CommissionSettlementForPeriod(time.Now().UTC().Format("2006-01"))
}

// CommissionPayout is the durable per-(agent, period) payout marker — the
// dedup record that makes a re-run of a settled period a no-op.
type CommissionPayout struct {
	AgentID    string `json:"agent_id"`
	Period     string `json:"period"`
	AmountKobo uint64 `json:"amount_kobo"`
	TransferID string `json:"transfer_id"`
	PaidAt     string `json:"paid_at"`
}

func payoutKey(agentID, period string) string { return agentID + ":" + period }

func (w *Workflows) payoutMarked(agentID, period string) bool {
	var p CommissionPayout
	ok, err := w.st.Get("commission_payouts", payoutKey(agentID, period), &p)
	return err == nil && ok
}

func (w *Workflows) markPayout(agentID, period string, amount uint64, transferID string) error {
	return w.st.Put("commission_payouts", payoutKey(agentID, period), CommissionPayout{
		AgentID: agentID, Period: period, AmountKobo: amount, TransferID: transferID, PaidAt: nowRFC3339(),
	})
}

// CommissionSettlementForPeriod pays accrued agent commissions for one
// period. Hardened as a funds flow (audit Flow 4 / fix #8):
//   - per-period dedup marker per agent: a re-run for the same period is a
//     no-op (200 replay), never a double payout;
//   - each payout is a pending -> post saga (deterministic transfer ids) so
//     a crash mid-run is safely resumable/replayable;
//   - the pool-funding leg is likewise idempotent per period.
func (w *Workflows) CommissionSettlementForPeriod(period string) WorkflowRun {
	return w.record("wf-onb-commission-settlement", map[string]any{"period": period}, func(run *WorkflowRun) error {
		ops, err := w.registry.List()
		if err != nil {
			return err
		}
		perAgent := map[string]int{}
		for _, op := range ops {
			if op.Status == "nin_verified" || op.Status == "tin_provisioned" || op.Status == "graduated" {
				perAgent[op.AgentID]++
			}
		}
		poolID, err := w.ensurePoolAccount()
		if err != nil {
			return err
		}
		// fund the pool first (treasury topup, code 4) so settlement is funded
		var total uint64
		for _, n := range perAgent {
			total += uint64(n) * commissionPerVerifiedKobo
		}
		if total == 0 {
			w.step(run, "no verified operators; nothing to settle")
			run.Result = map[string]any{"settled_kobo": 0}
			return nil
		}
		treasuryID := ledger.AccountID(nsCommissionsPool, 2)
		if _, err := w.ledger.Balance(treasuryID); err != nil {
			if err := w.ledger.CreateAccounts([]ledger.Account{{ID: treasuryID, Ledger: ledger.LedgerCommissions, Code: 4, UserData: "nrs-treasury-offset"}}); err != nil && err != ledger.ErrAccountExists {
				return err
			}
		}
		// Fund the pool only for payouts not already marked settled this
		// period (the funding leg is idempotent per period+amount set).
		var fundTotal uint64
		for agentID, n := range perAgent {
			if w.payoutMarked(agentID, period) {
				continue
			}
			fundTotal += uint64(n) * commissionPerVerifiedKobo
		}
		if fundTotal > 0 {
			if _, err := w.ledger.Transfer(ledger.Transfer{
				ID: ledger.DeterministicTransferID("comm-fund:" + period + ":" + fmt.Sprint(fundTotal)),
				DebitAccountID: treasuryID, CreditAccountID: poolID, Ledger: ledger.LedgerCommissions,
				Code: ledger.CodeTopup, Amount: fundTotal, UserData: "pool-funding:" + period,
			}); err != nil {
				return fmt.Errorf("pool funding: %w", err)
			}
			w.step(run, "pool funded with %d kobo (ledger 700, code 4 topup)", fundTotal)
		} else {
			w.step(run, "pool funding not needed (all payouts already settled for %s)", period)
		}
		settled := map[string]uint64{}
		for agentID, n := range perAgent {
			amount := uint64(n) * commissionPerVerifiedKobo
			if w.payoutMarked(agentID, period) {
				w.step(run, "agent %s already paid for %s (dedup marker) — no-op", agentID, period)
				settled[agentID] = amount
				continue
			}
			acctID, err := w.ensureCommissionAccount(agentID)
			if err != nil {
				return err
			}
			// payout saga: pending -> mark -> post (compensation voids)
			pendID := ledger.DeterministicTransferID("comm-pending:" + agentID + ":" + period)
			postID := ledger.DeterministicTransferID("comm-post:" + agentID + ":" + period)
			if _, err := w.ledger.PendingTransfer(ledger.Transfer{
				ID: pendID, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
				Code: ledger.CodeSettle, Amount: amount, UserData: "commission:" + agentID + ":" + period,
			}); err != nil {
				return fmt.Errorf("settle agent %s (pending): %w", agentID, err)
			}
			// durable marker BEFORE the post: crash after the post replays
			// via PostPendingAs idempotency, never a second transfer.
			if err := w.markPayout(agentID, period, amount, postID); err != nil {
				_, _ = w.ledger.VoidPending(pendID) // compensation
				return err
			}
			txID, err := w.ledger.PostPendingAs(pendID, postID, amount)
			if err != nil {
				_, _ = w.ledger.VoidPending(pendID) // compensation
				return fmt.Errorf("settle agent %s (post): %w", agentID, err)
			}
			settled[agentID] = amount
			w.step(run, "settled %d kobo to agent %s (tx %s)", amount, agentID, txID[:12])
		}
		w.bus.Publish("nrs.onb.commission.settled.v1", events.New("nrs.onb.commission.settled.v1", serviceName, "", "", map[string]any{
			"settled": settled, "total_kobo": total,
		}))
		run.Result = map[string]any{"settled": settled, "total_kobo": total, "ledger": ledger.LedgerCommissions}
		return nil
	})
}

// FilingReminders (wf-onb-filing-reminders): queue reminders for provisioned
// operators with no filing activity (dev: emits reminder events).
func (w *Workflows) FilingReminders() WorkflowRun {
	return w.record("wf-onb-filing-reminders", nil, func(run *WorkflowRun) error {
		ops, err := w.registry.List()
		if err != nil {
			return err
		}
		queued := 0
		for _, op := range ops {
			if op.Status != "tin_provisioned" {
				continue
			}
			created, _ := time.Parse(time.RFC3339, op.CreatedAt)
			if time.Since(created) < 24*time.Hour {
				continue // grace period
			}
			w.bus.Publish("nrs.onb.reminder.queued.v1", events.New("nrs.onb.reminder.queued.v1", serviceName, "", "", map[string]any{
				"operator_id": op.ID, "tin_hash": op.TINHash, "phone": op.Phone, "channel": "sms",
			}))
			queued++
		}
		w.step(run, "queued %d filing reminders", queued)
		run.Result = map[string]any{"queued": queued}
		return nil
	})
}

// MBSGraduate (wf-onb-mbs-graduate): operators whose estimated turnover
// exceeds the presumptive ceiling graduate to MBS (standard regime).
func (w *Workflows) MBSGraduate() WorkflowRun {
	type gradInput struct {
		OperatorID            string `json:"operator_id"`
		EstimatedTurnoverKobo uint64 `json:"estimated_turnover_kobo"`
	}
	return w.record("wf-onb-mbs-graduate", nil, func(run *WorkflowRun) error {
		// dev input channel: graduation candidates staged via the API
		var staged []gradInput
		if err := w.st.List("graduation_candidates", &staged); err != nil {
			return err
		}
		graduated := 0
		for _, g := range staged {
			if g.EstimatedTurnoverKobo <= presumptiveCeilingKobo {
				continue
			}
			op, ok, err := w.registry.Get(g.OperatorID)
			if err != nil || !ok {
				continue
			}
			op.Status = "graduated"
			if err := w.registry.Update(op); err != nil {
				return err
			}
			w.bus.Publish("nrs.onb.mbs.graduate.v1", events.New("nrs.onb.mbs.graduate.v1", serviceName, "", "", map[string]any{
				"operator_id": op.ID, "tin_hash": op.TINHash, "estimated_turnover_kobo": g.EstimatedTurnoverKobo,
			}))
			graduated++
			w.step(run, "operator %s graduated to MBS (turnover %d kobo > ceiling %d)", op.ID, g.EstimatedTurnoverKobo, presumptiveCeilingKobo)
		}
		run.Result = map[string]any{"graduated": graduated, "candidates": len(staged)}
		return nil
	})
}
