package main

// R4-9b (item 1): the meridian_payments_levy_uniq partial UNIQUE index is
// the cross-replica enforcer of (tenant, tin, period, instalment-class)
// uniqueness. These tests run against a REAL Postgres (MERIDIAN_TEST_PG_DSN,
// e.g. postgres://postgres@localhost:55432/postgres); they are skipped when
// no DSN is set. CI/staging must run them once per change that touches the
// payments write path — the embedded-store tests cannot exercise the index.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// pgPackageDSN returns a DSN pointing at a per-package test database
// (created if absent) so packages running in parallel under `go test ./...`
// never share a meridian_docs table.
func pgPackageDSN(t *testing.T, dbName string) string {
	t.Helper()
	base := os.Getenv("MERIDIAN_TEST_PG_DSN")
	if base == "" {
		t.Skip("MERIDIAN_TEST_PG_DSN unset: skipping live-Postgres R4-9b test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+dbName); err != nil &&
		!strings.Contains(err.Error(), "42P04") { // duplicate_database
		t.Fatalf("create test database: %v", err)
	}
	// swap the database segment of the DSN (path between host and query)
	q := strings.Index(base, "?")
	head, tail := base, ""
	if q >= 0 {
		head, tail = base[:q], base[q:]
	}
	slash := strings.LastIndex(head, "/")
	return head[:slash+1] + dbName + tail
}

// pgLevyStore opens a fresh Postgres-backed store on the per-package test
// database and wipes the documents table so each test starts clean.
func pgLevyStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := pgPackageDSN(t, "meridian_test_psm")
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
	// wipe before OpenPostgres re-applies the (idempotent) DDL
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS meridian_docs`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	st, err := store.OpenPostgres(poolDSN)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	return st
}

// Two INDEPENDENT PaymentService instances over the same Postgres simulate
// two replicas: each has its own per-process mutex, so only the database
// unique index can prevent a double create.
func newPGReplica(t *testing.T, st *store.Store) *PaymentService {
	t.Helper()
	engine, err := LoadBandEngine()
	if err != nil {
		t.Fatal(err)
	}
	gates := &GateClient{file: filepath.Join(t.TempDir(), "gates.json")}
	if _, err := gates.Flip(presumptiveGateID, true); err != nil {
		t.Fatal(err)
	}
	lc := ledger.NewDevClient()
	return NewPaymentService(st, lc, NewPSSPHub(), engine, gates,
		NewCertificateService(st), events.NewInprocBus())
}

func TestR4PGLevyUniqueIndexExists(t *testing.T) {
	st := pgLevyStore(t)
	if !st.IsPostgres() {
		t.Fatal("expected postgres backend")
	}
	// OpenPostgres ran the DDL; the partial unique index must exist with the
	// exact duplicateLevyStatuses predicate set.
	dsn := pgPackageDSN(t, "meridian_test_psm")
	poolDSN := dsn
	if strings.Contains(poolDSN, "?") {
		poolDSN += "&pool_max_conns=2"
	} else {
		poolDSN += "?pool_max_conns=2"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var def string
	err = conn.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'meridian_payments_levy_uniq'`).Scan(&def)
	if err != nil {
		t.Fatalf("index meridian_payments_levy_uniq not found: %v", err)
	}
	for _, status := range []string{"intent", "pending_authorisation", "authorised",
		"captured_awaiting_post", "capture_in_flight", "captured", "settled", "disputed"} {
		if !strings.Contains(def, status) {
			t.Fatalf("index predicate missing status %q: %s", status, def)
		}
	}
	// idempotent: applying the DDL a second time must not error
	if _, err := store.OpenPostgres(poolDSN); err != nil {
		t.Fatalf("second OpenPostgres (DDL re-apply) failed: %v", err)
	}
}

// The multi-replica race the app-level mutex cannot close: two services
// (separate mutexes) create an intent for the same (tin, period, class) at
// the same time. Exactly ONE may succeed; every loser must get
// ErrDuplicateLevy (mapped from SQLSTATE 23505), never a second payment.
func TestR4PGConcurrentCrossReplicaDuplicateLevy(t *testing.T) {
	st1 := pgLevyStore(t) // same DSN -> same database
	poolDSN := pgPackageDSN(t, "meridian_test_psm")
	if strings.Contains(poolDSN, "?") {
		poolDSN += "&pool_max_conns=2"
	} else {
		poolDSN += "?pool_max_conns=2"
	}
	st2, err := store.OpenPostgres(poolDSN)
	if err != nil {
		t.Fatal(err)
	}
	replicas := []*PaymentService{newPGReplica(t, st1), newPGReplica(t, st2)}

	const attempts = 16
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := replicas[i%2].CreateIntent(IntentRequest{
				TINHash: "tinhash-r4-pg-race", State: "Lagos", TradeCategory: "retail",
				AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
				// no idempotency key: raw cross-channel creates
			})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	succeeded, duplicates, other := 0, 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDuplicateLevy):
			duplicates++
		default:
			other++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("exactly one create may win; got %d successes, %d duplicates, %d other",
			succeeded, duplicates, other)
	}
	if duplicates != attempts-1 {
		t.Fatalf("every loser must get ErrDuplicateLevy; got %d of %d", duplicates, attempts-1)
	}
	// and the store holds exactly one live payment for the levy
	var all []Payment
	if err := st1.List("payments", &all); err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, p := range all {
		if p.TINHash == "tinhash-r4-pg-race" && duplicateLevyStatuses[p.Status] {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("store holds %d live payments for the raced levy, want 1", live)
	}
}
