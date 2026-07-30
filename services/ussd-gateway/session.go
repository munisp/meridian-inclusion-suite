// Package main implements services/ussd-gateway (SPEC §4): a USSD gateway
// with a JSON menu-graph DSL, a session engine (180s TTL, in-mem dev store),
// onboarding + presumptive menu trees, an Africa's-Talking-style webhook and
// a built-in simulator endpoint.
package main

import (
	"sync"
	"time"
)

const serviceName = "ussd-gateway"
const serviceVersion = "1.0.0"

// Session is one USSD session's state.
type Session struct {
	ID        string            `json:"id"`
	Phone     string            `json:"phone"`
	Menu      string            `json:"menu"` // current menu id
	Data      map[string]string `json:"data"` // collected inputs + action outputs
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// Expired reports whether the session is past TTL.
func (s *Session) Expired(now time.Time) bool { return now.After(s.ExpiresAt) }

// SessionStore is the session engine interface (Redis or in-mem; dev: in-mem).
type SessionStore interface {
	Get(id string) (*Session, bool)
	Put(s *Session)
	Delete(id string)
}

// InMemSessionStore is the dev in-memory store with lazy TTL expiry.
type InMemSessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*Session
}

func NewInMemSessionStore(ttlSeconds int) *InMemSessionStore {
	if ttlSeconds <= 0 {
		ttlSeconds = 180
	}
	return &InMemSessionStore{ttl: time.Duration(ttlSeconds) * time.Second, m: map[string]*Session{}}
}

func (s *InMemSessionStore) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if sess.Expired(time.Now()) {
		delete(s.m, id)
		return nil, false
	}
	// sliding TTL: each interaction extends the window
	sess.ExpiresAt = time.Now().Add(s.ttl)
	return sess, true
}

func (s *InMemSessionStore) Put(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ExpiresAt.IsZero() {
		sess.ExpiresAt = time.Now().Add(s.ttl)
	}
	s.m[sess.ID] = sess
}

func (s *InMemSessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}
