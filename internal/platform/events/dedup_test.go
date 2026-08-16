package events

import (
	"errors"
	"testing"
	"time"
)

// §6.3 "consumer crash" + at-least-once redelivery coverage (R7 item 3).

func dedupTestEnv(id string) Envelope {
	return Envelope{ID: id, Type: "nrs.test.v1", Source: "test", Data: map[string]any{"n": 1}}
}

// Duplicate delivery (ack lost / replay): the handler runs exactly once.
func TestDedupDuplicateDeliverySingleEffect(t *testing.T) {
	d := NewDedupConsumer(NewInprocProcessedStore())
	env := dedupTestEnv("evt-1")
	calls := 0
	fn := func(Envelope) error { calls++; return nil }
	if err := d.Handle("topic-a", env, fn); err != nil {
		t.Fatal(err)
	}
	// at-least-once redelivery of the same event
	if err := d.Handle("topic-a", env, fn); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate delivery must invoke the handler once, got %d", calls)
	}
	// a DIFFERENT event id on the same topic is not deduped
	if err := d.Handle("topic-a", dedupTestEnv("evt-2"), fn); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("distinct event must be processed, got %d calls", calls)
	}
}

// Crash after the effect but before the ack: the claim persists, so the
// replay is deduped and the handler is NOT re-invoked.
func TestDedupCrashAfterEffectBeforeAck(t *testing.T) {
	st := &completeFailStore{InprocProcessedStore: NewInprocProcessedStore()}
	d := NewDedupConsumer(st)
	env := dedupTestEnv("evt-crash")
	effects := 0
	fn := func(Envelope) error { effects++; return nil }
	// First delivery: effect applied, then the process "dies" before the ack
	// (Complete never lands — the claim row survives).
	if err := d.Handle("topic-a", env, fn); err == nil {
		t.Fatal("expected the lost-ack (Complete failure) to surface")
	}
	if effects != 1 {
		t.Fatalf("effect must be applied once, got %d", effects)
	}
	// Restart + replay over the SAME processed store: deduped, no 2nd effect.
	d2 := NewDedupConsumer(st)
	if err := d2.Handle("topic-a", env, fn); err != nil {
		t.Fatalf("replayed delivery must be acked, got %v", err)
	}
	if effects != 1 {
		t.Fatalf("crash-before-ack replay must not re-run the handler, effects=%d", effects)
	}
}

// completeFailStore simulates a crash between handler effect and ack: Claim
// and Seen behave normally, Complete never lands.
type completeFailStore struct{ *InprocProcessedStore }

func (s *completeFailStore) Complete(key string) error {
	return errors.New("simulated crash: ack lost")
}

// Handler failure releases the claim: the redelivery retries the handler.
func TestDedupHandlerErrorAllowsRedelivery(t *testing.T) {
	d := NewDedupConsumer(NewInprocProcessedStore())
	env := dedupTestEnv("evt-flaky")
	calls := 0
	boom := errors.New("downstream down")
	fn := func(Envelope) error {
		calls++
		if calls == 1 {
			return boom
		}
		return nil
	}
	if err := d.Handle("topic-a", env, fn); !errors.Is(err, boom) {
		t.Fatalf("handler error must surface (no ack), got %v", err)
	}
	if err := d.Handle("topic-a", env, fn); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("redelivery after handler failure must retry, got %d calls", calls)
	}
}

// Dedup keys can also be derived from Kafka delivery coordinates.
func TestDedupOffsetKey(t *testing.T) {
	d := NewDedupConsumer(NewInprocProcessedStore())
	calls := 0
	fn := func(Envelope) error { calls++; return nil }
	key := OffsetKey("topic-b", 3, 42)
	if err := d.HandleKey(key, dedupTestEnv("evt-x"), fn); err != nil {
		t.Fatal(err)
	}
	// same coordinates, re-published under a fresh event id: still deduped
	if err := d.HandleKey(key, dedupTestEnv("evt-y"), fn); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("offset-keyed dedup failed, calls=%d", calls)
	}
	if OffsetKey("topic-b", 3, 42) == OffsetKey("topic-b", 3, 43) {
		t.Fatal("offset keys must differ by offset")
	}
}

// A store outage must NOT ack the delivery (error propagates; no claim).
func TestDedupStoreOutageNoAck(t *testing.T) {
	d := NewDedupConsumer(&outageStore{})
	calls := 0
	err := d.Handle("topic-a", dedupTestEnv("evt-z"), func(Envelope) error { calls++; return nil })
	if err == nil {
		t.Fatal("store outage must surface, delivery must not be acked")
	}
	if calls != 0 {
		t.Fatal("handler must not run when the dedup store is down")
	}
}

type outageStore struct{}

func (o *outageStore) Claim(string) (bool, error) { return false, errors.New("db down") }
func (o *outageStore) Complete(string) error      { return errors.New("db down") }
func (o *outageStore) Release(string) error       { return errors.New("db down") }
func (o *outageStore) Seen(string) (bool, error)  { return false, errors.New("db down") }

// Stale claims from the claim→effect crash window can be reclaimed.
func TestDedupReclaimStale(t *testing.T) {
	st := NewInprocProcessedStore()
	d := NewDedupConsumer(st)
	_ = d.HandleKey("k1", dedupTestEnv("e1"), func(Envelope) error { return errors.New("boom") }) // released
	cf := &completeFailStore{InprocProcessedStore: st}
	_ = NewDedupConsumer(cf).HandleKey("k2", dedupTestEnv("e2"), func(Envelope) error { return nil }) // stuck claim
	if n := ReclaimStale(st, time.Now().Add(time.Minute)); n != 1 {
		t.Fatalf("expected 1 stale claim reclaimed, got %d", n)
	}
	calls := 0
	if err := d.HandleKey("k2", dedupTestEnv("e2"), func(Envelope) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("reclaimed key must be reprocessable: err=%v calls=%d", err, calls)
	}
}
