package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

type testStack struct {
	st     *store.Store
	lc     *ledger.DevClient
	engine *BandEngine
	pay    *PaymentService
	float  *FloatService
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
	gateFile := filepath.Join(t.TempDir(), "gates.json")
	gates := &GateClient{file: gateFile}
	certs := NewCertificateService(st)
	bus := events.NewInprocBus()
	pay := NewPaymentService(st, lc, NewPSSPHub(), engine, gates, certs, bus)
	floats := NewFloatService(st, lc)
	wf := NewPSMWorkflows(st, pay, floats, engine, gates, lc, bus)
	return &testStack{st: st, lc: lc, engine: engine, pay: pay, float: floats, gates: gates, certs: certs, wf: wf}
}

func (ts *testStack) openGate(t *testing.T) {
	t.Helper()
	if _, err := ts.gates.Flip(presumptiveGateID, true); err != nil {
		t.Fatal(err)
	}
}

func TestBandEngine(t *testing.T) {
	ts := newTestStack(t)
	// exemption below N800k
	ex := ts.engine.Evaluate("Lagos", "retail", 70000000, false, 0)
	if !ex.Exempt || ex.PackID != "rp-exemption-nta" {
		t.Fatalf("expected exemption, got %+v", ex)
	}
	// lagos small band retail = N15,000/yr
	lag := ts.engine.Evaluate("Lagos", "retail", 300000000, false, 0)
	if lag.Exempt || lag.Band != "small" || lag.PackID != "rp-presumptive-lagos" || lag.AnnualLevyKobo != 1600000 {
		t.Fatalf("unexpected lagos eval: %+v", lag)
	}
	if lag.MonthlyLevyKobo != lag.AnnualLevyKobo/12 {
		t.Fatal("monthly should be annual/12")
	}
	// unknown state -> federal fallback
	fed := ts.engine.Evaluate("Borno", "tailoring", 300000000, false, 0)
	if fed.PackID != "rp-presumptive-federal" || fed.AnnualLevyKobo != 1000000 {
		t.Fatalf("expected federal fallback, got %+v", fed)
	}
	// above ceiling -> graduate
	grad := ts.engine.Evaluate("Kano", "transport", 9000000000, false, 0)
	if !grad.Graduate {
		t.Fatalf("expected graduate, got %+v", grad)
	}
	if len(lag.Trace) == 0 {
		t.Fatal("expected calc trace")
	}
}

func TestGateBlocksCollections(t *testing.T) {
	ts := newTestStack(t)
	_, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "abc", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita",
	})
	if err != ErrGateClosed {
		t.Fatalf("expected gate-closed error, got %v", err)
	}
	ts.openGate(t)
	if _, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "abc", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita",
	}); err != nil {
		t.Fatalf("expected intent after gate flip, got %v", err)
	}
}

func TestPaymentLifecycleAndCertificate(t *testing.T) {
	ts := newTestStack(t)
	ts.openGate(t)
	p, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-1", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "pending_authorisation" || p.PendingTransferID == "" {
		t.Fatalf("bad intent: %+v", p)
	}
	expected := uint64(1600000 + 10000) // lagos small retail + admin fee
	if p.AmountKobo != expected {
		t.Fatalf("amount %d != %d", p.AmountKobo, expected)
	}
	p, auth, err := ts.pay.Authorise(p.ID)
	if err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	p, cert, err := ts.pay.Capture(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "captured" || cert.Serial == "" || cert.Signature == "" {
		t.Fatalf("bad capture: %+v %+v", p, cert)
	}
	// ledger posted
	bal, err := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if err != nil || bal.CreditsPosted != expected {
		t.Fatalf("ledger collections: %+v %v", bal, err)
	}
	// certificate verifies
	got, valid, err := ts.certs.Verify(cert.Serial)
	if err != nil || !valid || got.Serial != cert.Serial {
		t.Fatalf("verify: %+v valid=%v %v", got, valid, err)
	}
	// tampered cert fails
	got.AmountKobo += 100
	if SignCertificate(got) == cert.Signature {
		t.Fatal("tampered payload must not verify")
	}
	// void-after-capture rejected
	if _, err := ts.pay.Void(p.ID); err == nil {
		t.Fatal("expected void-after-capture error")
	}
}

func TestWebhookSignature(t *testing.T) {
	body := []byte(`{"reference":"RRR-X","event":"charge.successful"}`)
	mac := hmac.New(sha256.New, []byte(webhookSecret()))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !VerifyWebhookSignature(sig, body) {
		t.Fatal("expected signature to verify")
	}
	if VerifyWebhookSignature("deadbeef", body) {
		t.Fatal("bad signature must fail")
	}
}

func TestFloatOverdraftEnforced(t *testing.T) {
	ts := newTestStack(t)
	if _, err := ts.float.Topup("agent-1", 1000000, "seed"); err != nil {
		t.Fatal(err)
	}
	bal, err := ts.float.Balance("agent-1")
	if err != nil || bal.CreditsPosted != 1000000 {
		t.Fatalf("balance: %+v %v", bal, err)
	}
	if _, err := ts.float.Debit("agent-1", 400000, "remit-1"); err != nil {
		t.Fatal(err)
	}
	// overdraft must fail with ErrExceedsCredits (DEBITS_MUST_NOT_EXCEED_CREDITS)
	if _, err := ts.float.Debit("agent-1", 700000, "remit-2"); err != ledger.ErrExceedsCredits {
		t.Fatalf("expected overdraft error, got %v", err)
	}
	bal, _ = ts.float.Balance("agent-1")
	if bal.NetPosted() != 600000 {
		t.Fatalf("net posted %d != 600000", bal.NetPosted())
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
	if sim.Totals["grand_total"] != 1600000+2500000 {
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
