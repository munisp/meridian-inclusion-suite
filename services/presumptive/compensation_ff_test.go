package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// flakyRefundAdapter wraps the remita sim and fails Refund on demand
// (FF-5: a PSSP refund outage during compensation).
type flakyRefundAdapter struct {
	PSSPAdapter
	failRefund bool
	refunded   int
}

func (f *flakyRefundAdapter) Refund(reference string, amountKobo uint64) error {
	if f.failRefund {
		return fmt.Errorf("simulated PSSP refund outage")
	}
	f.refunded++
	return f.PSSPAdapter.Refund(reference, amountKobo)
}

func newCompStack(t *testing.T, lc ledger.Client, hub *PSSPHub) (*store.Store, *PaymentService) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	gates := &GateClient{file: filepath.Join(t.TempDir(), "gates.json")}
	if _, err := gates.Flip(presumptiveGateID, true); err != nil {
		t.Fatal(err)
	}
	pay := NewPaymentService(st, lc, hub, engine, gates, NewCertificateService(st), events.NewInprocBus())
	return st, pay
}

func compensatedPayment(t *testing.T, st *store.Store, pay *PaymentService) Payment {
	t.Helper()
	p, err := pay.CreateIntent(IntentRequest{
		TINHash: "tinhash-ff45", State: "Lagos", TradeCategory: "retail",
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
		t.Fatalf("expected compensated, got %s", p2.Status)
	}
	return p2
}

// FF-4 regression: compensation must void the payer's pending ledger hold —
// pre-fix the hold stayed pending forever, locking the payer's balance.
func TestCompensationVoidsPendingHold(t *testing.T) {
	lc := ledger.NewDevClient()
	st, pay := newCompStack(t, failingLedger{lc, true}, NewPSSPHub())
	p2 := compensatedPayment(t, st, pay)

	tr, err := lc.LookupTransfer(p2.PendingTransferID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Pending {
		t.Fatalf("FF-4: pending hold %s still locks payer funds after compensation", p2.PendingTransferID)
	}
	var comps []Compensation
	if err := st.List("compensations", &comps); err != nil || len(comps) != 1 {
		t.Fatalf("compensations: %v %v", comps, err)
	}
	if !strings.HasPrefix(comps[0].HoldVoid, "ok") {
		t.Fatalf("hold void must be recorded ok, got %q", comps[0].HoldVoid)
	}
}

// flakyVoidLedger fails VoidPending on demand (simulates a void outage at
// compensation time) while PostPendingAs stays broken for the saga.
type flakyVoidLedger struct {
	failingLedger
	failVoid bool
}

func (f flakyVoidLedger) VoidPending(pendingID string) (string, error) {
	if f.failVoid {
		return "", fmt.Errorf("simulated void outage")
	}
	return f.failingLedger.Client.VoidPending(pendingID)
}

// FF-4 sweeper branch: a compensated payment whose hold void failed at
// compensation time gets the hold voided by the recovery sweep.
func TestSweeperVoidsDanglingHoldOnCompensated(t *testing.T) {
	lc := ledger.NewDevClient()
	fl := &flakyVoidLedger{failingLedger: failingLedger{lc, true}, failVoid: true}
	st, pay := newCompStack(t, fl, NewPSSPHub())
	p2 := compensatedPayment(t, st, pay)
	pendID := p2.PendingTransferID
	tr, _ := lc.LookupTransfer(pendID)
	if !tr.Pending {
		t.Fatal("setup: hold must still be pending after failed void")
	}
	var comps []Compensation
	if err := st.List("compensations", &comps); err != nil || len(comps) != 1 {
		t.Fatalf("compensations: %v %v", comps, err)
	}
	if !strings.HasPrefix(comps[0].HoldVoid, "failed:") {
		t.Fatalf("setup: expected failed hold-void marker, got %q", comps[0].HoldVoid)
	}
	// void path recovers; the sweep must void the dangling hold and update
	// the compensation record
	fl.failVoid = false
	sw := NewRecoverySweeper(pay, lc, events.NewInprocBus())
	if _, _, _, _, err := sw.SweepOnce(); err != nil {
		t.Fatal(err)
	}
	tr2, _ := lc.LookupTransfer(pendID)
	if tr2.Pending {
		t.Fatal("sweeper must void dangling hold on compensated payment")
	}
	var after []Compensation
	_ = st.List("compensations", &after)
	if strings.HasPrefix(after[0].HoldVoid, "failed:") {
		t.Fatalf("hold-void marker not cleared: %q", after[0].HoldVoid)
	}
}

// FF-5 regression: a failed PSSP refund must be retried by the sweeper until
// it lands, not terminally recorded "failed:" while the payer stays charged.
func TestFailedPSSPRefundRetriedBySweeper(t *testing.T) {
	lc := ledger.NewDevClient()
	hub := NewPSSPHub()
	flaky := &flakyRefundAdapter{PSSPAdapter: hub.adapters["remita"], failRefund: true}
	hub.adapters["remita"] = flaky
	st, pay := newCompStack(t, failingLedger{lc, true}, hub)
	compensatedPayment(t, st, pay)

	var comps []Compensation
	if err := st.List("compensations", &comps); err != nil || len(comps) != 1 {
		t.Fatalf("compensations: %v %v", comps, err)
	}
	if !strings.HasPrefix(comps[0].PSSPRefund, "failed:") {
		t.Fatalf("expected failed refund marker, got %q", comps[0].PSSPRefund)
	}

	// provider recovers; the sweep must retry and close the compensation
	flaky.failRefund = false
	sw := NewRecoverySweeper(pay, lc, events.NewInprocBus())
	if _, _, _, _, err := sw.SweepOnce(); err != nil {
		t.Fatal(err)
	}
	if flaky.refunded == 0 {
		t.Fatal("sweeper never retried the failed refund")
	}
	var after []Compensation
	if err := st.List("compensations", &after); err != nil || len(after) != 1 {
		t.Fatalf("compensations after: %v %v", after, err)
	}
	if strings.HasPrefix(after[0].PSSPRefund, "failed:") {
		t.Fatalf("refund still marked failed after successful retry: %q", after[0].PSSPRefund)
	}
	// idempotent: further sweeps do not refund again
	n := flaky.refunded
	if _, _, _, _, err := sw.SweepOnce(); err != nil {
		t.Fatal(err)
	}
	if flaky.refunded != n {
		t.Fatalf("refund retried after success: %d -> %d", n, flaky.refunded)
	}
}
