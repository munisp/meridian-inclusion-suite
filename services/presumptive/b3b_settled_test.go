package main

import (
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
)

// B3 #12 regression: "settled" was an unreachable payment status (no
// writer). The recovery sweeper now performs the post-settlement
// transition once the post transfer is verified posted on the ledger.
func TestCapturedPaymentSettlesAfterLedgerConfirmation(t *testing.T) {
	ts := newPSMTestStack(t)
	p := ts.authorisedPayment(t, "settle1")
	// drive the full capture saga to "captured"
	if _, _, err := ts.pay.Capture(p.ID); err != nil {
		t.Fatal(err)
	}
	var got Payment
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.Status != "captured" {
		t.Fatalf("want captured, got %+v", got)
	}
	sw := NewRecoverySweeper(ts.pay, ts.lc, events.NewInprocBus())
	_, _, _, settled, err := sw.SweepOnce()
	if err != nil {
		t.Fatal(err)
	}
	if settled != 1 {
		t.Fatalf("settled=%d, want 1", settled)
	}
	if ok, _ := ts.st.Get("payments", p.ID, &got); !ok || got.Status != "settled" {
		t.Fatalf("want settled, got %+v", got)
	}
	// sweeping again is a no-op (settled is stable)
	if _, _, _, settled, _ = sw.SweepOnce(); settled != 0 {
		t.Fatal("second sweep must not re-settle")
	}
}
