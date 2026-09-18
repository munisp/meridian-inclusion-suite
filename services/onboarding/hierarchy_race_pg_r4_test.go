package main

// hierarchy_race_pg_r4_test.go — R4-9b (item 2): the cross-replica
// cross-attach race. Two Hierarchy instances over two separate Postgres
// connections simulate two replicas: the per-process mutex no longer
// serialises them, so the DB transaction (row locks via SELECT ... FOR
// UPDATE in id-sorted order + validation inside the same transaction) must
// guarantee exactly one of A→B / B→A succeeds and no 2-cycle is stored.
//
// Requires a real Postgres: MERIDIAN_TEST_PG_DSN. Skipped otherwise — the
// embedded-store race is covered by hierarchy_race_r4_test.go.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func pgHierarchyStores(t *testing.T) (*store.Store, *store.Store) {
	t.Helper()
	dsn := os.Getenv("MERIDIAN_TEST_PG_DSN")
	poolDSN := dsn
	if strings.Contains(poolDSN, "?") {
		poolDSN += "&pool_max_conns=2"
	} else {
		poolDSN += "?pool_max_conns=2"
	}
	if dsn == "" {
		t.Skip("MERIDIAN_TEST_PG_DSN unset: skipping live-Postgres R4-9b test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS meridian_docs`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	st1, err := store.OpenPostgres(poolDSN)
	if err != nil {
		t.Fatalf("OpenPostgres #1: %v", err)
	}
	st2, err := store.OpenPostgres(poolDSN)
	if err != nil {
		t.Fatalf("OpenPostgres #2: %v", err)
	}
	return st1, st2
}

func TestAttachConcurrentCrossReplicaNoCyclePG(t *testing.T) {
	for round := 0; round < 50; round++ {
		st1, st2 := pgHierarchyStores(t)
		// two replicas: separate connections, separate mutexes
		reg1, h1 := NewAgentRegistry(st1), (*Hierarchy)(nil)
		h1 = NewHierarchy(reg1)
		h2 := NewHierarchy(NewAgentRegistry(st2))

		a, err := reg1.Register(Agent{FullName: "A", Phone: "080a", TenantID: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := reg1.Register(Agent{FullName: "B", Phone: "080b", TenantID: "t1"})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var errAB, errBA error
		go func() { defer wg.Done(); _, errAB = h1.Attach(a.ID, b.ID) }()
		go func() { defer wg.Done(); _, errBA = h2.Attach(b.ID, a.ID) }()
		wg.Wait()

		if errAB == nil && errBA == nil {
			t.Fatalf("round %d: both cross-replica attaches succeeded — 2-cycle stored", round)
		}
		if errAB != nil && !errors.Is(errAB, ErrHierarchyCycle) {
			t.Fatalf("round %d: A->B failed with %v, want ErrHierarchyCycle", round, errAB)
		}
		if errBA != nil && !errors.Is(errBA, ErrHierarchyCycle) {
			t.Fatalf("round %d: B->A failed with %v, want ErrHierarchyCycle", round, errBA)
		}

		// the stored hierarchy must be acyclic from both nodes
		if _, err := h1.Depth(a.ID); err != nil {
			t.Fatalf("round %d: depth(a): %v", round, err)
		}
		if _, err := h1.Depth(b.ID); err != nil {
			t.Fatalf("round %d: depth(b): %v", round, err)
		}
	}
}
