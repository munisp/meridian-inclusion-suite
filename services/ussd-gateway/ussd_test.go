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

// TestSessionResumeAfterDrop (audit fix #8): a session dropped mid-flow (new
// sessionId, same MSISDN) is offered a resume prompt and continues mid-menu.
func TestSessionResumeAfterDrop(t *testing.T) {
	graph, err := LoadMenuGraph()
	if err != nil {
		t.Fatal(err)
	}
	pub := &memPub{}
	eng := NewEngine(graph, RegisterActions(pub))
	st := NewInMemSessionStore(graph.SessionTTLSeconds)
	srv := &server{graph: graph, engine: eng, store: st}
	phone := "+2348099999999"

	// first dial: start onboarding and advance one step (enter a menu choice)
	r1 := srv.processInput("sess-A", phone, "")
	if !strings.HasPrefix(r1, "CON") {
		t.Fatalf("expected CON, got %q", r1)
	}
	r2 := srv.processInput("sess-A", phone, "1") // pick first option (onboarding)
	if !strings.HasPrefix(r2, "CON") {
		t.Fatalf("expected mid-flow CON, got %q", r2)
	}
	// call drops; redial with a NEW session id, same MSISDN
	r3 := srv.processInput("sess-B", phone, "")
	if !strings.Contains(r3, "Continue last transaction") {
		t.Fatalf("expected resume prompt, got %q", r3)
	}
	// choose 1 = continue: must land back on the same menu (not the start menu)
	r4 := srv.processInput("sess-B", phone, "1")
	if !strings.HasPrefix(r4, "CON Resuming.") {
		t.Fatalf("expected resume, got %q", r4)
	}
	if strings.Contains(r4, "Welcome") && !strings.Contains(r4, "Resuming") {
		t.Fatalf("resume must not restart the flow: %q", r4)
	}
	// a second redial opting to start over gets the start menu
	r5 := srv.processInput("sess-C", phone, "")
	if !strings.Contains(r5, "Continue last transaction") {
		t.Fatalf("expected resume prompt for live session, got %q", r5)
	}
	r6 := srv.processInput("sess-C", phone, "2")
	if strings.Contains(r6, "Resuming") {
		t.Fatalf("start-over must not resume: %q", r6)
	}
}

// TestMultilingualPacks (I15): all language packs are complete and a session
// that switches language renders translated menus.
func TestMultilingualPacks(t *testing.T) {
	for lang, b := range bundles {
		for _, k := range bundleKeys {
			if b[k] == "" {
				t.Fatalf("language %s missing key %s", lang, k)
			}
		}
	}
	graph, _ := LoadMenuGraph()
	pub := &memPub{}
	eng := NewEngine(graph, RegisterActions(pub))
	st := NewInMemSessionStore(graph.SessionTTLSeconds)
	srv := &server{graph: graph, engine: eng, store: st}
	phone := "+2348077777777"
	r := srv.processInput("l1", phone, "")
	if !strings.Contains(r, "Welcome to NRS") {
		t.Fatalf("default must be English, got %q", r)
	}
	r = srv.processInput("l1", phone, "#ha")
	if !strings.Contains(r, "Barka da zuwa") {
		t.Fatalf("expected Hausa home after #ha, got %q", r)
	}
	r = srv.processInput("l1", phone, "#pcm")
	if !strings.Contains(r, "Pidgin") || strings.Contains(r, "Barka") {
		t.Fatalf("expected Pidgin after #pcm, got %q", r)
	}
	// unknown key falls back to English template
	if got := T("yo", "nonexistent_key", "fallback-text"); got != "fallback-text" {
		t.Fatalf("fallback broken: %q", got)
	}
}
