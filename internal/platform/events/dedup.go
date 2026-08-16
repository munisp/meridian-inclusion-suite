package events

// dedup.go — generic consumer-side dedup middleware (assurance R7 item 3).
//
// The buses are at-least-once: a redelivery after a consumer crash (effect
// applied, ack lost) must NOT invoke the handler a second time. DedupConsumer
// wraps a handler with a durable processed-message table keyed by
// topic+partition+offset (Kafka delivery coordinates) or, when those are
// absent, by topic+event id.
//
// Delivery semantics:
//   - first delivery: the key is claimed durably BEFORE the handler runs;
//     on handler success the claim is completed, on handler error it is
//     released so the redelivery retries the handler;
//   - duplicate delivery (ack lost, producer retry, replay): the existing
//     claim short-circuits — the handler is NOT invoked again and the
//     delivery is acked;
//   - crash after the effect but before the ack: the claim survives, so
//     the replay is deduped (effectively-once handler invocation).
//
// The window between claim and handler effect is crash-safe in the
// at-most-once direction only; ReclaimStale lets a sweeper release claims
// whose handler never ran (crash between claim and effect), keyed off the
// claim timestamp.

import (
	"fmt"
	"sync"
	"time"
)

// ProcessedStore is the durable processed-message table behind the dedup
// middleware. Implementations must make Claim atomic (insert-if-absent;
// a Postgres implementation uses INSERT ... ON CONFLICT DO NOTHING).
type ProcessedStore interface {
	// Claim records key as in-progress. claimed=false means the key is
	// already claimed or completed — the delivery is a duplicate.
	Claim(key string) (claimed bool, err error)
	// Complete marks a claimed key fully processed.
	Complete(key string) error
	// Release drops a claim after a handler failure so the next redelivery
	// re-enters the handler.
	Release(key string) error
	// Seen reports whether key has ever been claimed (claimed or completed).
	Seen(key string) (bool, error)
}

// claimRec is one processed-message row.
type claimRec struct {
	ClaimedAt  time.Time
	Completed  bool
	CompleteAt time.Time
}

// InprocProcessedStore is the dev/test ProcessedStore.
type InprocProcessedStore struct {
	mu   sync.Mutex
	recs map[string]*claimRec
}

func NewInprocProcessedStore() *InprocProcessedStore {
	return &InprocProcessedStore{recs: map[string]*claimRec{}}
}

func (s *InprocProcessedStore) Claim(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[key]; ok {
		return false, nil
	}
	s.recs[key] = &claimRec{ClaimedAt: time.Now().UTC()}
	return true, nil
}

func (s *InprocProcessedStore) Complete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[key]; ok {
		r.Completed = true
		r.CompleteAt = time.Now().UTC()
	}
	return nil
}

func (s *InprocProcessedStore) Release(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, key)
	return nil
}

func (s *InprocProcessedStore) Seen(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recs[key]
	return ok, nil
}

// DedupConsumer wraps a topic handler with processed-message dedup.
type DedupConsumer struct {
	st ProcessedStore
}

func NewDedupConsumer(st ProcessedStore) *DedupConsumer { return &DedupConsumer{st: st} }

// EventKey derives the dedup key from the envelope: topic + event id.
func EventKey(topic string, env Envelope) string { return topic + "|" + env.ID }

// OffsetKey derives the dedup key from Kafka delivery coordinates
// (preferred in prod where the same payload may be republished under a new
// event id but identical topic/partition/offset).
func OffsetKey(topic string, partition int, offset int64) string {
	return fmt.Sprintf("%s|%d|%d", topic, partition, offset)
}

// Handle processes one delivery of env on topic: exactly-once handler
// invocation under at-least-once delivery. A nil return acks the delivery
// (including deduped duplicates); a handler error releases the claim so the
// redelivery retries.
func (d *DedupConsumer) Handle(topic string, env Envelope, fn func(Envelope) error) error {
	return d.HandleKey(EventKey(topic, env), env, fn)
}

// HandleKey is Handle with an explicit dedup key (e.g. OffsetKey when
// partition/offset coordinates are available from the consumer).
func (d *DedupConsumer) HandleKey(key string, env Envelope, fn func(Envelope) error) error {
	seen, err := d.st.Seen(key)
	if err != nil {
		return err // store down: do NOT ack — redeliver later
	}
	if seen {
		return nil // duplicate delivery: deduped, ack without re-invoking
	}
	claimed, err := d.st.Claim(key)
	if err != nil {
		return err
	}
	if !claimed {
		return nil // raced with a concurrent consumer: deduped
	}
	if err := fn(env); err != nil {
		_ = d.st.Release(key) // handler failed: allow redelivery to retry
		return err
	}
	return d.st.Complete(key)
}

// Wrap adapts Handle to the Bus Subscribe signature.
func (d *DedupConsumer) Wrap(topic string, fn func(Envelope) error) func(Envelope) {
	return func(env Envelope) {
		// The inproc Bus Subscribe signature has no error return; dedup
		// state is authoritative regardless.
		_ = d.Handle(topic, env, fn)
	}
}

// ReclaimStale releases claims older than `before` that never completed —
// the crash-between-claim-and-effect window. Callers must pick a horizon
// safely larger than the slowest handler.
func ReclaimStale(st *InprocProcessedStore, before time.Time) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for k, r := range st.recs {
		if !r.Completed && r.ClaimedAt.Before(before) {
			delete(st.recs, k)
			n++
		}
	}
	return n
}
