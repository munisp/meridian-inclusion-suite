// pin.go — USSD PIN for sensitive actions (audit M-2/M-7 follow-up).
//
// No PIN existed anywhere in the USSD flows; session identity rested solely
// on the aggregator HMAC. This adds PIN setup + verification for sensitive
// actions (TIN status today; balance when that action lands), with:
//   - hashed storage: HMAC-SHA256(pepper=USSD_PIN_PEPPER, phone:pin) —
//     never the raw PIN; keyx.MustKey fails closed in prod when unset
//   - attempt limiting: 3 failed verifications lock the MSISDN and emit a
//     nrs.ussd.pin_lock.v1 event
//   - pluggable store: in-proc default; a durable store implements PINStore
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
)

// maxPINStrikes is the failed-attempt budget before the MSISDN locks.
const maxPINStrikes = 3

// errPINLocked is returned (and surfaced via the menu error branch) when a
// locked MSISDN attempts verification.
var errPINLocked = errors.New("PIN locked after 3 failed attempts. Visit an NRS office or contact support to unlock.")

var pinFormat = regexp.MustCompile(`^\d{4,6}$`)

// PINRecord is the stored credential state for one MSISDN.
type PINRecord struct {
	Hash    string `json:"hash"`
	Salt    string `json:"salt,omitempty"` // B4-10: per-user random salt; empty = legacy record
	Strikes int    `json:"strikes"`
	Locked  bool   `json:"locked"`
}

// PINStore persists PIN records (in-proc default; durable impls plug in).
type PINStore interface {
	Get(phone string) (PINRecord, bool)
	Put(phone string, rec PINRecord)
}

// InMemPINStore is the default in-process PIN store.
type InMemPINStore struct {
	mu sync.Mutex
	m  map[string]PINRecord
}

func NewInMemPINStore() *InMemPINStore { return &InMemPINStore{m: map[string]PINRecord{}} }

func (s *InMemPINStore) Get(phone string) (PINRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[phone]
	return r, ok
}

func (s *InMemPINStore) Put(phone string, rec PINRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[phone] = rec
}

// PINManager owns hashing, verification and strike/lock policy.
type PINManager struct {
	store  PINStore
	pepper string
	events eventPublisher // may be nil in tests
}

// NewPINManager builds a manager. The pepper comes from USSD_PIN_PEPPER
// (keyx: fatal in prod when unset — fail closed).
func NewPINManager(store PINStore, bus eventPublisher) *PINManager {
	if store == nil {
		store = NewInMemPINStore()
	}
	return &PINManager{store: store, pepper: keyx.MustKey("USSD_PIN_PEPPER", "dev-pin-pepper-do-not-deploy"), events: bus}
}

// pinIterations is the iterated-HMAC work factor (B4-10): a USSD PIN is a
// 4-6 digit number, so a single-pass hash is brute-forced in milliseconds.
// 10000 HMAC-SHA256 iterations with a per-user random salt raises offline
// attack cost and defeats cross-user rainbow tables. No new deps.
const pinIterations = 10000

// hash is the LEGACY single-pass HMAC (kept only to verify pre-B4-10
// records; they are upgraded to salted+iterated on next successful verify).
func (m *PINManager) hash(phone, pin string) string {
	mac := hmac.New(sha256.New, []byte(m.pepper))
	mac.Write([]byte("ussd-pin:" + phone + ":" + pin))
	return hex.EncodeToString(mac.Sum(nil))
}

// hashWithSalt computes iterated HMAC-SHA256: H0 = HMAC(pepper,
// "ussd-pin:"+salt+":"+pin), Hi = HMAC(pepper, Hi-1), i=1..pinIterations-1.
func (m *PINManager) hashWithSalt(salt, pin string) string {
	mac := hmac.New(sha256.New, []byte(m.pepper))
	mac.Write([]byte("ussd-pin:" + salt + ":" + pin))
	d := mac.Sum(nil)
	for i := 1; i < pinIterations; i++ {
		mac = hmac.New(sha256.New, []byte(m.pepper))
		mac.Write(d)
		d = mac.Sum(nil)
	}
	return hex.EncodeToString(d)
}

