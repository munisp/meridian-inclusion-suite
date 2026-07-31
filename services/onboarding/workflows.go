package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// Workflows runs the multi-step onboarding flows (SPEC §3 wf-onb-*).
type Workflows struct {
	st     *store.Store
	reg    *Registry
	nimc   NIMCClient
	tins   TINProvisioner
	cons   *ConsentService
	ledger ledger.Client
	bus    events.Bus
}

func NewWorkflows(st *store.Store, reg *Registry, nimc NIMCClient, tins TINProvisioner, cons *ConsentService, lc ledger.Client, bus events.Bus) *Workflows {
	return &Workflows{st: st, reg: reg, nimc: nimc, tins: tins, cons: cons, ledger: lc, bus: bus}
}

func (w *Workflows) record(flow string, input map[string]any, run func(*WorkflowRun) error) WorkflowRun {
	r := WorkflowRun{ID: NewULID(), Flow: flow, Input: input, Status: "running", StartedAt: nowRFC3339()}
	defer func() { _ = w.st.Put("workflow_runs", r.ID, &r) }()
	if err := run(&r); err != nil {
		r.Status = "failed"
		r.Error = err.Error()
		return r
	}
	r.Status = "completed"
	r.FinishedAt = nowRFC3339()
	return r
}

func (w *Workflows) step(r *WorkflowRun, format string, args ...any) {
	r.Steps = append(r.Steps, fmt.Sprintf(format, args...))
}

// NINVerify (wf-onb-nin-verify): validate format -> verify against NIMC
// (simulator locally; {NIMC_API_URL}+mTLS in prod) -> bind to operator.
func (w *Workflows) NINVerify(operatorID, nin string) WorkflowRun {
	return w.record("wf-onb-nin-verify", map[string]any{"operator_id": operatorID}, func(run *WorkflowRun) error {
		op, err := w.reg.Get(operatorID)
		if err != nil {
			return err
		}
		if err := ValidateNIN(nin); err != nil {
			return err
		}
		w.step(run, "nin format valid")
		res, err := w.nimc.Verify(nin)
		if err != nil {
			return fmt.Errorf("nimc verify: %w", err)
		}
		if !res.Match {
			return fmt.Errorf("nimc mismatch: %s", res.Detail)
		}
		w.step(run, "nimc verified (%s)", res.Name)
		op.NINHash = NINHash(nin)
		now := nowRFC3339()
		op.VerifiedAt = &now
		op.Lifecycle = LifecycleRegistered
		if err := w.reg.Create(op); err != nil {
			return err
		}
		w.bus.Publish("nrs.onboarding.events.v1", events.New("nrs.onboarding.events.v1", serviceName, "", "", map[string]any{
			"operator_id": op.ID, "agent_id": op.AgentID, "event": "nin_verified",
		}))
		return nil
	})
}

// TINProvision (wf-onb-tin-provision): NIN -> TIN issuance (sandbox locally,
// JTB TIN API in prod) -> operator updated to tin_registered.
func (w *Workflows) TINProvision(operatorID, nin string) WorkflowRun {
	return w.record("wf-onb-tin-provision", map[string]any{"operator_id": operatorID}, func(run *WorkflowRun) error {
		op, err := w.reg.Get(operatorID)
		if err != nil {
			return err
		}
		if op.NINHash == "" {
			// provision implies verify first
			vr := w.NINVerify(operatorID, nin)
			if vr.Status != "completed" {
				return fmt.Errorf("verify step: %s", vr.Error)
			}
			w.step(run, "verify inlined: %s", vr.ID)
			op, _ = w.reg.Get(operatorID)
		}
		tin, err := w.tins.Provision(op.NINHash, op.FullName)
		if err != nil {
			return fmt.Errorf("tin provision: %w", err)
		}
		op.TINHash = TINHash(tin)
		op.Lifecycle = LifecycleTINRegistered
		if err := w.reg.Create(op); err != nil {
			return err
		}
		w.step(run, "tin issued (hash %s)", op.TINHash)
		w.bus.Publish("nrs.onboarding.events.v1", events.New("nrs.onboarding.events.v1", serviceName, "", "", map[string]any{
			"operator_id": op.ID, "agent_id": op.AgentID, "event": "tin_issued",
		}))
		return nil
	})
}

