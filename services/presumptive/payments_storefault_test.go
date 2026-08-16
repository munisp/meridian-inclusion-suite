package main

// payments_storefault_test.go — §6.3 db-fault cells for the PSM capture/void
// (C/V) flow (assurance R7): a db timeout or deadlock on the intent state
// write must surface an error (never a silent success), must not move
// posted money, and the flow must be safely retryable after the db
// recovers.

import (
	"strings"
	"sync/atomic"
	"testing"
)

func dbfIntentReq(key string) IntentRequest {
	return IntentRequest{
		TINHash: "tinhash-dbf-" + key, State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: key,
	}
}

func TestCreateIntentDBTimeoutOnStateWrite(t *testing.T) {
	ts := newTestStack(t)
	var armed atomic.Bool
	armed.Store(true)
	ts.st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" && coll == "payments" {
			return errSimDBTimeout
		}
		return nil
	})
	_, err := ts.pay.CreateIntent(dbfIntentReq("dbf-t1"))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected db timeout error, got %v", err)
	}
	// fail-closed: no payment record, no idempotency mapping
	var payments []Payment
	ts.st.SetFaultHook(nil)
	if err := ts.st.List("payments", &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 0 {
		t.Fatalf("phantom payment record: %+v", payments)
	}
	// retry after recovery succeeds and is safely replayable
	p1, err := ts.pay.CreateIntent(dbfIntentReq("dbf-t1"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ts.pay.CreateIntent(dbfIntentReq("dbf-t1"))
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("retry diverged: %s vs %s", p1.ID, p2.ID)
	}
}

func TestCreateIntentDBDeadlockRetryable(t *testing.T) {
	ts := newTestStack(t)
	var armed atomic.Bool
	armed.Store(true)
	ts.st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" && coll == "payments" {
			return errSimDeadlock
		}
		return nil
	})
	if _, err := ts.pay.CreateIntent(dbfIntentReq("dbf-d1")); err == nil {
		t.Fatal("expected deadlock error")
	}
	armed.Store(false)
	p, err := ts.pay.CreateIntent(dbfIntentReq("dbf-d1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "pending_authorisation" || p.PendingTransferID == "" {
		t.Fatalf("after recovery: %+v", p)
	}
}

// db fault on the idempotency-key replay READ: fail closed, never a
// guessing replay.
func TestCreateIntentDBTimeoutOnReadFailsClosed(t *testing.T) {
	ts := newTestStack(t)
	p1, err := ts.pay.CreateIntent(dbfIntentReq("dbf-r1"))
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	armed.Store(true)
	ts.st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "get" && coll == "idempotency" {
			return errSimDBTimeout
		}
		return nil
	})
	_, err = ts.pay.CreateIntent(dbfIntentReq("dbf-r1"))
	ts.st.SetFaultHook(nil)
	// fail closed: the read fault aborts the retry with an error, and no
	// second payment/hold is created for the reused key
	if err == nil || !strings.Contains(err.Error(), "idempotency lookup") {
		t.Fatalf("expected fail-closed lookup error, got %v", err)
	}
	var payments []Payment
	if err := ts.st.List("payments", &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 || payments[0].ID != p1.ID {
		t.Fatalf("double-created on read fault: %+v", payments)
	}
}
