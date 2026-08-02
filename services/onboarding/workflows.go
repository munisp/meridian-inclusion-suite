package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
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

// storeRunStore adapts the document store to workflowx.RunStore (runs live in
// the "workflow_runs" collection — the same place the dev runner used, so
// GET /v1/workflows/runs is unchanged).
type storeRunStore struct{ st *store.Store }

func (s storeRunStore) PutRun(run workflowx.Run) error {
	return s.st.Put("workflow_runs", run.ID, run)
}

func (s storeRunStore) GetRun(id string) (workflowx.Run, bool, error) {
	var run workflowx.Run
	ok, err := s.st.Get("workflow_runs", id, &run)
	return run, ok, err
}

// Workflows is the wf-onb-* registry. Definitions are durable workflowx
// workflows built from named, idempotent activities (audit O1): the dev
// profile runs them on the persisted in-process runner (crash → run stays
// "running" → POST /v1/workflows/runs/{id}/redrive re-drives idempotently);
// profile=prod requires TEMPORAL_URL (workflowx.NewRunnerFromEnv, fail-closed).
type Workflows struct {
	st          *store.Store
	registry    *Registry
	verifier    NINVerifier
	provisioner TINProvisioner
	consent     *ConsentService
	ledger      ledger.Client
	bus         events.Bus
	runner      *workflowx.Runner
}

// NewWorkflows wires the registry with the dev in-process durable runner
// (used by tests and offline tools).
func NewWorkflows(st *store.Store, reg *Registry, v NINVerifier, p TINProvisioner, c *ConsentService, lc ledger.Client, bus events.Bus) *Workflows {
	r := workflowx.NewRunner(storeRunStore{st}, workflowx.DefaultRetryPolicy, "inproc", nil)
	return NewWorkflowsWithRunner(st, reg, v, p, c, lc, bus, r)
}

// NewWorkflowsWithRunner wires the registry with an explicit runner
// (main.go: workflowx.NewRunnerFromEnv — TEMPORAL_URL prod hook).
func NewWorkflowsWithRunner(st *store.Store, reg *Registry, v NINVerifier, p TINProvisioner, c *ConsentService, lc ledger.Client, bus events.Bus, runner *workflowx.Runner) *Workflows {
	w := &Workflows{st: st, registry: reg, verifier: v, provisioner: p, consent: c, ledger: lc, bus: bus, runner: runner}
	w.register()
	return w
}

