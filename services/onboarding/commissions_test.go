package main

import (
	"errors"
	"testing"
	"time"

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

// TestCommissionPayoutCrashReplay: a crash between the post and the marker
// write (post landed, marker missing) is now the only mid-saga crash window
// (marker is written AFTER the post). Re-running the period replays the
// deterministic post idempotently and writes the marker: exactly one payout.
// The legacy fully-committed state (pending+post+marker) is also covered —
// re-run must be a no-op.
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

var errLedgerOutage = errors.New("ledger: unavailable (injected outage)")

// flakyPostClient wraps a DevClient and fails PostPendingAs while armed,
// simulating a ledger outage mid-settlement.
type flakyPostClient struct {
	*ledger.DevClient
	armed bool
}

func (c *flakyPostClient) PostPendingAs(pendingID, postID string, amount uint64) (string, error) {
	if c.armed {
		return "", errLedgerOutage
	}
	return c.DevClient.PostPendingAs(pendingID, postID, amount)
}

// TestCommissionPayoutPostFailureRetryable (audit funds-flow #2): a
// PostPendingAs failure must NOT leave a paid marker — the pending is
// voided, no marker exists, and a re-run pays the agent exactly once.
func TestCommissionPayoutPostFailureRetryable(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := &flakyPostClient{DevClient: ledger.NewDevClient()}
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())
	op := Operator{NINHash: NINHash("12345678901"), FullName: "Test Op", AgentID: "agent-5", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if run := wf.TINProvision(op.ID, "12345678901"); run.Status != "completed" {
		t.Fatalf("provision: %s", run.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-5"))

	// ledger outage during the post: run fails, agent NOT marked paid
	lc.armed = true
	run := wf.CommissionSettlementForPeriod("2026-07")
	if run.Status == "completed" {
		t.Fatalf("settlement must fail when the post fails")
	}
	if wf.payoutMarked("agent-5", "2026-07") {
		t.Fatalf("post failure must not leave a paid marker (agent would never be paid)")
	}
	bal, _ := lc.Balance(acctID)
	if bal.CreditsPosted != 0 {
		t.Fatalf("no money may move on a failed post, got %+v", bal)
	}

	// ledger recovers: re-run retries and pays exactly once
	lc.armed = false
	run2 := wf.CommissionSettlementForPeriod("2026-07")
	if run2.Status != "completed" {
		t.Fatalf("retry after post failure: %s", run2.Error)
	}
	bal2, _ := lc.Balance(acctID)
	if bal2.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("retry must pay exactly once, got %+v", bal2)
	}
	if !wf.payoutMarked("agent-5", "2026-07") {
		t.Fatalf("marker must exist after a successful retry")
	}
}

// TestCommissionPayoutCrashBetweenPostAndMark covers the remaining crash
// window: post landed (deterministic id) but the process died before the
// marker write. Re-run replays the post idempotently (no double-pay) and
// writes the marker.
func TestCommissionPayoutCrashBetweenPostAndMark(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())
	op := Operator{NINHash: NINHash("12345678901"), FullName: "Test Op", AgentID: "agent-6", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if run := wf.TINProvision(op.ID, "12345678901"); run.Status != "completed" {
		t.Fatalf("provision: %s", run.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-6"))

	// simulate the crash state: pending + post landed, NO marker
	poolID, _ := wf.ensurePoolAccount()
	if _, err := wf.ensureCommissionAccount("agent-6"); err != nil {
		t.Fatal(err)
	}
	pendID := ledger.DeterministicTransferID("comm-pending:agent-6:2026-07")
	postID := ledger.DeterministicTransferID("comm-post:agent-6:2026-07")
	if _, err := lc.PendingTransfer(ledger.Transfer{
		ID: pendID, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
		Code: ledger.CodeSettle, Amount: commissionPerVerifiedKobo, UserData: "commission:agent-6:2026-07",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.PostPendingAs(pendID, postID, commissionPerVerifiedKobo); err != nil {
		t.Fatal(err)
	}
	if wf.payoutMarked("agent-6", "2026-07") {
		t.Fatalf("test setup: marker must be absent (crash before mark)")
	}

	// --- restart: re-run the period ---
	run := wf.CommissionSettlementForPeriod("2026-07")
	if run.Status != "completed" {
		t.Fatalf("re-run: %s", run.Error)
	}
	bal, _ := lc.Balance(acctID)
	if bal.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("crash between post and mark must not double-pay, got %+v", bal)
	}
	if !wf.payoutMarked("agent-6", "2026-07") {
		t.Fatalf("re-run must write the marker after replaying the post")
	}
}

// TestCommissionPayoutExpiredTreatedAsNew proves the R4 idempotency TTL: a
// payout marker older than commissionPayoutTTL no longer suppresses a
// settlement re-run for that period (expired key treated as new).
func TestCommissionPayoutExpiredTreatedAsNew(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())

	// fresh marker: dedup active
	if err := wf.markPayout("agent-x", "2026-01", 100, "tx-1"); err != nil {
		t.Fatal(err)
	}
	if !wf.payoutMarked("agent-x", "2026-01") {
		t.Fatal("fresh marker must suppress re-run")
	}

	// backdate the marker beyond the TTL: dedup lapsed
	old := time.Now().Add(-2 * commissionPayoutTTL).UTC().Format(time.RFC3339)
	if err := st.Put("commission_payouts", payoutKey("agent-x", "2026-01"), CommissionPayout{
		AgentID: "agent-x", Period: "2026-01", AmountKobo: 100, TransferID: "tx-1", PaidAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if wf.payoutMarked("agent-x", "2026-01") {
		t.Fatal("expired marker must be treated as absent")
	}
}

// TestCommissionPayoutPurgeExpired proves the purge removes only expired,
// terminal markers (TransferID recorded) and retains fresh or
// partially-written records.
func TestCommissionPayoutPurgeExpired(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())

	old := time.Now().Add(-2 * commissionPayoutTTL).UTC().Format(time.RFC3339)
	put := func(p CommissionPayout) {
		if err := st.Put("commission_payouts", payoutKey(p.AgentID, p.Period), p); err != nil {
			t.Fatal(err)
		}
	}
	put(CommissionPayout{AgentID: "a1", Period: "2026-01", AmountKobo: 1, TransferID: "tx", PaidAt: old})              // expired, terminal -> purge
	put(CommissionPayout{AgentID: "a2", Period: "2026-01", AmountKobo: 1, TransferID: "", PaidAt: old})                // expired, non-terminal -> keep
	put(CommissionPayout{AgentID: "a3", Period: "2026-01", AmountKobo: 1, TransferID: "tx", PaidAt: nowRFC3339()})     // fresh -> keep

	n, err := wf.PurgeExpiredCommissionPayouts()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged, got %d", n)
	}
	var remaining []CommissionPayout
	if err := st.List("commission_payouts", &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 retained, got %d", len(remaining))
	}
	for _, p := range remaining {
		if p.AgentID == "a1" {
			t.Fatal("expired terminal marker must be purged")
		}
	}
}
