package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

type psmTestStack struct {
	st   *store.Store
	lc   *ledger.DevClient
	pay  *PaymentService
	hub  *PSSPHub
	cert *CertificateService
}

func newPSMTestStack(t *testing.T) psmTestStack {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	lc := ledger.NewDevClient()
	engine, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	gates := &GateClient{file: filepath.Join(t.TempDir(), "gates.json")}
	if _, err := gates.Flip(presumptiveGateID, true); err != nil {
		t.Fatal(err)
	}
	hub := NewPSSPHub()
	certs := NewCertificateService(st)
	pay := NewPaymentService(st, lc, hub, engine, gates, certs, events.NewInprocBus())
	return psmTestStack{st: st, lc: lc, pay: pay, hub: hub, cert: certs}
}

func (ts psmTestStack) authorisedPayment(t *testing.T, key string) Payment {
	t.Helper()
	p, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-" + key, State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, auth, err := ts.pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	got, _, _ := ts.pay.get(p.ID)
	return got
}

// TestCreateIntentIdempotencyKey: a client retry with the same key replays
// the original payment; no second pending hold is created (audit Flow 1b).
func TestCreateIntentIdempotencyKey(t *testing.T) {
	ts := newPSMTestStack(t)
	p1, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-idem", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-idem", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID || p2.PendingTransferID != p1.PendingTransferID {
		t.Fatalf("replay must return the same payment, got %+v vs %+v", p1, p2)
	}
	var all []Payment
	if err := ts.st.List("payments", &all); err != nil || len(all) != 1 {
		t.Fatalf("exactly one payment must exist, got %d err=%v", len(all), err)
	}
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPending != p1.AmountKobo {
		t.Fatalf("exactly one pending hold expected, got %d", bal.CreditsPending)
	}
}

// TestCapturePSSPIdempotentReplay: crash between PSSP capture and the state
// persist leaves the payment "authorised" while the PSSP has the money. A
// retried Capture must replay at the provider (same idempotency key), not
// double-charge (audit Flow 1a crash window 2).
func TestCapturePSSPIdempotentReplay(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "replay")
	adapter, _ := ts.hub.Adapter(p.Provider)
	// first capture reaches the PSSP; simulate the crash by NOT going
	// through pay.Capture (store still says "authorised")
	res1, err := adapter.Capture(p.PSSPRef, p.AmountKobo, "capture:"+p.ID)
	if err != nil || res1.Status != "captured" {
		t.Fatalf("first capture: %+v %v", res1, err)
	}
	// process restarts, payment still "authorised" -> full Capture retry
	p2, cert, err := ts.pay.Capture(p.ID)
	if err != nil {
		t.Fatalf("retried capture must succeed via idempotent replay: %v", err)
	}
	if p2.Status != "captured" || cert.Serial == "" {
		t.Fatalf("expected captured with certificate, got %+v", p2)
	}
	// exactly one post to collections
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPosted != p.AmountKobo {
		t.Fatalf("collections must be posted exactly once, got %+v", bal)
	}
}

// TestRecoverySweepResumesCrashAfterPost: crash between the ledger post and
// the final persist (status captured_awaiting_post, post already on the
// ledger). The sweeper must resume: issue the certificate and mark captured
// WITHOUT posting again (audit Flow 1a crash window 1).
func TestRecoverySweepResumesCrashAfterPost(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "crash1")
	adapter, _ := ts.hub.Adapter(p.Provider)
	if _, err := adapter.Capture(p.PSSPRef, p.AmountKobo, "capture:"+p.ID); err != nil {
		t.Fatal(err)
	}
	// drive the saga to just after the ledger post, then "crash"
	p.Status = "captured_awaiting_post"
	p.PostTransferID = ledger.DeterministicTransferID("psm-post:" + p.ID)
	p.FeeKobo = 0
	if err := ts.st.Put("payments", p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.lc.PostPendingAs(p.PendingTransferID, p.PostTransferID, p.AmountKobo); err != nil {
		t.Fatal(err)
	}
	before, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	// --- process restart: recovery sweeper runs ---
	sw := NewRecoverySweeper(ts.pay, ts.lc, events.NewInprocBus())
	resumed, compensated, _, err := sw.SweepOnce()
	if err != nil {
		t.Fatal(err)
	}
	if resumed != 1 || compensated != 0 {
		t.Fatalf("expected resumed=1 compensated=0, got %d/%d", resumed, compensated)
	}
	var got Payment
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.Status != "captured" || got.CertificateSerial == "" {
		t.Fatalf("payment must be captured with certificate, got %+v", got)
	}
	after, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if after.CreditsPosted != before.CreditsPosted || after.CreditsPosted != p.AmountKobo {
		t.Fatalf("sweeper must not double-post: before=%+v after=%+v", before, after)
	}
	// sweeping again is a no-op
	resumed, _, _, _ = sw.SweepOnce()
	if resumed != 0 {
		t.Fatal("second sweep must be a no-op")
	}
}

