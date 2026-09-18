package main

// hierarchy_subtreeheight_r4_test.go — R4-S3#3 residual regression:
// subtreeHeight must terminate on a PRE-EXISTING stored cycle (legacy data
// written before the Attach mutex existed). Before this fix the recursion
// had no visited/path guard and overflowed the stack on the next Attach
// under the corrupted subtree.

import (
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// TestSubtreeHeightTerminatesOnStoredCycle plants a 2-cycle directly in the
// store (bypassing Attach) below a legit child, then calls subtreeHeight on
// the corrupted subtree listing: it must return a bounded height, not hang
// or panic.
func TestSubtreeHeightTerminatesOnStoredCycle(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	h := NewHierarchy(reg)
	c := seedAgent(t, reg, "c", "t1")
	a := seedAgent(t, reg, "a", "t1")
	b := seedAgent(t, reg, "b", "t1")
	// Plant the legacy 3-cycle c -> a -> b -> c directly (bypassing Attach),
	// so the cycle is reachable from c's own subtree listing.
	a.ParentID = c.ID
	if err := st.Put("agents", a.ID, a); err != nil {
		t.Fatal(err)
	}
	b.ParentID = a.ID
	if err := st.Put("agents", b.ID, b); err != nil {
		t.Fatal(err)
	}
	c.ParentID = b.ID
	if err := st.Put("agents", c.ID, c); err != nil {
		t.Fatal(err)
	}

	sub, err := h.Subtree(c.ID)
	if err != nil {
		t.Fatalf("Subtree on stored cycle: %v", err)
	}
	if len(sub) != 3 {
		t.Fatalf("Subtree returned %d agents, want 3 unique", len(sub))
	}

	type result struct{ height int }
	done := make(chan result, 1)
	go func() { done <- result{subtreeHeight(c.ID, sub)} }()
	select {
	case r := <-done:
		if r.height < 0 || r.height > len(sub) {
			t.Fatalf("subtreeHeight returned %d, want a bounded value in [0,%d]", r.height, len(sub))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subtreeHeight did not terminate on a stored cycle")
	}
}

// TestAttachUnderStoredCycleTerminates drives the full Attach path against
// legacy corrupted state: the next Attach under a subtree containing a
// stored cycle must return (with an error or a bounded result), never hang
// or crash the process.
func TestAttachUnderStoredCycleTerminates(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	h := NewHierarchy(reg)
	c := seedAgent(t, reg, "c", "t1")
	a := seedAgent(t, reg, "a", "t1")
	b := seedAgent(t, reg, "b", "t1")
	p := seedAgent(t, reg, "p", "t1")
	// Plant the stored 3-cycle c -> a -> b -> c (bypassing Attach) so the
	// recursion inside Attach's depth check must walk corrupted data.
	a.ParentID = c.ID
	if err := st.Put("agents", a.ID, a); err != nil {
		t.Fatal(err)
	}
	b.ParentID = a.ID
	if err := st.Put("agents", b.ID, b); err != nil {
		t.Fatal(err)
	}
	c.ParentID = b.ID
	if err := st.Put("agents", c.ID, c); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := h.Attach(c.ID, p.ID); done <- err }()
	select {
	case err := <-done:
		t.Logf("Attach under stored cycle returned (bounded): err=%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not terminate with a stored cycle in the subtree")
	}
}
