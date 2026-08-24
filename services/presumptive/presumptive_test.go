package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	wf     *PSMWorkflows
}

func newTestStack(t *testing.T) *testStack {
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
	bus := events.NewInprocBus()
	pay := NewPaymentService(st, lc, hub, engine, gates, certs, bus)
	wf := NewPSMWorkflows(st, pay, floats, engine, gates, lc, bus)
	return &testStack{st: st, lc: lc, pay: pay, float: floats, engine: engine, gates: gates, certs: certs, wf: wf}
}

func (ts *testStack) openGate(t *testing.T) {
	t.Helper()
	if _, err := ts.gates.Flip(presumptiveGateID, true); err != nil {
		t.Fatal(err)
	}
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
	if _, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-gated", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
	}); err != ErrGateClosed {
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

func TestSimulationWorkflow(t *testing.T) {
	ts := newTestStack(t)
	run := ts.wf.Simulate(map[string]any{"cohort": []map[string]any{
		{"operator_ref": "op-1", "state": "Lagos", "trade_category": "retail", "annual_turnover_kobo": 300000000},
		{"operator_ref": "op-2", "state": "Kano", "trade_category": "food_vendor", "annual_turnover_kobo": 50000000},
		{"operator_ref": "op-3", "state": "Kano", "trade_category": "transport", "annual_turnover_kobo": 700000000},
	}})
	if run.Status != "completed" {
		t.Fatalf("simulation failed: %s", run.Error)
	}
	sim := run.Result.(Simulation)
	if sim.Scenarios != 3 || sim.Results[1].AnnualLevyKobo != 0 && !sim.Results[1].Exempt {
		t.Fatalf("unexpected sim results: %+v", sim.Results)
	}
	if sim.Results[1].Exempt != true { // below N800k
		t.Fatal("op-2 should be exempt")
	}
	// Canonical packs (B3 #3): op-1 Lagos N3m -> lower_medium N20,000.00
	// (2,000,000 kobo); op-3 Kano N7m -> medium N20,000.00 (2,000,000 kobo).
	if sim.Totals["grand_total"] != 2000000+2000000 {
		t.Fatalf("grand total %v", sim.Totals)
	}
	// persisted
	var stored Simulation
	ok, _ := ts.st.Get("simulations", sim.ID, &stored)
	if !ok {
		t.Fatal("simulation not persisted")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(3, 0.0001)
	for i := 0; i < 3; i++ {
		if !rl.Allow("ip1") {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if rl.Allow("ip1") {
		t.Fatal("4th call should be rate-limited")
	}
	if !rl.Allow("ip2") {
		t.Fatal("different key has its own bucket")
	}
}

func TestMain(m *testing.M) {
	// ensure dev keys are deterministic in tests
	os.Setenv("TIN_HMAC_KEY", "test-tin")
	os.Exit(m.Run())
}

var _ = fmt.Sprint // keep fmt import if unused in future edits

// TestDeviceEnrolmentAndReceiptVerify (audit fix #6): a receipt signed by an
// enrolled device verifies; unenrolled devices and tampered payloads fail
// closed.
func TestDeviceEnrolmentAndReceiptVerify(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ds := NewDeviceService(st)
	if _, err := ds.Enroll("agent-1", "dev-1", "device-secret-key-123"); err != nil {
		t.Fatal(err)
	}
	req := ReceiptVerifyRequest{
		Serial: "RCPT-OFF-1", AgentID: "agent-1", DeviceID: "dev-1",
		PayerName: "Musa Bello", AmountKobo: 500000, Purpose: "presumptive levy",
		IssuedAt: "2026-01-01T10:00:00Z",
	}
	req.Signature = signReceipt("device-secret-key-123", CanonicalReceiptPayload(req))
	valid, detail, err := ds.VerifyReceipt(req)
	if err != nil || !valid {
		t.Fatalf("enrolled receipt must verify: valid=%v detail=%s err=%v", valid, detail, err)
	}
	// tampered amount
	bad := req
	bad.AmountKobo = 900000
	if valid, _, _ := ds.VerifyReceipt(bad); valid {
		t.Fatal("tampered receipt must not verify")
	}
	// unenrolled device fails closed
	bad = req
	bad.DeviceID = "dev-unknown"
	if valid, d, _ := ds.VerifyReceipt(bad); valid || d != "device not enrolled" {
		t.Fatalf("unenrolled device must fail closed, got valid=%v detail=%s", valid, d)
	}
	// wrong key never verifies
	bad = req
	bad.Signature = signReceipt("attacker-key-0000000", CanonicalReceiptPayload(req))
	if valid, _, _ := ds.VerifyReceipt(bad); valid {
		t.Fatal("forged signature must not verify")
	}
}

// TestFloatRiskScoring (I17): risk score responds to utilization, dormancy
// and low balance.
func TestFloatRiskScoring(t *testing.T) {
	now := time.Now().UTC()
	// healthy agent: low utilization, recent activity, good balance
	healthy := AssessFloatRisk("a1", ledger.Balance{CreditsPosted: 10000000, DebitsPosted: 1000000},
		[]FloatMovement{{CreatedAt: now.Add(-time.Hour).Format(time.RFC3339)}}, now)
	if healthy.Band != "low" || healthy.LowFloatAlert {
		t.Fatalf("healthy agent misclassified: %+v", healthy)
	}
	// risky agent: fully utilized, dormant 45 days, below low-float threshold
	risky := AssessFloatRisk("a2", ledger.Balance{CreditsPosted: 1000000, DebitsPosted: 990000},
		[]FloatMovement{{CreatedAt: now.Add(-45 * 24 * time.Hour).Format(time.RFC3339)}}, now)
	if risky.Band != "critical" || !risky.LowFloatAlert || risky.DormancyDays != 45 {
		t.Fatalf("risky agent misclassified: %+v", risky)
	}
}

func TestWebhookCaptureFlow(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-webhook")
	p, auth, err := ts.pay.Authorise(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: auth.Reference, Event: "charge.successful", PaymentID: p.ID})
	if err != nil || got.Status != "captured" {
		t.Fatalf("webhook capture: %+v %v", got, err)
	}
}
