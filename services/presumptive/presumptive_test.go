package main

import (
	"path/filepath"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

type testStack struct {
	st     *store.Store
	lc     *ledger.DevClient
	pay    *PaymentService
	float  *FloatService
	engine *BandEngine
	gates  *GateClient
	certs  *CertificateService
}

func newTestStack(t *testing.T) testStack {
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
	floats := NewFloatService(st, lc)
	pay := NewPaymentService(st, lc, hub, engine, gates, certs, events.NewInprocBus())
	return testStack{st: st, lc: lc, pay: pay, float: floats, engine: engine, gates: gates, certs: certs}
}

func (ts testStack) mkIntent(t *testing.T, tin string) Payment {
	t.Helper()
	p, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: tin, State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, // ₦3m -> micro band
		Provider: "remita", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPaymentLifecycleHappyPath(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-lifecycle")
	if p.Status != "pending_authorisation" {
		t.Fatalf("intent: %+v", p)
	}
	if p.PendingTransferID == "" {
		t.Fatal("pending transfer id required")
	}
	p2, auth, err := ts.pay.Authorise(p.ID)
	if err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	if p2.PSSPRef == "" {
		t.Fatal("pssp ref required")
	}
	p3, cert, err := ts.pay.Capture(p.ID)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if p3.Status != "captured" || cert.Serial == "" {
		t.Fatalf("captured: %+v cert %+v", p3, cert)
	}
	// certificate verifies
	got, valid, err := ts.certs.Verify(cert.Serial)
	if err != nil || !valid {
		t.Fatalf("verify: %+v valid=%v err=%v", got, valid, err)
	}
	// collections account received the funds
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPosted != p.AmountKobo {
		t.Fatalf("collections %+v", bal)
	}
}

func TestGateClosedBlocksIntent(t *testing.T) {
	ts := newTestStack(t)
	if _, err := ts.gates.Flip(presumptiveGateID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.mkIntent(t, "tinhash-gated"); err != ErrGateClosed {
		t.Fatalf("expected ErrGateClosed, got %v", err)
	}
}

func TestExemptOperatorRejected(t *testing.T) {
	ts := newTestStack(t)
	_, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-exempt", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 20000000, // ₦0.2m <= ₦25m exemption threshold
		Provider: "remita", Period: "2026",
	})
	if err == nil {
		t.Fatal("expected exemption rejection")
	}
}

func TestGraduateRejected(t *testing.T) {
	ts := newTestStack(t)
	_, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-grad", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 200000000000, // ₦2b >> ceiling
		Provider: "remita", Period: "2026",
	})
	if err == nil {
		t.Fatal("expected graduation rejection")
	}
}

func TestVoidReleasesHold(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-void")
	if _, err := ts.pay.Void(p.ID); err != nil {
		t.Fatal(err)
	}
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPending != 0 {
		t.Fatalf("hold must be released: %+v", bal)
	}
}

func TestBandEngineDeterministic(t *testing.T) {
	ts := newTestStack(t)
	a := ts.engine.Evaluate("Lagos", "retail", 300000000, false, 0)
	b := ts.engine.Evaluate("Lagos", "retail", 300000000, false, 0)
	if a.Band != b.Band || a.AnnualLevyKobo != b.AnnualLevyKobo || a.PackID == "" {
		t.Fatalf("non-deterministic engine: %+v vs %+v", a, b)
	}
}

func TestCertificateSignatureTamper(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-tamper")
	if _, _, err := ts.pay.Authorise(p.ID); err != nil {
		t.Fatal(err)
	}
	_, cert, err := ts.pay.Capture(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	cert.AmountKobo++
	if hmacHex(certHMACKey(), canonicalCertPayload(cert)) == cert.Signature {
		t.Fatal("tampered certificate must not verify")
	}
}

func TestFloatOverdraftEnforced(t *testing.T) {
	ts := newTestStack(t)
	// fund the float treasury first (treasury now enforces
	// DEBITS_MUST_NOT_EXCEED_CREDITS — audit Flow 4c)
	treasury, err := ts.float.treasuryAccountID()
	if err != nil {
		t.Fatal(err)
	}
	src := ledger.AccountID(nsFloatTreasury, 99)
	if err := ts.lc.CreateAccounts([]ledger.Account{{ID: src, Ledger: ledger.LedgerAgentFloat, Code: 4, UserData: "funding"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.lc.Transfer(ledger.Transfer{DebitAccountID: src, CreditAccountID: treasury, Ledger: ledger.LedgerAgentFloat, Code: ledger.CodeTopup, Amount: 5000000, UserData: "funding"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.float.Topup("agent-1", 1000000, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.float.Debit("agent-1", 2000000, "too-much"); err == nil {
		t.Fatal("overdraft debit must fail")
	}
	if _, err := ts.float.Debit("agent-1", 400000, "ok"); err != nil {
		t.Fatal(err)
	}
	bal, _ := ts.float.Balance("agent-1")
	if bal.NetPosted() != 600000 {
		t.Fatalf("float %+v", bal)
	}
}

func TestWebhookCaptureFlow(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-webhook")
	p, auth, err := ts.pay.Authorise(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.pay.HandleWebhook("remita", struct {
		Reference string `json:"reference"`
		Event     string `json:"event"`
		PaymentID string `json:"payment_id"`
	}{Reference: auth.Reference, Event: "charge.successful", PaymentID: p.ID})
	if err != nil || got.Status != "captured" {
		t.Fatalf("webhook capture: %+v %v", got, err)
	}
}