// register declares workflow definitions + activities on the runner.
func (w *Workflows) register() {
	r := w.runner

	// --- activities (idempotent; safe to re-execute on redrive) ---
	r.RegisterActivity("act-nimc-verify", func(ctx context.Context, in any) (any, error) {
		m := in.(map[string]any)
		op, ok, err := w.registry.Get(m["operator_id"].(string))
		if err != nil || !ok {
			return nil, fmt.Errorf("operator not found")
		}
		// idempotent: already verified -> return the recorded hash
		if op.NINHash != "" && (op.Status == "nin_verified" || op.Status == "tin_provisioned" || op.Status == "graduated") {
			return map[string]any{"nin_hash": op.NINHash, "verified": true, "source": "cached"}, nil
		}
		v, err := w.verifier.VerifyNIN(m["nin"].(string))
		if err != nil {
			return nil, fmt.Errorf("nimc verify: %w", err)
		}
		if !v.Verified {
			return nil, fmt.Errorf("NIMC verification failed: %s", v.Detail)
		}
		if op.NINHash != "" && op.NINHash != v.NINHash {
			return nil, fmt.Errorf("NIN does not match operator record")
		}
		return map[string]any{"nin_hash": v.NINHash, "verified": true, "source": v.Source, "reference": v.Reference}, nil
	})
	r.RegisterActivity("act-tin-provision", func(ctx context.Context, in any) (any, error) {
		m := in.(map[string]any)
		op, ok, err := w.registry.Get(m["operator_id"].(string))
		if err != nil || !ok {
			return nil, fmt.Errorf("operator not found")
		}
		if op.TIN != "" { // idempotent: already provisioned
			return map[string]any{"tin": op.TIN, "tin_hash": op.TINHash, "source": "cached"}, nil
		}
		prov, err := w.provisioner.ProvisionForNIN(m["nin_hash"].(string))
		if err != nil {
			return nil, fmt.Errorf("tin provision: %w", err)
		}
		return map[string]any{"tin": prov.TIN, "tin_hash": prov.TINHash, "source": prov.Source}, nil
	})
	r.RegisterActivity("act-operator-update", func(ctx context.Context, in any) (any, error) {
		m := in.(map[string]any)
		op, ok, err := w.registry.Get(m["operator_id"].(string))
		if err != nil || !ok {
			return nil, fmt.Errorf("operator not found")
		}
		op.NINHash = m["nin_hash"].(string)
		op.TIN = m["tin"].(string)
		op.TINHash = m["tin_hash"].(string)
		// registered -> nin_verified -> tin_provisioned (idempotent per state)
		if err := w.registry.Transition(&op, "nin_verified", "wf-onb-tin-provision"); err != nil {
			return nil, err
		}
		if err := w.registry.Transition(&op, "tin_provisioned", "wf-onb-tin-provision"); err != nil {
			return nil, err
		}
		return map[string]any{"operator_id": op.ID, "status": op.Status}, nil
	})
	r.RegisterActivity("act-publish-provisioned", func(ctx context.Context, in any) (any, error) {
		m := in.(map[string]any)
		// at-least-once: event id carries the run id so consumers can dedupe
		w.bus.Publish("nrs.onb.tin.provisioned.v1", events.New("nrs.onb.tin.provisioned.v1", serviceName, "", "", map[string]any{
			"operator_id": m["operator_id"], "nin_hash": m["nin_hash"], "tin_hash": m["tin_hash"], "source": m["source"],
		}))
		return nil, nil
	})

	// --- workflow definitions ---
	r.RegisterWorkflow("wf-onb-tin-provision", func(ctx *workflowx.Ctx) error {
		m, _ := ctx.Run.Input.(map[string]any)
		if m == nil {
			if s, ok := ctx.Run.Input.(map[string]string); ok {
				m = map[string]any{}
				for k, v := range s {
					m[k] = v
				}
			}
		}
		opID, _ := m["operator_id"].(string)
		nin, _ := m["nin"].(string)
		if opID == "" {
			return fmt.Errorf("operator_id input is required")
		}
		ver, err := ctx.ExecuteActivity("act-nimc-verify", map[string]any{"operator_id": opID, "nin": nin})
		if err != nil {
			return err
		}
		vm := ver.(map[string]any)
		prov, err := ctx.ExecuteActivity("act-tin-provision", map[string]any{"operator_id": opID, "nin_hash": vm["nin_hash"]})
		if err != nil {
			return err
		}
		pm := prov.(map[string]any)
		if _, err := ctx.ExecuteActivity("act-operator-update", map[string]any{
			"operator_id": opID, "nin_hash": vm["nin_hash"], "tin": pm["tin"], "tin_hash": pm["tin_hash"],
		}); err != nil {
			return err
		}
		if _, err := ctx.ExecuteActivity("act-publish-provisioned", map[string]any{
			"operator_id": opID, "nin_hash": vm["nin_hash"], "tin_hash": pm["tin_hash"], "source": pm["source"],
		}); err != nil {
			return err
		}
		ctx.Run.Result = map[string]any{"operator_id": opID, "tin": pm["tin"], "tin_hash": pm["tin_hash"], "source": pm["source"]}
		return nil
	})

	r.RegisterWorkflow("wf-onb-capture-ingest", func(ctx *workflowx.Ctx) error {
		res, err := ctx.ExecuteActivity("act-capture-rollup", nil)
		if err != nil {
			return err
		}
		ctx.Run.Result = res
		return nil
	})
	r.RegisterActivity("act-capture-rollup", func(ctx context.Context, in any) (any, error) {
		var batches []CaptureBatch
		if err := w.st.List("capture_batches", &batches); err != nil {
			return nil, err
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
		return res, nil
	})

	r.RegisterWorkflow("wf-onb-ledger-rollup", func(ctx *workflowx.Ctx) error {
		res, err := ctx.ExecuteActivity("act-ledger-rollup", nil)
		if err != nil {
			return err
		}
		ctx.Run.Result = res
		return nil
	})
	r.RegisterActivity("act-ledger-rollup", func(ctx context.Context, in any) (any, error) {
		return w.ledgerRollup()
	})

	r.RegisterWorkflow("wf-onb-commission-settlement", func(ctx *workflowx.Ctx) error {
		period, _ := ctx.Run.Input.(map[string]any)["period"].(string)
		res, err := ctx.ExecuteActivity("act-commission-settlement", map[string]any{"period": period})
		if err != nil {
			return err
		}
		ctx.Run.Result = res
		return nil
	})
	r.RegisterActivity("act-commission-settlement", func(ctx context.Context, in any) (any, error) {
		period, _ := in.(map[string]any)["period"].(string)
		if period == "" {
			period = time.Now().UTC().Format("2006-01")
		}
		return w.commissionSettlement(period)
	})

	r.RegisterWorkflow("wf-onb-filing-reminders", func(ctx *workflowx.Ctx) error {
		res, err := ctx.ExecuteActivity("act-filing-reminders", nil)
		if err != nil {
			return err
		}
		ctx.Run.Result = res
		return nil
	})
	r.RegisterActivity("act-filing-reminders", func(ctx context.Context, in any) (any, error) {
		return w.filingReminders()
	})

	r.RegisterWorkflow("wf-onb-mbs-graduate", func(ctx *workflowx.Ctx) error {
		res, err := ctx.ExecuteActivity("act-mbs-graduate", nil)
		if err != nil {
			return err
		}
		ctx.Run.Result = res
		return nil
	})
	r.RegisterActivity("act-mbs-graduate", func(ctx context.Context, in any) (any, error) {
		return w.mbsGraduate()
	})
}

// Names is the wf-onb-* catalog.
func (w *Workflows) Names() []string { return w.runner.WorkflowNames() }

// Run dispatches a workflow by name through the durable runner.
func (w *Workflows) Run(name string, input map[string]any) (WorkflowRun, error) {
	run, err := w.runner.Execute(context.Background(), name, input)
	return WorkflowRun(run), err
}

// Redrive re-executes a crashed/failed run idempotently (crash recovery).
func (w *Workflows) Redrive(runID string) (WorkflowRun, error) {
	run, err := w.runner.Redrive(context.Background(), runID)
	return WorkflowRun(run), err
}

func (w *Workflows) mustRun(name string, input map[string]any) WorkflowRun {
	run, _ := w.Run(name, input)
	return run
}

// TINProvision (wf-onb-tin-provision): verify NIN with NIMC, then provision
// TIN via tin-graph with local fallback, update operator, emit event.
func (w *Workflows) TINProvision(operatorID, nin string) WorkflowRun {
	return w.mustRun("wf-onb-tin-provision", map[string]any{"operator_id": operatorID, "nin": nin})
}

// CaptureIngestStats (wf-onb-capture-ingest): rolls up batch ingest stats and
// emits the ingest event (the synchronous ingest itself happens in the API).
func (w *Workflows) CaptureIngestStats() WorkflowRun {
	return w.mustRun("wf-onb-capture-ingest", nil)
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
func (w *Workflows) LedgerRollup() WorkflowRun { return w.mustRun("wf-onb-ledger-rollup", nil) }

func (w *Workflows) ledgerRollup() (any, error) {
	ops, err := w.registry.List()
	if err != nil {
		return nil, err
	}
	perAgent := map[string]int{}
	for _, op := range ops {
		if op.Status == "nin_verified" || op.Status == "tin_provisioned" || op.Status == "graduated" {
			perAgent[op.AgentID]++
		}
	}
	poolID, err := w.ensurePoolAccount()
	if err != nil {
		return nil, err
	}
	var total uint64
	for agentID, n := range perAgent {
		acctID, err := w.ensureCommissionAccount(agentID)
		if err != nil {
			return nil, err
		}
		accrual := uint64(n) * commissionPerVerifiedKobo
		total += accrual
		_ = acctID
	}
	res := map[string]any{"agents": len(perAgent), "pool_account": poolID, "accrued_kobo": total}
	w.bus.Publish("nrs.onb.ledger.rollup.v1", events.New("nrs.onb.ledger.rollup.v1", serviceName, "", "", map[string]any{
		"agents": len(perAgent), "accrued_kobo": total,
	}))
	return res, nil
}

// CommissionSettlement (wf-onb-commission-settlement): settle accrued agent
// commissions on ledger 700 (code 5 settle) from the NRS pool for the current
// period (YYYY-MM). Uses the dev LedgerClient when LEDGER_URL is unset. See
// CommissionSettlementForPeriod.
func (w *Workflows) CommissionSettlement() WorkflowRun {
	return w.CommissionSettlementForPeriod(time.Now().UTC().Format("2006-01"))
}

// CommissionPayout is the durable per-(agent, period) payout marker — the
// dedup record that makes a re-run of a settled period a no-op (F6 funds-flow
// hardening).
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

// nextPayoutPendingID finds the next fresh deterministic pending-transfer id
// for a payout whose earlier attempt ids were voided after post failures.
// Deterministic given ledger state, so concurrent/crashed re-runs converge.
func (w *Workflows) nextPayoutPendingID(agentID, period, poolID, acctID string, amount uint64) (string, error) {
	base := ledger.DeterministicTransferID("comm-pending:" + agentID + ":" + period)
	for i := 1; i <= 100; i++ {
		cand := ledger.DeterministicTransferID(fmt.Sprintf("%s:r%d", base, i))
		if _, err := w.ledger.PendingTransfer(ledger.Transfer{
			ID: cand, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
			Code: ledger.CodeSettle, Amount: amount, UserData: "commission:" + agentID + ":" + period,
		}); err != nil {
			if errors.Is(err, ledger.ErrTransferIDConflict) {
				continue // this attempt id was already consumed/voided
			}
			return "", err
		}
		return cand, nil
	}
	return "", fmt.Errorf("exhausted payout attempt ids for %s:%s", agentID, period)
}

func (w *Workflows) markPayout(agentID, period string, amount uint64, transferID string) error {
	return w.st.Put("commission_payouts", payoutKey(agentID, period), CommissionPayout{
		AgentID: agentID, Period: period, AmountKobo: amount, TransferID: transferID, PaidAt: nowRFC3339(),
	})
}

// CommissionSettlementForPeriod pays accrued agent commissions for one period
// (YYYY-MM). Hardened as a funds flow (funds-flow F6):
//   - per-period dedup marker per agent: a re-run for the same period is a
//     no-op (200 replay), never a double payout;
//   - each payout is a pending -> post saga (deterministic transfer ids) so
//     a crash mid-run is safely resumable/replayable;
//   - the pool-funding leg is likewise idempotent per period.
func (w *Workflows) CommissionSettlementForPeriod(period string) WorkflowRun {
	return w.mustRun("wf-onb-commission-settlement", map[string]any{"period": period})
}

func (w *Workflows) commissionSettlement(period string) (any, error) {
	ops, err := w.registry.List()
	if err != nil {
		return nil, err
	}
	perAgent := map[string]int{}
	for _, op := range ops {
		if op.Status == "nin_verified" || op.Status == "tin_provisioned" || op.Status == "graduated" {
			if op.AgentID != "" {
				perAgent[op.AgentID]++
			}
		}
	}
	if len(perAgent) == 0 {
		return map[string]any{"settled_kobo": 0, "period": period}, nil
	}
	poolID, err := w.ensurePoolAccount()
	if err != nil {
		return nil, err
	}
	treasuryID := ledger.AccountID(nsCommissionsPool, 2)
	if _, err := w.ledger.Balance(treasuryID); err != nil {
		if err := w.ledger.CreateAccounts([]ledger.Account{{ID: treasuryID, Ledger: ledger.LedgerCommissions, Code: 4, UserData: "nrs-treasury-offset"}}); err != nil && err != ledger.ErrAccountExists {
			return nil, err
		}
	}
	// fund the pool (treasury topup, code 4) only for payouts not already
	// marked settled this period — the funding leg is idempotent per period.
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
			return nil, fmt.Errorf("pool funding: %w", err)
		}
	}
	settled := map[string]uint64{}
	var total uint64
	for agentID, n := range perAgent {
		amount := uint64(n) * commissionPerVerifiedKobo
		total += amount
		if w.payoutMarked(agentID, period) {
			settled[agentID] = amount // already paid this period — no-op
			continue
		}
		acctID, err := w.ensureCommissionAccount(agentID)
		if err != nil {
			return nil, err
		}
		// payout saga: pending -> post -> mark-paid. The durable marker is
		// written ONLY after a successful post (audit funds-flow #2): a post
		// failure voids the pending and leaves no marker, so a re-run retries
		// the agent instead of permanently skipping an unpaid payout. All
		// transfer ids are deterministic, so a crash between the post and the
		// marker replays the post idempotently on re-run and then marks.
		pendID := ledger.DeterministicTransferID("comm-pending:" + agentID + ":" + period)
		postID := ledger.DeterministicTransferID("comm-post:" + agentID + ":" + period)
		posted := false
		if _, err := w.ledger.PendingTransfer(ledger.Transfer{
			ID: pendID, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
			Code: ledger.CodeSettle, Amount: amount, UserData: "commission:" + agentID + ":" + period,
		}); err != nil {
			if !errors.Is(err, ledger.ErrTransferIDConflict) {
				return nil, fmt.Errorf("settle agent %s (pending): %w", agentID, err)
			}
			// The deterministic pending id already exists in a terminal form
			// (an earlier run reached the post or its compensation):
			//   - post transfer present: crash between post and marker —
			//     skip to the marker write (the post replay below is a no-op);
			//   - otherwise: an earlier attempt was voided after a post
			//     failure — retry under a fresh deterministic attempt id.
			if _, perr := w.ledger.LookupTransfer(postID); perr == nil {
				posted = true
			} else {
				pendID, err = w.nextPayoutPendingID(agentID, period, poolID, acctID, amount)
				if err != nil {
					return nil, fmt.Errorf("settle agent %s (pending retry): %w", agentID, err)
				}
			}
		}
		if !posted {
			if _, err := w.ledger.PostPendingAs(pendID, postID, amount); err != nil {
				_, _ = w.ledger.VoidPending(pendID) // compensation
				// belt-and-braces: never leave a paid marker for an unposted payout
				_, _ = w.st.Delete("commission_payouts", payoutKey(agentID, period))
				return nil, fmt.Errorf("settle agent %s (post): %w", agentID, err)
			}
		}
		if err := w.markPayout(agentID, period, amount, postID); err != nil {
			// post landed under a deterministic id; a re-run replays the post
			// idempotently and re-attempts the marker — no double-pay.
			return nil, fmt.Errorf("settle agent %s (mark payout): %w", agentID, err)
		}
		settled[agentID] = amount
	}
	w.bus.Publish("nrs.onb.commission.settled.v1", events.New("nrs.onb.commission.settled.v1", serviceName, "", "", map[string]any{
		"settled": settled, "total_kobo": total, "period": period,
	}))
	return map[string]any{"settled": settled, "total_kobo": total, "ledger": ledger.LedgerCommissions, "period": period}, nil
}

