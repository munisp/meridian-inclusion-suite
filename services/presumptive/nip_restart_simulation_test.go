package main

// nip_restart_simulation_test.go — §6.3 NIP crash-during-dispatch window
// (assurance R7 item 4 / w2 mutation M1a). Unlike the hand-crafted
// in-flight record in TestPayoutRestartDedupesAndSweeps, this harness runs
// Payout against a rail that BLOCKS mid-dispatch (the stand-in for a
// process kill while the rail call is outstanding), asserts the durable
// pre-dispatch in_flight record already exists at that instant, then
// simulates a restart with a fresh service over the same store and proves
// recovery adopts the record instead of double-dispatching. Without the
// pre-dispatch durable Put pair (mutation M1a) this test FAILS.

import (
	"sync"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// blockingRail blocks inside FundsTransfer until released — the stand-in
// for a process killed while the rail call is outstanding.
type blockingRail struct {
	NIPRail
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRail) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return r.NIPRail.FundsTransfer(req)
}

func TestNIPCrashDuringDispatchRestartSeesInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	rail := &blockingRail{NIPRail: NewNIPSim(), entered: entered, release: release}

	st, err := store.Open(t.TempDir() + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewNIPService(rail, st, nil, true)
	req := payoutReq("0123456789")

	type outcome struct {
		tr  NIPTransfer
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		tr, err := svc.Payout(req)
		done <- outcome{tr, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rail dispatch never entered")
	}
	// The process is "dead" mid-dispatch. The durable in_flight record must
	// ALREADY exist (this is the assertion the M1a mutant fails).
	var inflight NIPTransfer
	ok, err := st.Get("nip_transfers", "idem:"+req.IdempotencyKey, &inflight)
	if err != nil || !ok || inflight.Status != NIPStatusInFlight {
		t.Fatalf("pre-dispatch durable record must exist during dispatch (M1a window): ok=%v err=%v rec=%+v", ok, err, inflight)
	}
	// Restart: fresh service + fresh rail over the SAME store.
	rail2 := &faultCountingRail{NIPRail: NewNIPSim()}
	restarted := NewNIPService(rail2, st, nil, true)
	tr, err := restarted.Payout(req)
	if err != nil {
		t.Fatalf("restart retry must return the in-flight record, got %v", err)
	}
	if tr.SessionID != inflight.SessionID || tr.ID != inflight.ID {
		t.Fatalf("restart must adopt the SAME transfer record: %+v vs %+v", tr, inflight)
	}
	if rail2.dispatches.Load() != 0 {
		t.Fatalf("restart must NOT double-dispatch, got %d rail calls", rail2.dispatches.Load())
	}
	// Let the original (killed) call land; the record resolves consistently.
	close(release)
	out := <-done
	if out.err != nil || out.tr.Status != NIPStatusSuccess {
		t.Fatalf("original dispatch: %+v err=%v", out.tr, out.err)
	}
	var final NIPTransfer
	if ok, _ := st.Get("nip_transfers", "idem:"+req.IdempotencyKey, &final); !ok ||
		final.Status != NIPStatusSuccess || final.SessionID != inflight.SessionID {
		t.Fatalf("final record must be the same transfer, resolved: %+v", final)
	}
}
