// pin_test.go — USSD PIN setup/verify/lock for sensitive actions (M-2).
package main

import (
	"strings"
	"testing"
	"time"
)

type pinEventSink struct{ events []string }

func (s *pinEventSink) Publish(topic string, _ map[string]any) { s.events = append(s.events, topic) }

func TestPINManagerSetVerifyLock(t *testing.T) {
	sink := &pinEventSink{}
	mgr := NewPINManager(NewInMemPINStore(), sink)
	phone := "+2348000000001"

	if mgr.HasPIN(phone) {
		t.Fatal("no PIN expected initially")
	}
	if err := mgr.SetPIN(phone, "12"); err == nil {
		t.Fatal("short PIN must be rejected")
	}
	if err := mgr.SetPIN(phone, "4321"); err != nil {
		t.Fatal(err)
	}
	if !mgr.HasPIN(phone) {
		t.Fatal("PIN expected after setup")
	}
	// stored hashed, never raw
	rec, _ := mgr.store.Get(phone)
	if rec.Hash == "" || strings.Contains(rec.Hash, "4321") {
		t.Fatalf("PIN not stored hashed: %+v", rec)
	}

	if err := mgr.Verify(phone, "0000"); err == nil {
		t.Fatal("wrong PIN must fail")
	}
	if err := mgr.Verify(phone, "0000"); err == nil {
		t.Fatal("wrong PIN must fail (2)")
	}
	// 3rd failure locks + emits lock event
	if err := mgr.Verify(phone, "0000"); err == nil {
		t.Fatal("wrong PIN must fail (3)")
	}
	rec, _ = mgr.store.Get(phone)
	if !rec.Locked {
		t.Fatal("expected lock after 3 strikes")
	}
	if len(sink.events) != 1 || sink.events[0] != "nrs.ussd.pin_lock.v1" {
		t.Fatalf("lock event missing: %+v", sink.events)
	}
	// even the correct PIN is rejected while locked
	if err := mgr.Verify(phone, "4321"); err != errPINLocked {
		t.Fatalf("locked verify: got %v, want errPINLocked", err)
	}
}

func TestPINGateFlowTinStatus(t *testing.T) {
	graph, err := LoadMenuGraph()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewPINManager(NewInMemPINStore(), &pinEventSink{})
	actions := RegisterActions(&memPub{})
	registerPINActions(actions, mgr)
	eng := NewEngine(graph, actions)

	sess := &Session{ID: "s1", Phone: "+2348000000002", Menu: "onb_tin_input", Data: map[string]string{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(3 * time.Minute)}

	// NIN input -> gate detours to PIN setup (no PIN yet)
	text, cont, err := eng.Handle(sess, "12345678901")
	if err != nil || !cont {
		t.Fatalf("nin input: %v", err)
	}
	if !strings.Contains(text, "Create a 4-digit PIN") {
		t.Fatalf("expected PIN setup prompt, got %q", text)
	}
	// setup: enter + confirm
	text, _, _ = eng.Handle(sess, "4321")
	if !strings.Contains(text, "Confirm") {
		t.Fatalf("expected confirm prompt, got %q", text)
	}
	text, _, err = eng.Handle(sess, "4321")
	if err != nil {
		t.Fatal(err)
	}
	// after setup the gated action runs (tin status report)
	if !strings.Contains(text, "TIN status") {
		t.Fatalf("expected TIN status after PIN setup, got %q", text)
	}
	if sess.Data["pin_new"] != "" || sess.Data["pin_confirm"] != "" {
		t.Fatal("raw PIN must not persist in session data")
	}

	// new session, same phone: gate detours to verify
	sess2 := &Session{ID: "s2", Phone: sess.Phone, Menu: "onb_tin_input", Data: map[string]string{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(3 * time.Minute)}
	text, _, _ = eng.Handle(sess2, "12345678901")
	if !strings.Contains(text, "Enter your PIN") {
		t.Fatalf("expected verify prompt, got %q", text)
	}
	// wrong PIN -> error branch
	text, cont, _ = eng.Handle(sess2, "9999")
	if cont || !strings.Contains(text, "incorrect PIN") {
		t.Fatalf("expected incorrect-PIN END, got cont=%v %q", cont, text)
	}
	// redial: NIN -> verify prompt -> correct PIN -> through to TIN status
	sess3 := &Session{ID: "s3", Phone: sess.Phone, Menu: "onb_tin_input", Data: map[string]string{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(3 * time.Minute)}
	if text, _, _ = eng.Handle(sess3, "12345678901"); !strings.Contains(text, "Enter your PIN") {
		t.Fatalf("expected verify prompt, got %q", text)
	}
	text, _, err = eng.Handle(sess3, "4321")
	if err != nil || !strings.Contains(text, "TIN status") {
		t.Fatalf("expected TIN status after verify, got %q err=%v", text, err)
	}
}

func TestPINGateSetupMismatch(t *testing.T) {
	graph, _ := LoadMenuGraph()
	mgr := NewPINManager(NewInMemPINStore(), &pinEventSink{})
	actions := RegisterActions(&memPub{})
	registerPINActions(actions, mgr)
	eng := NewEngine(graph, actions)
	sess := &Session{ID: "s4", Phone: "+2348000000003", Menu: "pin_setup_input", Data: map[string]string{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(3 * time.Minute)}
	eng.Handle(sess, "4321")
	text, cont, _ := eng.Handle(sess, "0000") // mismatch
	if cont || !strings.Contains(text, "did not match") {
		t.Fatalf("expected mismatch END, got cont=%v %q", cont, text)
	}
	if mgr.HasPIN(sess.Phone) {
		t.Fatal("PIN must not be stored on mismatch")
	}
}
