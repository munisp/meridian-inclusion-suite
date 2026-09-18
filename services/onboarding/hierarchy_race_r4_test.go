package main

// hierarchy_race_r4_test.go — R4-S3#3 regression: concurrent re-parents
// (A->B and B->A) must never store a 2-cycle, and Subtree must terminate
// even in the presence of one. Run with -race.

import (
	"errors"
	"sync"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// TestAttachConcurrentNoCycle hammers Attach from both directions at once.
// Before the fix the cycle check and the Put were not atomic, so two
// concurrent attaches could each pass the check and store children[A]=[B],
// children[B]=[A]. After the fix exactly one direction can win per pair and
// no cycle is ever stored.
func TestAttachConcurrentNoCycle(t *testing.T) {
	for round := 0; round < 200; round++ {
		st, _ := store.Open("")
		reg := NewAgentRegistry(st)
		h := NewHierarchy(reg)
		a := seedAgent(t, reg, "a", "t1")
		b := seedAgent(t, reg, "b", "t1")

		var wg sync.WaitGroup
		wg.Add(2)
		var errAB, errBA error
		go func() { defer wg.Done(); _, errAB = h.Attach(a.ID, b.ID) }()
		go func() { defer wg.Done(); _, errBA = h.Attach(b.ID, a.ID) }()
		wg.Wait()

		// At most one direction may succeed.
		if errAB == nil && errBA == nil {
			t.Fatalf("round %d: both A->B and B->A succeeded — 2-cycle stored", round)
		}
		// A failure, when one occurs, must be the cycle rejection.
		if errAB != nil && !errors.Is(errAB, ErrHierarchyCycle) {
			t.Fatalf("round %d: A->B failed with %v (want ErrHierarchyCycle)", round, errAB)
		}
		if errBA != nil && !errors.Is(errBA, ErrHierarchyCycle) {
			t.Fatalf("round %d: B->A failed with %v (want ErrHierarchyCycle)", round, errBA)
		}
		// Whatever was stored must be a valid acyclic hierarchy: Depth and
		// Subtree must terminate and stay within bounds.
		for _, id := range []string{a.ID, b.ID} {
			if _, err := h.Depth(id); err != nil {
				t.Fatalf("round %d: Depth(%s) after race: %v", round, id, err)
			}
			sub, err := h.Subtree(id)
			if err != nil {
				t.Fatalf("round %d: Subtree(%s) after race: %v", round, id, err)
			}
			if len(sub) > 2 {
				t.Fatalf("round %d: Subtree(%s) returned %d agents (cycle)", round, id, len(sub))
			}
		}
	}
}

// TestSubtreeTerminatesOnStoredCycle is the defense-in-depth half: even if
// a cycle is present in the store (written directly, bypassing Attach),
// Subtree's visited set makes the BFS terminate with a bounded result.
// Before the fix this looped forever.
func TestSubtreeTerminatesOnStoredCycle(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	h := NewHierarchy(reg)
	a := seedAgent(t, reg, "a", "t1")
	b := seedAgent(t, reg, "b", "t1")
	// Plant the 2-cycle directly in the store, bypassing Attach's checks.
	a.ParentID = b.ID
	if err := st.Put("agents", a.ID, a); err != nil {
		t.Fatal(err)
	}
	b.ParentID = a.ID
	if err := st.Put("agents", b.ID, b); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		sub, _ := h.Subtree(a.ID)
		done <- len(sub)
	}()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("Subtree over stored cycle returned %d agents, want 2", n)
		}
	default:
		// give the BFS a moment; an unbounded loop never sends
	}
	// synchronous guard: if the BFS ever regresses to unbounded this test
	// hangs and is caught by `go test -timeout`.
	sub, err := h.Subtree(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 2 {
		t.Fatalf("Subtree over stored cycle = %d agents, want exactly 2", len(sub))
	}
}