// newPINSalt returns a 128-bit random salt (hex).
func newPINSalt() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable for credential storage
	}
	return hex.EncodeToString(b)
}

// HasPIN reports whether the MSISDN has completed PIN setup.
func (m *PINManager) HasPIN(phone string) bool {
	rec, ok := m.store.Get(phone)
	return ok && rec.Hash != ""
}

// SetPIN stores a new hashed PIN (resets strikes/lock).
func (m *PINManager) SetPIN(phone, pin string) error {
	if !pinFormat.MatchString(pin) {
		return fmt.Errorf("PIN must be 4-6 digits")
	}
	salt := newPINSalt()
	m.store.Put(phone, PINRecord{Hash: m.hashWithSalt(salt, pin), Salt: salt})
	return nil
}

// Verify checks a PIN attempt, applying the strike/lock policy. On success
// strikes reset. On the 3rd failure the MSISDN locks and a lock event is
// published.
func (m *PINManager) Verify(phone, pin string) error {
	rec, ok := m.store.Get(phone)
	if !ok || rec.Hash == "" {
		return errors.New("no PIN set")
	}
	if rec.Locked {
		return errPINLocked
	}
	want := m.hashWithSalt(rec.Salt, pin)
	legacy := rec.Salt == ""
	if legacy {
		want = m.hash(phone, pin) // pre-B4-10 record
	}
	if hmac.Equal([]byte(rec.Hash), []byte(want)) {
		rec.Strikes = 0
		if legacy {
			// transparent upgrade to salted+iterated storage
			rec.Salt = newPINSalt()
			rec.Hash = m.hashWithSalt(rec.Salt, pin)
		}
		m.store.Put(phone, rec)
		return nil
	}
	rec.Strikes++
	if rec.Strikes >= maxPINStrikes {
		rec.Locked = true
		if m.events != nil {
			m.events.Publish("nrs.ussd.pin_lock.v1", map[string]any{
				"msisdn_hash": msisdnHash(phone), "strikes": rec.Strikes,
			})
		}
	}
	m.store.Put(phone, rec)
	return fmt.Errorf("incorrect PIN (%d/%d attempts)", rec.Strikes, maxPINStrikes)
}

// registerPINActions wires the PIN gate/setup/verify actions. The gate is
// registered per sensitive target ("pin.gate_tin_status") because the DSL
// success branch is static: the gate records the resume point and uses
// _next_override to detour unauthenticated sessions into the PIN flow.
func registerPINActions(actions map[string]ActionHandler, mgr *PINManager) {
	gate := func(resume string) ActionHandler {
		return func(sess *Session) error {
			if sess.Data["pin_ok"] == "1" {
				return nil // verified this session: proceed to the action
			}
			sess.Data["pin_resume"] = resume
			if mgr.HasPIN(sess.Phone) {
				sess.Data["_next_override"] = "pin_verify_input"
			} else {
				sess.Data["_next_override"] = "pin_setup_input"
			}
			return nil
		}
	}
	actions["pin.gate_tin_status"] = gate("onb_tin_status")

	actions["pin.setup_save"] = func(sess *Session) error {
		newPIN := sess.Data["pin_new"]
		confirm := sess.Data["pin_confirm"]
		delete(sess.Data, "pin_new")
		delete(sess.Data, "pin_confirm") // never retain raw PINs in session
		if newPIN == "" || newPIN != confirm {
			return errors.New("PINs did not match. Please redial and try again.")
		}
		if err := mgr.SetPIN(sess.Phone, newPIN); err != nil {
			return err
		}
		sess.Data["pin_ok"] = "1"
		sess.Data["_next_override"] = sess.Data["pin_resume"]
		return nil
	}

	actions["pin.verify"] = func(sess *Session) error {
		attempt := sess.Data["pin_attempt"]
		delete(sess.Data, "pin_attempt") // never retain raw PINs in session
		if err := mgr.Verify(sess.Phone, attempt); err != nil {
			return err
		}
		sess.Data["pin_ok"] = "1"
		sess.Data["_next_override"] = sess.Data["pin_resume"]
		return nil
	}
}
