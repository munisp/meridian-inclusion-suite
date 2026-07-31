package crdtx

import (
	"testing"
	"time"
)

// TestCRDTDuplicateAndOutOfOrder (I18): duplicate deliveries are no-ops and
// out-of-order delivery converges to the same state.
func TestCRDTDuplicateAndOutOfOrder(t *testing.T) {
	clkA := NewClock("agent-A")
	clkB := NewClock("agent-B")

	add1 := Op{ID: "op-1", Kind: "add", Element: "ref-1", Tag: clkA.Now(), Payload: `{"nin":"x"}`}
	time.Sleep(time.Millisecond)
	add2 := Op{ID: "op-2", Kind: "add", Element: "ref-2", Tag: clkB.Now()}
	rm1 := Op{ID: "op-3", Kind: "remove", Element: "ref-1", Tag: add1.Tag}

	// server receives: duplicate add1, out-of-order remove-before-add replay,
	// then the full set again in reverse order.
	server := NewORSet()
	seq := [][]Op{
		{add1, add1},            // duplicate delivery
		{rm1, add1, add2},       // out-of-order: remove arrives before its add replay
		{add2, add1, rm1, add1}, // full replay in reverse
	}
	for _, batch := range seq {
		server.Merge(batch)
	}
	if server.Contains("ref-1") {
		t.Fatal("ref-1 was removed (remove observed its only tag): must stay removed")
	}
	if !server.Contains("ref-2") {
		t.Fatal("ref-2 must be live")
	}
	// convergence: a second replica applying the same ops in a different
	// order reaches the identical live set.
	replica := NewORSet()
	replica.Merge([]Op{rm1, add2, add1, add1})
	got, want := replica.Elements(), server.Elements()
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("replicas diverged: %v vs %v", got, want)
	}
	// concurrent add after remove is NOT resurrected (only observed tags die)
	concurrent := NewORSet()
	concurrent.Merge([]Op{add1, rm1})
	add1b := Op{ID: "op-4", Kind: "add", Element: "ref-1", Tag: clkA.Now()}
	concurrent.Merge([]Op{add1b})
	if !concurrent.Contains("ref-1") {
		t.Fatal("add-wins: a concurrent (unobserved) add must survive a remove")
	}
}

func TestHLCOrdering(t *testing.T) {
	clk := NewClock("n1")
	a := clk.Now()
	b := clk.Now()
	if !a.Less(b) {
		t.Fatal("HLC must be monotonic per node")
	}
	remote := HLC{Wall: time.Now().Add(time.Hour).UnixNano(), Count: 0, Node: "n2"}
	c := clk.Update(remote)
	if !remote.Less(c) && !(remote == c) {
		t.Fatal("merged HLC must be >= remote")
	}
}
