// Package webhookguard provides replay protection for inbound webhooks
// (audit M-1): a timestamp-tolerance check (default ±5 minutes) plus a
// replay cache (seen-nonce set with TTL). The replay store is pluggable —
// an in-process store is provided for single-instance deploys; a Redis- or
// SQL-backed store can implement Store for multi-instance deploys.
//
// Wired into the ussd-gateway aggregator webhook and the presumptive PSSP
// webhook (X-PSSP-Timestamp + signature as the replay key).
package webhookguard

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	// ErrMissingHeaders: required timestamp/nonce headers absent (prod fail-closed).
	ErrMissingHeaders = errors.New("webhookguard: required timestamp/nonce headers missing")
	// ErrStaleTimestamp: timestamp outside the tolerance window -> 401.
	ErrStaleTimestamp = errors.New("webhookguard: timestamp outside tolerance")
	// ErrBadTimestamp: unparsable timestamp -> 401.
	ErrBadTimestamp = errors.New("webhookguard: malformed timestamp")
	// ErrReplay: nonce already seen within the cache TTL -> 409 dedup.
	ErrReplay = errors.New("webhookguard: replayed webhook")
)

// Store is the replay cache. SeenOrAdd records key for ttl and returns
// true when the key was already present (i.e. the webhook is a replay).
type Store interface {
	SeenOrAdd(key string, ttl time.Duration) bool
}

// InProcStore is a single-process replay cache with lazy expiry.
type InProcStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

// NewInProcStore returns an empty in-process replay cache.
func NewInProcStore() *InProcStore {
	return &InProcStore{seen: map[string]time.Time{}, now: time.Now}
}

func (s *InProcStore) SeenOrAdd(key string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// lazy sweep occasionally (bounded work: only on contention growth)
	if len(s.seen) > 4096 {
		for k, exp := range s.seen {
			if now.After(exp) {
				delete(s.seen, k)
			}
		}
	}
	if exp, ok := s.seen[key]; ok && now.Before(exp) {
		return true
	}
	s.seen[key] = now.Add(ttl)
	return false
}

// Guard checks webhook timestamp freshness and nonce uniqueness.
type Guard struct {
	// TimestampHeader carries the sender's timestamp (unix seconds or RFC3339).
	TimestampHeader string
	// NonceHeader carries a unique-per-delivery nonce (or signature).
	NonceHeader string
	// Tolerance is the accepted clock skew (default 5 minutes).
	Tolerance time.Duration
	// TTL is how long nonces are remembered (default 15 minutes).
	TTL time.Duration
	// Store is the replay cache (default: in-process).
	Store Store
	// RequireHeaders fails closed when the headers are absent (prod).
	// When false (dev), missing headers skip the checks with no error.
	RequireHeaders bool
	// Now is overridable for tests.
	Now func() time.Time
}

// NewGuard returns a Guard with defaults applied (±5 min tolerance,
// 15 min replay TTL, in-process store).
func NewGuard(timestampHeader, nonceHeader string, requireHeaders bool, store Store) *Guard {
	if store == nil {
		store = NewInProcStore()
	}
	return &Guard{
		TimestampHeader: timestampHeader,
		NonceHeader:     nonceHeader,
		Tolerance:       5 * time.Minute,
		TTL:             15 * time.Minute,
		Store:           store,
		RequireHeaders:  requireHeaders,
		Now:             time.Now,
	}
}

func parseTimestamp(v string) (time.Time, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.Parse(time.RFC3339, v)
}

// Check verifies the request's timestamp freshness and nonce uniqueness.
// The returned error is one of ErrMissingHeaders / ErrStaleTimestamp /
// ErrBadTimestamp (-> 401) or ErrReplay (-> 409).
func (g *Guard) Check(r *http.Request) error {
	now := g.Now
	if now == nil {
		now = time.Now
	}
	ts := r.Header.Get(g.TimestampHeader)
	nonce := r.Header.Get(g.NonceHeader)
	if ts == "" || nonce == "" {
		if g.RequireHeaders {
			return ErrMissingHeaders
		}
		// dev: check whatever is present
		if ts == "" {
			return nil
		}
	}
	if ts != "" {
		t, err := parseTimestamp(ts)
		if err != nil {
			return ErrBadTimestamp
		}
		tol := g.Tolerance
		if tol == 0 {
			tol = 5 * time.Minute
		}
		if d := now().Sub(t); d > tol || d < -tol {
			return ErrStaleTimestamp
		}
	}
	if nonce != "" {
		ttl := g.TTL
		if ttl == 0 {
			ttl = 15 * time.Minute
		}
		if g.Store.SeenOrAdd(g.NonceHeader+":"+nonce, ttl) {
			return ErrReplay
		}
	}
	return nil
}
