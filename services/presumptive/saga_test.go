package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// failingLedger wraps the dev ledger and fails PostPending on demand to
// simulate a ledger outage mid-saga (after the PSSP capture succeeded).
type failingLedger struct {
	ledger.Client
	failPost bool
}

func (f failingLedger) PostPending(pendingID string, amount uint64) (string, error) {
	if f.failPost {
		return "", fmt.Errorf("simulated ledger outage")
	}
	return f.Client.PostPending(pendingID, amount)
}

func (f failingLedger) PostPendingAs(pendingID, postID string, amount uint64) (string, error) {
	if f.failPost {
		return "", fmt.Errorf("simulated ledger outage")
	}
	return f.Client.PostPendingAs(pendingID, postID, amount)
}

// TestCaptureSagaCompensation proves the capture saga is atomic: when the
// ledger leg fails after a successful PSSP capture, the payer is NOT left
// charged without a certificate — compensating actions refund the PSSP
// capture and persist a Compensation record.
func TestCaptureSagaCompensation(t *testing.T) {
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
	bus := events.NewInprocBus()
	pay := NewPaymentService(st, failingLedger{lc, true}, NewPSSPHub(), engine, gates, NewCertificateService(st), bus)

	p, err := pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-saga", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, auth, err := pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	p2, _, err := pay.Capture(p.ID)
	if err == nil {
		t.Fatal("capture must fail when the ledger leg fails")
	}
	if p2.Status != "compensated" {
		t.Fatalf("expected compensated status, got %s (%s)", p2.Status, p2.FailReason)
	}
	// compensation record persisted with a successful PSSP refund
	var comps []Compensation
	if err := st.List("compensations", &comps); err != nil || len(comps) != 1 {
		t.Fatalf("expected 1 compensation record, got %v err=%v", comps, err)
	}
	if comps[0].PSSPRefund != "ok" {
		t.Fatalf("expected PSSP refund ok, got %+v", comps[0])
	}
	// collections account must have received nothing
	bal, _ := lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPosted != 0 {
		t.Fatalf("compensated capture must post nothing to collections, got %+v", bal)
	}
	// payment is durably compensated (recon-worker visible)
	var got Payment
	if ok, _ := st.Get("payments", p.ID, &got); !ok || got.Status != "compensated" {
		t.Fatalf("persisted payment must be compensated, got %+v", got)
	}
}

// flakyCaptureAdapter simulates a PSSP whose Capture call fails at the
// transport level, with a controllable server-side Verify view (audit
// funds-flow #4 tests).
type flakyCaptureAdapter struct {
	PSSPAdapter
	captureFirst bool // perform the underlying capture before erroring
	verifyStatus string
	verifyErr    error
}

func (a *flakyCaptureAdapter) Name() string { return "flaky" }

func (a *flakyCaptureAdapter) Capture(reference string, amountKobo uint64, idem string) (CaptureResponse, error) {
	if a.captureFirst {
		// the provider actually captures, but the response is lost
		_, _ = a.PSSPAdapter.Capture(reference, amountKobo, idem)
	}
	return CaptureResponse{}, fmt.Errorf("simulated transport error")
}

func (a *flakyCaptureAdapter) Verify(reference string) (CaptureResponse, error) {
	if a.verifyErr != nil {
		return CaptureResponse{}, a.verifyErr
	}
	vr, err := a.PSSPAdapter.Verify(reference)
	if err != nil {
		return vr, err
	}
	vr.Status = a.verifyStatus
	return vr, nil
}

func newFlakyCaptureService(t *testing.T, adapter *flakyCaptureAdapter) (*PaymentService, *ledger.DevClient, *store.Store) {
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
	adapter.PSSPAdapter = newPSSPSim("flaky", "FLK-%s", FeeSchedule{RateBps: 100, CapKobo: 200000})
	hub.adapters["flaky"] = adapter
	pay := NewPaymentService(st, lc, hub, engine, gates, NewCertificateService(st), events.NewInprocBus())
	return pay, lc, st
}

