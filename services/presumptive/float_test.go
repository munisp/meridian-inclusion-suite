package main

import (
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// --- F4: float saga + reference dedup ---

func newFloatStack(t *testing.T) (*store.Store, *ledger.DevClient, *FloatService) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	lc := ledger.NewDevClient()
	return st, lc, NewFloatService(st, lc)
}

func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fundTreasury credits the float treasury so top-ups pass the funding flag.
func fundTreasury(t *testing.T, lc *ledger.DevClient, amount uint64) {
	t.Helper()
	// ensure the treasury account exists (it is created lazily on first use)
	fs := NewFloatService(mustStore(t), lc)
	if _, err := fs.treasuryAccountID(); err != nil {
		t.Fatal(err)
	}
	src := ledger.AccountID(nsFloatTreasury, 99) // external funding source
	if err := lc.CreateAccounts([]ledger.Account{{ID: src, Ledger: ledger.LedgerAgentFloat, Code: 4, UserData: "funding-source"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Transfer(ledger.Transfer{
		DebitAccountID: src, CreditAccountID: ledger.AccountID(nsFloatTreasury, 1),
		Ledger: ledger.LedgerAgentFloat, Code: ledger.CodeTopup, Amount: amount, UserData: "treasury-funding",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFloatTopupReferenceDedup: retried top-up with the same reference is a
// 200 replay — exactly one credit lands (audit Flow 4b).
func TestFloatTopupReferenceDedup(t *testing.T) {
	_, lc, fs := newFloatStack(t)
	fundTreasury(t, lc, 1000000)
	if _, err := fs.Topup("agent-1", 100000, "ref-1"); err != nil {
		t.Fatal(err)
	}
	mv1, err := fs.Topup("agent-1", 100000, "ref-2")
	if err != nil {
		t.Fatal(err)
	}
	mv2, err := fs.Topup("agent-1", 100000, "ref-2") // retry
	if err != nil {
		t.Fatal(err)
	}
	if mv1.ID != mv2.ID || mv2.Status != "posted" {
		t.Fatalf("replay must return the same posted movement, got %+v vs %+v", mv1, mv2)
	}
	bal, _ := fs.Balance("agent-1")
	if bal.CreditsPosted != 200000 {
		t.Fatalf("exactly two distinct top-ups must post, got %+v", bal)
	}
	mvs, _ := fs.Movements("agent-1")
	if len(mvs) != 2 {
		t.Fatalf("expected 2 movements, got %d", len(mvs))
	}
}

// TestFloatTopupTreasuryFundingControl: treasury has
// DEBITS_MUST_NOT_EXCEED_CREDITS — an unfunded top-up fails (Flow 4c).
func TestFloatTopupTreasuryFundingControl(t *testing.T) {
	_, _, fs := newFloatStack(t)
	if _, err := fs.Topup("agent-2", 100000, "ref-unfunded"); err == nil {
		t.Fatal("unfunded top-up must fail: treasury debits must not exceed credits")
	}
}

// TestFloatDebitOverdraftStillBlocked: float drawdown beyond balance fails.
func TestFloatDebitOverdraftStillBlocked(t *testing.T) {
	_, lc, fs := newFloatStack(t)
	fundTreasury(t, lc, 50000)
	if _, err := fs.Topup("agent-3", 50000, "ref-t"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Debit("agent-3", 60000, "ref-d"); err == nil {
		t.Fatal("overdraft debit must fail")
	}
}

// TestFloatSagaCrashRecovery: crash after the pending hold + record but
// before the post -> SweepFloatMovements finishes the post exactly once.
func TestFloatSagaCrashRecovery(t *testing.T) {
	st, lc, fs := newFloatStack(t)
	fundTreasury(t, lc, 200000)
	fa, _ := fs.Open("agent-4")
	treasury, _ := fs.treasuryAccountID()
	// simulate the crashed saga: pending transfer + pending record, no post
	pendID := ledger.DeterministicTransferID("float-pending:topup:ref-crash")
	postID := ledger.DeterministicTransferID("float-post:topup:ref-crash")
	if _, err := lc.PendingTransfer(ledger.Transfer{
		ID: pendID, DebitAccountID: treasury, CreditAccountID: fa.AccountID,
		Ledger: ledger.LedgerAgentFloat, Code: ledger.CodeTopup, Amount: 75000, UserData: "topup:ref-crash",
	}); err != nil {
		t.Fatal(err)
	}
	mv := FloatMovement{ID: movementID("topup", "ref-crash"), AgentID: "agent-4", Kind: "topup",
		AmountKobo: 75000, Reference: "ref-crash", PendingTransferID: pendID, TransferID: postID,
		Status: "pending", CreatedAt: nowRFC3339()}
	if err := st.Put("float_movements", mv.ID, mv); err != nil {
		t.Fatal(err)
	}
	finished, voided, err := fs.SweepFloatMovements()
	if err != nil || finished != 1 || voided != 0 {
		t.Fatalf("sweep: finished=%d voided=%d err=%v", finished, voided, err)
	}
	bal, _ := fs.Balance("agent-4")
	if bal.CreditsPosted != 75000 {
		t.Fatalf("crashed top-up must be completed exactly once, got %+v", bal)
	}
	// a client retry after recovery replays the posted movement
	got, err := fs.Topup("agent-4", 75000, "ref-crash")
	if err != nil || got.Status != "posted" {
		t.Fatalf("retry must replay posted movement, got %+v err=%v", got, err)
	}
	if bal2, _ := fs.Balance("agent-4"); bal2.CreditsPosted != 75000 {
		t.Fatal("retry must not double-credit")
	}
}

// TestLedgerTransferIDDedup: the dev ledger dedupes client-supplied transfer
// ids and rejects conflicts (TigerBeetle semantics, audit fix #1).
func TestLedgerTransferIDDedup(t *testing.T) {
	lc := ledger.NewDevClient()
	a := ledger.AccountID(1, 1)
	b := ledger.AccountID(1, 2)
	if err := lc.CreateAccounts([]ledger.Account{{ID: a, Ledger: 1, Code: 1}, {ID: b, Ledger: 1, Code: 1}}); err != nil {
		t.Fatal(err)
	}
	id := ledger.DeterministicTransferID("seed-1")
	tr := ledger.Transfer{ID: id, DebitAccountID: a, CreditAccountID: b, Ledger: 1, Code: 1, Amount: 100}
	if _, err := lc.Transfer(tr); err != nil {
		t.Fatal(err)
	}
	got, err := lc.Transfer(tr) // exact replay
	if err != nil || got != id {
		t.Fatalf("replay must return same id, got %s err=%v", got, err)
	}
	tr.Amount = 200
	if _, err := lc.Transfer(tr); err != ledger.ErrTransferIDConflict {
		t.Fatalf("conflicting id must fail, got %v", err)
	}
	bal, _ := lc.Balance(b)
	if bal.CreditsPosted != 100 {
		t.Fatalf("single post expected, got %+v", bal)
	}
	// PostPendingAs idempotent replay
	pendID := ledger.DeterministicTransferID("pend-1")
	if _, err := lc.PendingTransfer(ledger.Transfer{ID: pendID, DebitAccountID: a, CreditAccountID: b, Ledger: 1, Code: 1, Amount: 50}); err != nil {
		t.Fatal(err)
	}
	postID := ledger.DeterministicTransferID("post-1")
	if _, err := lc.PostPendingAs(pendID, postID, 50); err != nil {
		t.Fatal(err)
	}
	if got, err := lc.PostPendingAs(pendID, postID, 50); err != nil || got != postID {
		t.Fatalf("post replay must succeed, got %s err=%v", got, err)
	}
	if bal, _ := lc.Balance(b); bal.CreditsPosted != 150 {
		t.Fatalf("post must land once, got %+v", bal)
	}
}