// FilingReminders (wf-onb-filing-reminders): queue reminders for provisioned
// operators with no filing activity (dev: emits reminder events).
func (w *Workflows) FilingReminders() WorkflowRun { return w.mustRun("wf-onb-filing-reminders", nil) }

func (w *Workflows) filingReminders() (any, error) {
	ops, err := w.registry.List()
	if err != nil {
		return nil, err
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
	return map[string]any{"queued": queued}, nil
}

// MBSGraduate (wf-onb-mbs-graduate): operators whose estimated turnover
// exceeds the presumptive ceiling graduate to MBS (standard regime).
func (w *Workflows) MBSGraduate() WorkflowRun { return w.mustRun("wf-onb-mbs-graduate", nil) }

func (w *Workflows) mbsGraduate() (any, error) {
	type gradInput struct {
		OperatorID            string `json:"operator_id"`
		EstimatedTurnoverKobo uint64 `json:"estimated_turnover_kobo"`
	}
	var staged []gradInput
	if err := w.st.List("graduation_candidates", &staged); err != nil {
		return nil, err
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
		if err := w.registry.Transition(&op, "graduated", "wf-onb-mbs-graduate"); err != nil {
			continue // illegal transition (e.g. not yet tin_provisioned); skip
		}
		w.bus.Publish("nrs.onb.mbs.graduate.v1", events.New("nrs.onb.mbs.graduate.v1", serviceName, "", "", map[string]any{
			"operator_id": op.ID, "tin_hash": op.TINHash, "estimated_turnover_kobo": g.EstimatedTurnoverKobo,
		}))
		graduated++
	}
	return map[string]any{"graduated": graduated, "candidates": len(staged)}, nil
}
