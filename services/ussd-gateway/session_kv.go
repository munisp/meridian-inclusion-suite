package main

import (
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// session_kv.go — durable session stores enabling session RESUME (audit HIGH
// #8: a dropped USSD call meant a brand-new sessionId restarted the flow).
//
// KVSessionStore persists sessions in the embedded KV store (survives
// restarts, sliding TTL) and maintains a phone->session index so a redial
// from the same MSISDN can offer "continue last transaction".
// RedisSessionStore (redis.go) is used instead when REDIS_URL is set.

// PhoneIndexer is implemented by stores that can find a live session by
// MSISDN (for the resume prompt).
type PhoneIndexer interface {
	GetByPhone(phone string) (*Session, bool)
}

// KVSessionStore is the embedded-KV-backed SessionStore with TTL + MSISDN index.
type KVSessionStore struct {
	st  *store.Store
	ttl time.Duration
}

func NewKVSessionStore(st *store.Store, ttlSeconds int) *KVSessionStore {
	if ttlSeconds <= 0 {
		ttlSeconds = 180
	}
	return &KVSessionStore{st: st, ttl: time.Duration(ttlSeconds) * time.Second}
}

type phoneIndexEntry struct {
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *KVSessionStore) Get(id string) (*Session, bool) {
	var sess Session
	ok, err := s.st.Get("ussd_sessions", id, &sess)
	if err != nil || !ok {
		return nil, false
	}
	if sess.Expired(time.Now()) {
		s.Delete(id)
		return nil, false
	}
	sess.ExpiresAt = time.Now().Add(s.ttl) // sliding TTL
	return &sess, true
}

func (s *KVSessionStore) Put(sess *Session) {
	if sess.ExpiresAt.IsZero() {
		sess.ExpiresAt = time.Now().Add(s.ttl)
	}
	_ = s.st.Put("ussd_sessions", sess.ID, *sess)
	if sess.Phone != "" {
		_ = s.st.Put("ussd_phone_index", sess.Phone, phoneIndexEntry{SessionID: sess.ID, ExpiresAt: sess.ExpiresAt})
	}
}

func (s *KVSessionStore) Delete(id string) {
	var sess Session
	if ok, _ := s.st.Get("ussd_sessions", id, &sess); ok && sess.Phone != "" {
		s.st.Delete("ussd_phone_index", sess.Phone)
	}
	s.st.Delete("ussd_sessions", id)
}

// GetByPhone returns the live session last seen for an MSISDN, if any.
func (s *KVSessionStore) GetByPhone(phone string) (*Session, bool) {
	var idx phoneIndexEntry
	ok, err := s.st.Get("ussd_phone_index", phone, &idx)
	if err != nil || !ok || time.Now().After(idx.ExpiresAt) {
		return nil, false
	}
	return s.Get(idx.SessionID)
}

// InMem GetByPhone for the dev store (same resume semantics in tests/dev).
func (s *InMemSessionStore) GetByPhone(phone string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.m {
		if sess.Phone == phone && !sess.Expired(time.Now()) {
			sess.ExpiresAt = time.Now().Add(s.ttl)
			return sess, true
		}
	}
	return nil, false
}
