package main

import (
	"fmt"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// seedFlat seeds n agents directly (Attach itself is the benchmark subject;
// seeding through it would be O(n^2) on the old full-scan implementation).
func seedFlat(b *testing.B, n int) (*Hierarchy, string) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	root, err := reg.Register(Agent{FullName: "root", Phone: "1"})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ag := Agent{ID: fmt.Sprintf("ag_%d", i), FullName: fmt.Sprintf("a%d", i), Phone: fmt.Sprintf("p%d", i), TenantID: root.TenantID, ParentID: root.ID, VettingStatus: "approved"}
		if err := st.Put("agents", ag.ID, ag); err != nil {
			b.Fatal(err)
		}
	}
	return NewHierarchy(reg), root.ID
}

func BenchmarkSubtree10000(b *testing.B) {
	h, rootID := seedFlat(b, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Subtree(rootID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAttachPair10000(b *testing.B) {
	h, rootID := seedFlat(b, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c1, _ := h.agents.Register(Agent{FullName: "c1", Phone: "c1"})
		b.StartTimer()
		if _, err := h.Attach(c1.ID, rootID); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_, _ = h.Attach(c1.ID, "")
		b.StartTimer()
	}
}

// seedTree seeds a depth-2 tree: root -> 100 mid agents -> 99 leaves each
// (9900 leaves). Subtree of a mid agent is ~100 docs; with the parent_id
// index it no longer scans all 10k agents to find them.
func seedTree(b *testing.B) (*Hierarchy, string) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	root, err := reg.Register(Agent{FullName: "root", Phone: "1"})
	if err != nil {
		b.Fatal(err)
	}
	midID := ""
	for i := 0; i < 100; i++ {
		mid := Agent{ID: fmt.Sprintf("ag_m%d", i), FullName: "m", Phone: "m", TenantID: root.TenantID, ParentID: root.ID, VettingStatus: "approved"}
		if err := st.Put("agents", mid.ID, mid); err != nil {
			b.Fatal(err)
		}
		if i == 50 {
			midID = mid.ID
		}
		for j := 0; j < 99; j++ {
			leaf := Agent{ID: fmt.Sprintf("ag_%d_%d", i, j), FullName: "l", Phone: "l", TenantID: root.TenantID, ParentID: mid.ID, VettingStatus: "approved"}
			if err := st.Put("agents", leaf.ID, leaf); err != nil {
				b.Fatal(err)
			}
		}
	}
	return NewHierarchy(reg), midID
}

func BenchmarkSubtreeMidOf10000(b *testing.B) {
	h, midID := seedTree(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sub, err := h.Subtree(midID)
		if err != nil {
			b.Fatal(err)
		}
		if len(sub) != 100 {
			b.Fatal(len(sub))
		}
	}
}
