package main

// session_lock_r4_test.go — R4-S3#16 regression: the global process mutex
// was held across the whole engine.Handle call (including ~15s upstream
// round-trips), so one slow session head-of-line blocked EVERY concurrent
// USSD session. Locks are now keyed per MSISDN.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestR4SlowSessionDoesNotBlockOthers: while phone A is parked inside a slow
// action, phone B's session must complete promptly. On main (global mutex)
// B blocks behind A and this test times out.
func TestR4SlowSessionDoesNotBlockOthers(t *testing.T) {
	graph := &MenuGraph{
		ServiceCode: "*737#", Start: "go", SessionTTLSeconds: 180,
		Menus: map[string]Menu{
			"go":   {Type: "action", Action: "maybe.block", Next: "done"},
			"done": {Type: "end", Text: "bye"},
		},
	}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	actions := map[string]ActionHandler{
		"maybe.block": func(sess *Session) error {
			if sess.Phone == "+234111" {
				entered <- struct{}{}
				<-release // simulate a hung upstream call
			}
			return nil
		},
	}
	eng := NewEngine(graph, actions)
	st := NewInMemSessionStore(graph.SessionTTLSeconds)
	srv := &server{graph: graph, engine: eng, store: st}

	// Phone A enters the slow action and stays there.
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		srv.processInput("sess-A", "+234111", "")
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("phone A never entered the action")
	}

	// Phone B must complete even though A is still blocked.
	doneB := make(chan string, 1)
	go func() { doneB <- srv.processInput("sess-B", "+234222", "") }()
	select {
	case resp := <-doneB:
		if !strings.HasPrefix(resp, "END") {
			t.Fatalf("phone B response = %q, want END", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("phone B blocked behind phone A's slow session (global lock)")
	}
	close(release)
	wgA.Wait()
}

// TestR4SamePhoneStillSerialized: concurrent inputs for the SAME MSISDN
// (incl. a redial with a new session id) remain serialized — the resume
// check-then-act stays race-free. Run with -race.
func TestR4SamePhoneStillSerialized(t *testing.T) {
	graph := &MenuGraph{
		ServiceCode: "*737#", Start: "go", SessionTTLSeconds: 180,
		Menus: map[string]Menu{
			"go":   {Type: "action", Action: "count", Next: "done"},
			"done": {Type: "end", Text: "bye"},
		},
	}
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	actions := map[string]ActionHandler{
		"count": func(sess *Session) error {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond) // hold the critical section
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		},
	}
	eng := NewEngine(graph, actions)
	st := NewInMemSessionStore(graph.SessionTTLSeconds)
	srv := &server{graph: graph, engine: eng, store: st}
	phone := "+234333"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv.processInput("sess-"+string(rune('a'+i)), phone, "")
		}(i)
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Fatalf("same-phone sessions ran concurrently: max in-flight = %d", maxInFlight)
	}
}
