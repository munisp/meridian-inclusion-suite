package main

// nip_publishfault_test.go — §6.3 publish-failure cell (assurance R9): a
// bus publish failure AFTER the rail dispatch must not silently lose the
// transfer-status event. The event lands in the durable nip_outbox and is
// relayed once the bus recovers; if the outbox write itself fails, the gap
// is logged loudly and the transfer record stays durable and correct.

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

var errSimBusDown = errors.New("simulated bus unavailable")

// flakyBus wraps the inproc bus and fails Publish while armed.
type flakyBus struct {
	*events.InprocBus
	down atomic.Bool
}

func (b *flakyBus) Publish(topic string, env events.Envelope) error {
	if b.down.Load() {
		return errSimBusDown
	}
	return b.InprocBus.Publish(topic, env)
}

func newNIPWithFlakyBus(t *testing.T) (*NIPService, *flakyBus, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	bus := &flakyBus{InprocBus: events.NewInprocBus()}
	return NewNIPService(NewNIPSim(), st, bus, true), bus, st
}

// bus down at publish time: the transfer still succeeds at the rail and is
// durably recorded, and the event is queued to nip_outbox (no silent loss).
func TestPayoutPublishFailureQueuedToOutbox(t *testing.T) {
	svc, bus, st := newNIPWithFlakyBus(t)
	bus.down.Store(true)
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != NIPStatusSuccess {
		t.Fatalf("transfer must still succeed: %+v", tr)
	}
	if got := len(bus.Published("nrs.psm.nip.v1")); got != 0 {
		t.Fatalf("bus must not have received the event, got %d", got)
	}
	var out []nipOutboxRecord
	if err := st.List("nip_outbox", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TransferID != tr.ID {
		t.Fatalf("expected 1 outbox record for %s, got %+v", tr.ID, out)
	}

	// bus recovers: the relay drains the outbox exactly once per record.
	bus.down.Store(false)
	n, err := svc.RelayOutbox()
	if err != nil || n != 1 {
		t.Fatalf("relay: n=%d err=%v", n, err)
	}
	if got := len(bus.Published("nrs.psm.nip.v1")); got != 1 {
		t.Fatalf("expected 1 relayed event, got %d", got)
	}
	if err := st.List("nip_outbox", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("outbox must be drained, got %+v", out)
	}
	// re-relay is a no-op (no duplicate publish)
	if n, _ := svc.RelayOutbox(); n != 0 {
		t.Fatalf("re-relay must be a no-op, relayed %d", n)
	}
	if got := len(bus.Published("nrs.psm.nip.v1")); got != 1 {
		t.Fatalf("duplicate publish detected: %d events", got)
	}
}

// bus still down at relay time: the record stays queued and the attempt is
// recorded — nothing is dropped.
func TestRelayOutboxRetainsOnFailure(t *testing.T) {
	svc, bus, st := newNIPWithFlakyBus(t)
	bus.down.Store(true)
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := svc.RelayOutbox()
	if err != nil || n != 0 {
		t.Fatalf("relay while down: n=%d err=%v", n, err)
	}
	var out []nipOutboxRecord
	if err := st.List("nip_outbox", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Attempts != 1 || out[0].LastError == "" {
		t.Fatalf("record must be retained with attempt+error: %+v", out)
	}
	_ = tr
}

// publish fails AND the outbox write faults: the transfer record itself is
// still durable and in the correct terminal state — the failure is only in
// event delivery and is logged, never silent about the money movement.
func TestPayoutPublishAndOutboxFaultTransferStillDurable(t *testing.T) {
	svc, bus, st := newNIPWithFlakyBus(t)
	bus.down.Store(true)
	st.SetFaultHook(func(op, coll, id string) error {
		if op == "put" && coll == "nip_outbox" {
			return errSimDBTimeout
		}
		return nil
	})
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatalf("payout must not fail on event-delivery faults: %v", err)
	}
	if tr.Status != NIPStatusSuccess {
		t.Fatalf("status: %+v", tr)
	}
	st.SetFaultHook(nil)
	// the durable transfer record is intact and readable by session id
	persisted, ok, err := svc.getBySession(tr.SessionID)
	if err != nil || !ok {
		t.Fatalf("transfer record lost: ok=%v err=%v", ok, err)
	}
	if persisted.Status != NIPStatusSuccess {
		t.Fatalf("persisted: %+v", persisted)
	}
}
