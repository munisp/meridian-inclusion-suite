package main

import (
	"strings"
	"testing"
	"time"
)

type memPub struct{ events []string }

func (m *memPub) Publish(topic string, data map[string]any) { m.events = append(m.events, topic) }

func newTestEngine(t *testing.T) (*Engine, SessionStore, *memPub) {
	t.Helper()
	graph, err := LoadMenuGraph()
	if err != nil {
		t.Fatal(err)
	}
	pub := &memPub{}
	eng := NewEngine(graph, RegisterActions(pub))
	return eng, NewInMemSessionStore(graph.SessionTTLSeconds), pub
}

func runFlow(t *testing.T, eng *Engine, store SessionStore, inputs ...string) (string, *Session) {
	t.Helper()
	sess := &Session{ID: "t1", Phone: "+2348011111111", Data: map[string]string{}, CreatedAt: time.Now()}
	store.Put(sess)
	text, cont, err := eng.Start(sess)
	if err != nil || !cont {
		t.Fatalf("start: %v cont=%v", err, cont)
	}
	store.Put(sess)
	last := text
	for _, in := range inputs {
		last, cont, err = eng.Handle(sess, in)
		if err != nil {
			t.Fatalf("handle %q: %v", in, err)
		}
		if !cont {
			break
		}
		store.Put(sess)
	}
	return last, sess
}

func TestOnboardingFlow(t *testing.T) {
	eng, store, pub := newTestEngine(t)
	out, sess := runFlow(t, eng, store, "1", "12345678901", "Adaeze Okafor", "1")
	if !strings.Contains(out, "Registration successful") {
		t.Fatalf("unexpected final screen: %s", out)
	}
	if !strings.Contains(out, "TIN: 2") {
		t.Fatalf("expected TIN in output: %s", out)
	}
	if sess.Data["tin"] == "" || sess.Data["state"] != "Lagos" {
		t.Fatalf("session data: %+v", sess.Data)
	}
	found := false
	for _, e := range pub.events {
		if e == "nrs.onb.ussd.v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected nrs.onb.ussd.v1 event")
	}
}

func TestOnboardingInvalidNIN(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	sess := &Session{ID: "t2", Phone: "+2348011111111", Data: map[string]string{}, CreatedAt: time.Now()}
	store.Put(sess)
	_, _, _ = eng.Start(sess)
	out, cont, err := eng.Handle(sess, "1")
	if err != nil || !cont {
		t.Fatal(err)
	}
	out, cont, err = eng.Handle(sess, "123") // invalid NIN
	if err != nil {
		t.Fatal(err)
	}
	if !cont || !strings.Contains(out, "Invalid NIN") {
		t.Fatalf("expected invalid-NIN re-prompt, got %q cont=%v", out, cont)
	}
}

func TestPresumptivePayFlow(t *testing.T) {
	eng, store, pub := newTestEngine(t)
	// 3 pay -> Lagos -> retail -> N1m-N5m -> pay now
	out, sess := runFlow(t, eng, store, "3", "1", "5", "3")
	if !strings.Contains(out, "Band: small") || !strings.Contains(out, "Annual levy: N") {
		t.Fatalf("expected band+levy screen, got: %s", out)
	}
	out2, _, err := eng.Handle(sess, "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "Certificate: PSM-") || !strings.Contains(out2, "SMS") {
		t.Fatalf("expected certificate serial screen, got: %s", out2)
	}
	found := false
	for _, e := range pub.events {
		if e == "nrs.psm.ussd.v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected nrs.psm.ussd.v1 event")
	}
}

func TestPresumptiveExempt(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	// turnover option 1 = up to N800k -> exempt end screen
	out, _ := runFlow(t, eng, store, "3", "2", "1", "1")
	if !strings.Contains(out, "EXEMPT") && !strings.Contains(out, "exempt") {
		t.Fatalf("expected exemption screen, got: %s", out)
	}
}

func TestSessionTTL(t *testing.T) {
	store := NewInMemSessionStore(1) // 1s TTL
	s := &Session{ID: "ttl", Phone: "x", Data: map[string]string{}, CreatedAt: time.Now()}
	s.ExpiresAt = time.Now().Add(10 * time.Millisecond)
	store.m[s.ID] = s
	time.Sleep(20 * time.Millisecond)
	if _, ok := store.Get("ttl"); ok {
		t.Fatal("session should have expired")
	}
}

func TestTINStatusFlow(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	out, _ := runFlow(t, eng, store, "2", "12345678901")
	if !strings.Contains(out, "TIN: 2") || !strings.Contains(out, "provisioned") {
		t.Fatalf("unexpected tin status screen: %s", out)
	}
	if !strings.Contains(out, "123*****901") {
		t.Fatalf("expected masked NIN, got: %s", out)
	}
}