func authoriseFlaky(t *testing.T, pay *PaymentService, tin string) Payment {
	t.Helper()
	p, err := pay.CreateIntent(IntentRequest{
		TINHash: tin, State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "flaky", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, auth, err := pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	p2, ok, _ := pay.get(p.ID)
	if !ok || p2.Status != "authorised" {
		t.Fatalf("payment must be authorised, got %+v", p2)
	}
	return p2
}

// Branch 1: capture transport error, but Verify shows the provider DID
// capture — the saga must continue (money moved), not mark failed.
func TestCaptureTransportErrorVerifyCaptured(t *testing.T) {
	adapter := &flakyCaptureAdapter{captureFirst: true, verifyStatus: "captured"}
	pay, lc, _ := newFlakyCaptureService(t, adapter)
	p := authoriseFlaky(t, pay, "tinhash-flaky-1")

	p2, _, err := pay.Capture(p.ID)
	if err != nil {
		t.Fatalf("verified capture must continue the saga: %v", err)
	}
	if p2.Status != "captured" {
		t.Fatalf("expected captured, got %s (%s)", p2.Status, p2.FailReason)
	}
	bal, _ := lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPosted != p2.AmountKobo {
		t.Fatalf("collections must hold the captured amount, got %+v", bal)
	}
}

// Branch 2: capture transport error and Verify confirms NOTHING was
// captured — fail terminally AND void the hold (previously the hold was
// left dangling on a blind 'failed').
func TestCaptureTransportErrorVerifyNotCaptured(t *testing.T) {
	adapter := &flakyCaptureAdapter{captureFirst: false, verifyStatus: "authorised"}
	pay, lc, _ := newFlakyCaptureService(t, adapter)
	p := authoriseFlaky(t, pay, "tinhash-flaky-2")

	p2, _, err := pay.Capture(p.ID)
	if err == nil {
		t.Fatal("capture must fail when the provider confirms no capture")
	}
	if p2.Status != "failed" {
		t.Fatalf("expected failed, got %s", p2.Status)
	}
	tr, err := lc.LookupTransfer(p.PendingTransferID)
	if err != nil || tr.Pending {
		t.Fatalf("hold must be voided on confirmed-not-captured, got %+v err=%v", tr, err)
	}
}

// Branch 3: capture transport error AND Verify indeterminate — the payment
// goes to capture_in_flight; the recovery sweeper later resolves it
// (here: provider confirms not captured -> failed + hold voided).
func TestCaptureTransportErrorIndeterminateThenSwept(t *testing.T) {
	adapter := &flakyCaptureAdapter{captureFirst: false, verifyErr: fmt.Errorf("verify unreachable")}
	pay, lc, st := newFlakyCaptureService(t, adapter)
	p := authoriseFlaky(t, pay, "tinhash-flaky-3")

	p2, _, err := pay.Capture(p.ID)
	if err == nil {
		t.Fatal("indeterminate capture must return an error")
	}
	if p2.Status != "capture_in_flight" {
		t.Fatalf("expected capture_in_flight, got %s", p2.Status)
	}
	// hold still in place while indeterminate
	if tr, _ := lc.LookupTransfer(p.PendingTransferID); !tr.Pending {
		t.Fatal("hold must remain while the capture is indeterminate")
	}

	// provider comes back: nothing was captured -> sweeper fails + voids
	adapter.verifyErr = nil
	adapter.verifyStatus = "authorised"
	sw := NewRecoverySweeper(pay, lc, events.NewInprocBus())
	if _, _, _, err := sw.SweepOnce(); err != nil {
		t.Fatal(err)
	}
	var got Payment
	if ok, _ := st.Get("payments", p.ID, &got); !ok || got.Status != "failed" {
		t.Fatalf("sweeper must fail the payment, got %+v", got)
	}
	if tr, _ := lc.LookupTransfer(p.PendingTransferID); tr.Pending {
		t.Fatal("sweeper must void the hold on confirmed-not-captured")
	}
}

// Branch 3b: indeterminate capture swept when the provider DID capture —
// the sweeper resumes the saga to captured exactly once.
func TestCaptureInFlightSweptToCaptured(t *testing.T) {
	adapter := &flakyCaptureAdapter{captureFirst: true, verifyErr: fmt.Errorf("verify unreachable")}
	pay, lc, st := newFlakyCaptureService(t, adapter)
	p := authoriseFlaky(t, pay, "tinhash-flaky-4")

	if _, _, err := pay.Capture(p.ID); err == nil {
		t.Fatal("indeterminate capture must return an error")
	}
	adapter.verifyErr = nil
	adapter.verifyStatus = "captured"
	sw := NewRecoverySweeper(pay, lc, events.NewInprocBus())
	resumed, _, _, err := sw.SweepOnce()
	if err != nil || resumed != 1 {
		t.Fatalf("sweeper must resume the capture: resumed=%d err=%v", resumed, err)
	}
	var got Payment
	if ok, _ := st.Get("payments", p.ID, &got); !ok || got.Status != "captured" {
		t.Fatalf("sweeper must capture the payment, got %+v", got)
	}
	bal, _ := lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.CreditsPosted != got.AmountKobo {
		t.Fatalf("collections must hold the captured amount exactly once, got %+v", bal)
	}
	// a second sweep is a no-op (idempotent)
	resumed, _, _, _ = sw.SweepOnce()
	if resumed != 0 {
		t.Fatalf("second sweep must be a no-op, resumed=%d", resumed)
	}
}
