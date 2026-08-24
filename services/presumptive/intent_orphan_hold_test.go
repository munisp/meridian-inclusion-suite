package main

// intent_orphan_hold_test.go — V2 repair (B3 #7 residual): when the ledger
// hold succeeded but the payments Put failed, the claim was released while
// the pending hold was orphaned (no payment row references it; no sweeper
// ever voids it). CreateIntent must now issue a compensating void.

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
)

// voidSpyLedger records VoidPending calls.
type voidSpyLedger struct {
	ledger.Client
	voided []string
}

func (v *voidSpyLedger) VoidPending(id string) (string, error) {
	v.voided = append(v.voided, id)
	return v.Client.VoidPending(id)
}

func TestCreateIntentStoreFailureVoidsHold(t *testing.T) {
	ts := newTestStack(t)
	spy := &voidSpyLedger{Client: ts.lc}
	ts.pay.lc = spy
	var armed atomic.Bool
	armed.Store(true)
	ts.st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" && coll == "payments" {
			return errSimDBTimeout
		}
		return nil
	})
	_, err := ts.pay.CreateIntent(dbfIntentReq("dbf-void"))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected db timeout error, got %v", err)
	}
	if len(spy.voided) != 1 {
		t.Fatalf("compensating void calls = %v, want exactly 1", spy.voided)
	}
	// the voided hold is consumed: posting it must now fail (it is no
	// longer pending), proving the orphaned hold was really released.
	if _, lerr := ts.lc.PostPending(spy.voided[0], 1); lerr == nil {
		t.Fatal("voided hold still postable — compensating void did not release it")
	}
	// retry after recovery succeeds and places a NEW hold (not a replay of
	// the voided one)
	ts.st.SetFaultHook(nil)
	p, err := ts.pay.CreateIntent(dbfIntentReq("dbf-void"))
	if err != nil {
		t.Fatal(err)
	}
	if p.PendingTransferID == spy.voided[0] {
		t.Fatal("retry reused the voided hold")
	}
	if len(spy.voided) != 1 {
		t.Fatalf("no further voids expected, got %v", spy.voided)
	}
}
