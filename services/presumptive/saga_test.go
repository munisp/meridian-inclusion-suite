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
