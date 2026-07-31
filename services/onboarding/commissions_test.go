package main

import (
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// TestCommissionPayoutDedupPerPeriod proves the F4 payout hardening: a
// payout run for a period pays each agent exactly once on ledger 700 via a
// pending->post saga; re-running the same period is a no-op (never a double
// payout), and a crash after the post replays safely.
func TestCommissionPayoutDedupPerPeriod(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())

	op := Operator{NINHash: NINHash("12345678901"), FullName: "Test Op", AgentID: "agent-9", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if run := wf.TINProvision(op.ID, "12345678901"); run.Status != "completed" {
		t.Fatalf("provision: %s", run.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-9"))

	run1 := wf.CommissionSettlementForPeriod("2026-07")
	if run1.Status != "completed" {
		t.Fatalf("settlement run 1: %s", run1.Error)
	}
	bal1, err := lc.Balance(acctID)
	if err != nil || bal1.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("agent must be paid once: %+v err=%v", bal1, err)
	}

	// re-run the same period: dedup marker -> no second payout
	run2 := wf.CommissionSettlementForPeriod("2026-07")
	if run2.Status != "completed" {
		t.Fatalf("settlement run 2: %s", run2.Error)
	}
	bal2, _ := lc.Balance(acctID)
	if bal2.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("re-run must not double-pay: before=%d after=%d", bal1.CreditsPosted, bal2.CreditsPosted)
	}

	// a NEW period pays again (markers are per-period)
	run3 := wf.CommissionSettlementForPeriod("2026-08")
	if run3.Status != "completed" {
		t.Fatalf("settlement run 3: %s", run3.Error)
	}
	bal3, _ := lc.Balance(acctID)
	if bal3.CreditsPosted != 2*commissionPerVerifiedKobo {
		t.Fatalf("new period must pay again, got %+v", bal3)
	}

	// payout markers exist and carry the deterministic post transfer ids
	var markers []CommissionPayout
	if err := st.List("commission_payouts", &markers); err != nil || len(markers) != 2 {
		t.Fatalf("expected 2 payout markers, got %v err=%v", markers, err)
	}
	for _, m := range markers {
		if m.TransferID != ledger.DeterministicTransferID("comm-post:"+m.AgentID+":"+m.Period) {
			t.Fatalf("marker transfer id mismatch: %+v", m)
		}
	}
}

// TestCommissionPayoutCrashReplay: crash after the post lands but before the
// marker write commits is impossible (marker precedes post), but a crash
// between the pending and the post leaves a voided-or-replayable pending;
// re-running the period completes the payout exactly once.
func TestCommissionPayoutCrashReplay(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())
	op := Operator{NINHash: NINHash("12345678901"), FullName: "Test Op", AgentID: "agent-7", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if run := wf.TINProvision(op.ID, "12345678901"); run.Status != "completed" {
		t.Fatalf("provision: %s", run.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-7"))

	// simulate a crash after the marker+pending exist and the post landed
	// (the worst case): post landed, marker exists. Re-run must be a no-op.
	poolID, _ := wf.ensurePoolAccount()
	if _, err := wf.ensureCommissionAccount("agent-7"); err != nil {
		t.Fatal(err)
	}
	pendID := ledger.DeterministicTransferID("comm-pending:agent-7:2026-07")
	postID := ledger.DeterministicTransferID("comm-post:agent-7:2026-07")
	if _, err := lc.PendingTransfer(ledger.Transfer{
		ID: pendID, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
		Code: ledger.CodeSettle, Amount: commissionPerVerifiedKobo, UserData: "commission:agent-7:2026-07",
	}); err != nil {
		t.Fatal(err)
	}
	if err := wf.markPayout("agent-7", "2026-07", commissionPerVerifiedKobo, postID); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.PostPendingAs(pendID, postID, commissionPerVerifiedKobo); err != nil {
		t.Fatal(err)
	}
	// --- restart: re-run the period ---
	run := wf.CommissionSettlementForPeriod("2026-07")
	if run.Status != "completed" {
		t.Fatalf("re-run: %s", run.Error)
	}
	bal, _ := lc.Balance(acctID)
	if bal.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("crash replay must not double-pay, got %+v", bal)
	}
}