// TestRecoverySweepCompensatesLegacyRecord: a record stuck in
// captured_awaiting_post WITHOUT a persisted post id (pre-fix shape) is
// compensated: PSSP refunded, and NO ledger reversal invents money (the post
// never landed).
func TestRecoverySweepCompensatesLegacyRecord(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "crash2")
	adapter, _ := ts.hub.Adapter(p.Provider)
	if _, err := adapter.Capture(p.PSSPRef, p.AmountKobo, "capture:"+p.ID); err != nil {
		t.Fatal(err)
	}
	p.Status = "captured_awaiting_post" // no PostTransferID persisted
	if err := ts.st.Put("payments", p.ID, p); err != nil {
		t.Fatal(err)
	}
	sw := NewRecoverySweeper(ts.pay, ts.lc, events.NewInprocBus())
	resumed, compensated, _, err := sw.SweepOnce()
	if err != nil {
		t.Fatal(err)
	}
	if resumed != 0 || compensated != 1 {
		t.Fatalf("expected compensated legacy record, got resumed=%d compensated=%d", resumed, compensated)
	}
	var got Payment
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.Status != "compensated" {
		t.Fatalf("got %+v", got)
	}
	var comps []Compensation
	if err := ts.st.List("compensations", &comps); err != nil || len(comps) != 1 {
		t.Fatalf("expected 1 compensation, got %v", comps)
	}
	if comps[0].LedgerReversal != "" {
		t.Fatal("no ledger reversal allowed when the post never landed")
	}
	if comps[0].PSSPRefund != "ok" {
		t.Fatalf("payer must be refunded, got %+v", comps[0])
	}
}

// TestRecoveryExpiresAbandonedIntents: stale pending_authorisation intents
// are voided so the hold is released (audit Flow 1d).
func TestRecoveryExpiresAbandonedIntents(t *testing.T) {
	ts := newPSMTestStack(t)
	p, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-stale", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	// age the record past the TTL
	p.UpdatedAt = time.Now().UTC().Add(-2 * intentTTL).Format(time.RFC3339)
	if err := ts.st.Put("payments", p.ID, p); err != nil {
		t.Fatal(err)
	}
	sw := NewRecoverySweeper(ts.pay, ts.lc, events.NewInprocBus())
	if _, _, expired, err := sw.SweepOnce(); err != nil || expired != 1 {
		t.Fatalf("expected 1 expired intent, got %d err=%v", expired, err)
	}
	var got Payment
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.Status != "expired" {
		t.Fatalf("got %+v", got)
	}
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPending != 0 {
		t.Fatalf("pending hold must be released, got %+v", bal)
	}
}

// TestCaptureFeeLeg: the PSSP fee is posted to the dedicated fee-income
// account so collections nets to the settled amount (audit fix #4 / F6).
func TestCaptureFeeLeg(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "fee")
	if _, _, err := ts.pay.Capture(p.ID); err != nil {
		t.Fatal(err)
	}
	collections, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	fee, _ := ts.lc.Balance(ledger.AccountID(nsPSPFeeIncome, 1))
	if fee.CreditsPosted == 0 {
		t.Fatal("fee income account must receive the PSSP fee")
	}
	// collections net == settled (gross - fee) == what treasury will receive
	if net := collections.CreditsPosted - collections.DebitsPosted; net != p.AmountKobo-fee.CreditsPosted {
		t.Fatalf("collections net %d != settled %d", net, p.AmountKobo-fee.CreditsPosted)
	}
	var got Payment
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.FeeKobo != fee.CreditsPosted {
		t.Fatalf("payment fee leg mismatch: %+v vs %+v", got, fee)
	}
}

// TestCertificateIssueIdempotent: re-issuing for the same payment returns
// the same serial (recovery-safe).
func TestCertificateIssueIdempotent(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "cert")
	c1, err := ts.cert.Issue(p)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ts.cert.Issue(p)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Serial != c2.Serial || c1.Signature != c2.Signature {
		t.Fatalf("certificate issuance must be idempotent, got %+v vs %+v", c1, c2)
	}
}

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

var _ = fmt.Sprint // keep fmt imported if unused in future edits