// USSDEnrol (wf-onb-ussd-enrol): create the operator's USSD PIN.
func (w *Workflows) USSDEnrol(operatorID, pin string) WorkflowRun {
	return w.record("wf-onb-ussd-enrol", map[string]any{"operator_id": operatorID}, func(run *WorkflowRun) error {
		op, err := w.reg.Get(operatorID)
		if err != nil {
			return err
		}
		if len(pin) < 4 || len(pin) > 6 {
			return fmt.Errorf("pin must be 4-6 digits")
		}
		sess := &USSDSession{ID: NewULID(), OperatorID: op.ID, MSISDN: op.Phone, PIN: pin, State: "enrolled", CreatedAt: nowRFC3339()}
		if err := w.st.Put("ussd_sessions", sess.ID, sess); err != nil {
			return err
		}
		w.step(run, "ussd enrolled session %s", sess.ID)
		return nil
	})
}

// AgentHierarchy (wf-onb-agent-hierarchy): attach operator to agent ->
// supervisor -> aggregator chain; commission accrues per verified operator.
func (w *Workflows) AgentHierarchy(operatorID, agentID, supervisorID, aggregatorID string) WorkflowRun {
	return w.record("wf-onb-agent-hierarchy", map[string]any{"operator_id": operatorID}, func(run *WorkflowRun) error {
		op, err := w.reg.Get(operatorID)
		if err != nil {
			return err
		}
		op.AgentID = agentID
		op.SupervisorID = supervisorID
		op.AggregatorID = aggregatorID
		op.Lifecycle = LifecycleActive
		if err := w.reg.Create(op); err != nil {
			return err
		}
		w.step(run, "hierarchy agent=%s supervisor=%s aggregator=%s", agentID, supervisorID, aggregatorID)
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
		var ops []Operator
		if err := w.st.List("operators", &ops); err != nil {
			return err
		}
		perAgent := map[string]int{}
		for _, op := range ops {
			if op.Lifecycle == LifecycleTINRegistered || op.Lifecycle == LifecycleActive {
				if op.AgentID != "" {
					perAgent[op.AgentID]++
				}
			}
		}
		if len(perAgent) == 0 {
			w.step(run, "no verified operators; nothing to settle")
			return nil
		}
		poolID, err := w.ensurePoolAccount()
		if err != nil {
			return err
		}
		var total uint64
		for _, n := range perAgent {
			total += uint64(n) * commissionPerVerifiedKobo
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
		run.Result = settled
		return nil
	})
}

func (w *Workflows) ensurePoolAccount() (string, error) {
	id := ledger.AccountID(nsCommissionsPool, 1)
	if _, err := w.ledger.Balance(id); err == nil {
		return id, nil
	}
	if err := w.ledger.CreateAccounts([]ledger.Account{{ID: id, Ledger: ledger.LedgerCommissions, Code: 4, UserData: "nrs-commissions-pool"}}); err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

func (w *Workflows) ensureCommissionAccount(agentID string) (string, error) {
	id := ledger.AccountID(nsAgentCommission, hashSerial(agentID))
	if _, err := w.ledger.Balance(id); err == nil {
		return id, nil
	}
	if err := w.ledger.CreateAccounts([]ledger.Account{{ID: id, Ledger: ledger.LedgerCommissions, Code: 5, UserData: "agent-commission:" + fmt.Sprint(hashSerial(agentID))}}); err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// OfflineQueue processes queued USSD offline registrations (simulated sync).
func (w *Workflows) OfflineQueue() WorkflowRun {
	return w.record("wf-onb-offline-queue", nil, func(run *WorkflowRun) error {
		var q []OfflineQueueItem
		if err := w.st.List("offline_queue", &q); err != nil {
			return err
		}
		processed := 0
		for _, item := range q {
			if item.Synced {
				continue
			}
			op := &Operator{
				ID: NewULID(), AgentID: item.AgentID, FullName: item.FullName, Phone: item.Phone,
				NINHash: NINHash(item.NIN), State: item.State, LGA: item.LGA,
				MarketCode: item.MarketCode, Lifecycle: LifecycleRegistered, CapturedAt: item.CreatedAt,
			}
			if err := w.reg.Create(op); err != nil {
				return fmt.Errorf("sync %s: %w", item.ID, err)
			}
			item.Synced = true
			if err := w.st.Put("offline_queue", item.ID, &item); err != nil {
				return err
			}
			processed++
		}
		w.step(run, "processed %d queued registrations", processed)
		run.Result = map[string]int{"processed": processed}
		return nil
	})
}

// ListRuns returns all workflow runs (newest first by StartedAt).
func (w *Workflows) ListRuns() ([]WorkflowRun, error) {
	var runs []WorkflowRun
	if err := w.st.List("workflow_runs", &runs); err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt > runs[j].StartedAt })
	return runs, nil
}
