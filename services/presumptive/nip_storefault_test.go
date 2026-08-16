package main

// nip_storefault_test.go — §6.3 db-fault + store-fault cells for the NIP
// flow (assurance R7), incl. the M1a recommendation (mutation-invisible
// pre-dispatch durable write): a failing pre-dispatch Put must ABORT the
// rail dispatch, and a post-dispatch state-write fault must leave a
// durable in_flight record that dedupes the client retry and is resolved
// by the TSQ sweeper.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

var errSimDBTimeout = errors.New("simulated db timeout on state write")
var errSimDeadlock = errors.New("simulated db deadlock (SQLSTATE 40P01)")

// faultCountingRail wraps the sim rail and counts rail-side effects.
type faultCountingRail struct {
	NIPRail
	dispatches atomic.Int32
}

func (c *faultCountingRail) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	c.dispatches.Add(1)
	return c.NIPRail.FundsTransfer(req)
}

func newCountingNIP(t *testing.T) (*NIPService, *faultCountingRail, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	rail := &faultCountingRail{NIPRail: NewNIPSim()}
	return NewNIPService(rail, st, nil, true), rail, st
}

// M1a: a failing pre-dispatch durable Put must abort the dispatch — the
// rail must NEVER see a transfer that has no durable record.
func TestPayoutPreDispatchStoreFaultAbortsDispatch(t *testing.T) {
	svc, rail, st := newCountingNIP(t)
	st.SetFaultHook(func(op, coll, id string) error {
		if op == "put" && coll == "nip_transfers" {
			return errSimDBTimeout
		}
		return nil
	})
	_, err := svc.Payout(payoutReq("0123456789"))
	if err == nil || !strings.Contains(err.Error(), "persist pre-dispatch record") {
		t.Fatalf("expected pre-dispatch persist failure, got %v", err)
	}
	if rail.dispatches.Load() != 0 {
		t.Fatalf("rail dispatched without a durable record (dispatches=%d)", rail.dispatches.Load())
	}
	// nothing durable: a retry after recovery is a fresh, single dispatch
	st.SetFaultHook(nil)
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != NIPStatusSuccess || rail.dispatches.Load() != 1 {
		t.Fatalf("recovery: %+v dispatches=%d", tr, rail.dispatches.Load())
	}
}

// db deadlock on the pre-dispatch write: refused, no dispatch, retryable.
func TestPayoutPreDispatchDeadlockRetryable(t *testing.T) {
	svc, rail, st := newCountingNIP(t)
	var armed atomic.Bool
	armed.Store(true)
	st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" {
			return errSimDeadlock
		}
		return nil
	})
	if _, err := svc.Payout(payoutReq("0123456789")); err == nil {
		t.Fatal("expected deadlock error")
	}
	if rail.dispatches.Load() != 0 {
		t.Fatal("rail must not have been called")
	}
	armed.Store(false)
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil || tr.Status != NIPStatusSuccess {
		t.Fatalf("retry: %v %+v", err, tr)
	}
	if rail.dispatches.Load() != 1 {
		t.Fatalf("dispatches=%d", rail.dispatches.Load())
	}
}

// kill AFTER provider effect BEFORE local persist: the rail dispatch lands
// but the post-dispatch state write fails. The durable in_flight record
// dedupes the client retry (NO second rail call) and the TSQ sweeper
// reconciles to success.
func TestPayoutPostDispatchWriteFaultSweeperReconciles(t *testing.T) {
	svc, rail, st := newCountingNIP(t)
	var dispatched atomic.Bool
	st.SetFaultHook(func(op, coll, id string) error {
		// once the rail has been called, every state write times out
		if dispatched.Load() && op == "put" {
			return errSimDBTimeout
		}
		return nil
	})
	orig := rail.NIPRail
	rail.NIPRail = &dispatchMarker{NIPRail: orig, fired: &dispatched}
	_, err := svc.Payout(payoutReq("0123456789"))
	if err == nil || !strings.Contains(err.Error(), "update transfer record") {
		t.Fatalf("expected post-dispatch persist failure, got %v", err)
	}
	if rail.dispatches.Load() != 1 {
		t.Fatalf("dispatches=%d", rail.dispatches.Load())
	}
	// client retry with the same key: replays the durable in_flight record,
	// must NOT re-dispatch even though the final write was lost
	st.SetFaultHook(nil)
	b, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != NIPStatusInFlight {
		t.Fatalf("retry: %+v", b)
	}
	if rail.dispatches.Load() != 1 {
		t.Fatalf("retry double-dispatched: %d", rail.dispatches.Load())
	}
	// TSQ sweeper reconciles the in_flight record from rail state
	n, err := svc.SweepTSQ()
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	var stored NIPTransfer
	if ok, err := st.Get("nip_transfers", "idem:"+b.IdempotencyKey, &stored); err != nil || !ok {
		t.Fatal("record missing")
	}
	if stored.Status != NIPStatusSuccess {
		t.Fatalf("after sweep: %+v", stored)
	}
}

// delayed restart: a new service over the same store file still dedupes
// the retry and sweeps the in_flight record.
func TestPayoutRestartDedupesAndSweeps(t *testing.T) {
	dir := t.TempDir()
	st1, err := store.Open(dir + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	rail := &faultCountingRail{NIPRail: NewNIPSim()}
	svc1 := NewNIPService(rail, st1, nil, true)
	if _, err := svc1.Payout(payoutReq("0123456789")); err != nil {
		t.Fatal(err)
	}
	// simulate a crash mid-flight: force the durable record in_flight
	var all []NIPTransfer
	if err := st1.List("nip_transfers", &all); err != nil {
		t.Fatal(err)
	}
	for _, tr := range all {
		tr.Status = NIPStatusInFlight
		if err := st1.Put("nip_transfers", tr.SessionID, tr); err != nil {
			t.Fatal(err)
		}
		if err := st1.Put("nip_transfers", "idem:"+tr.IdempotencyKey, tr); err != nil {
			t.Fatal(err)
		}
	}
	// restart: new service + store over the same file
	st2, err := store.Open(dir + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewNIPService(rail, st2, nil, true)
	b, err := svc2.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != NIPStatusInFlight || rail.dispatches.Load() != 1 {
		t.Fatalf("post-restart retry: %+v dispatches=%d", b, rail.dispatches.Load())
	}
	if n, err := svc2.SweepTSQ(); err != nil || n != 1 {
		t.Fatalf("post-restart sweep: %d %v", n, err)
	}
}

// dispatchMarker flags when the rail effect has landed.
type dispatchMarker struct {
	NIPRail
	fired *atomic.Bool
}

func (d *dispatchMarker) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	d.fired.Store(true)
	return d.NIPRail.FundsTransfer(req)
}

// db fault on the idempotency replay READ: fail closed — never proceed to
// a possible second dispatch on a failed lookup.
func TestPayoutIdempotencyReadFaultFailsClosed(t *testing.T) {
	svc, rail, st := newCountingNIP(t)
	if _, err := svc.Payout(payoutReq("0123456789")); err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	armed.Store(true)
	st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "get" {
			return errSimDBTimeout
		}
		return nil
	})
	_, err := svc.Payout(payoutReq("0123456789"))
	st.SetFaultHook(nil)
	if err == nil || !strings.Contains(err.Error(), "idempotency lookup") {
		t.Fatalf("expected fail-closed lookup error, got %v", err)
	}
	if rail.dispatches.Load() != 1 {
		t.Fatalf("double dispatch on read fault: %d", rail.dispatches.Load())
	}
}
