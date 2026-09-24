package store

// listwhere_test.go — coverage for the perf additions: secondary indexes
// (RegisterIndex/ListWhere), single-pass List semantics, and opt-in
// debounced STORE_FILE persistence.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type lwDoc struct {
	ID     string `json:"id"`
	Field  string `json:"field"`
	Other  string `json:"other,omitempty"`
	Number int    `json:"number"`
}

func seedLW(t *testing.T, st *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d := lwDoc{ID: fmt.Sprintf("d_%d", i), Field: fmt.Sprintf("f_%d", i%10), Number: i}
		if err := st.Put("docs", d.ID, d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListWhereRegisteredIndex(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	st.RegisterIndex("docs", "field")
	seedLW(t, st, 100)
	var out []lwDoc
	if err := st.ListWhere("docs", "field", "f_3", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 10 {
		t.Fatalf("expected 10 matches, got %d", len(out))
	}
	for _, d := range out {
		if d.Field != "f_3" {
			t.Fatalf("wrong doc: %+v", d)
		}
	}
	// no match -> empty, non-stale slice
	if err := st.ListWhere("docs", "field", "nope", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 matches, got %+v", out)
	}
}

func TestListWhereUnregisteredFallback(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	seedLW(t, st, 50)
	var out []lwDoc
	if err := st.ListWhere("docs", "field", "f_7", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("expected 5 matches via fallback scan, got %d", len(out))
	}
}

func TestIndexTracksWritesAndDeletes(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	st.RegisterIndex("docs", "field")
	seedLW(t, st, 10)
	// update moves the doc between value buckets
	if err := st.Put("docs", "d_0", lwDoc{ID: "d_0", Field: "moved"}); err != nil {
		t.Fatal(err)
	}
	var out []lwDoc
	if err := st.ListWhere("docs", "field", "f_0", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("stale index after update: %+v", out)
	}
	if err := st.ListWhere("docs", "field", "moved", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "d_0" {
		t.Fatalf("update not indexed: %+v", out)
	}
	// delete de-indexes
	if _, err := st.Delete("docs", "d_0"); err != nil {
		t.Fatal(err)
	}
	if err := st.ListWhere("docs", "field", "moved", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("stale index after delete: %+v", out)
	}
	// PutIfAbsent indexes too
	if won, err := st.PutIfAbsent("docs", "d_99", lwDoc{ID: "d_99", Field: "moved"}); err != nil || !won {
		t.Fatalf("putifabsent: won=%v err=%v", won, err)
	}
	if err := st.ListWhere("docs", "field", "moved", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "d_99" {
		t.Fatalf("putifabsent not indexed: %+v", out)
	}
}

func TestRegisterIndexBackfillsExistingDocs(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	seedLW(t, st, 20) // written BEFORE registration
	st.RegisterIndex("docs", "field")
	var out []lwDoc
	if err := st.ListWhere("docs", "field", "f_1", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("backfill missed docs: %+v", out)
	}
	// idempotent re-registration must not duplicate entries
	st.RegisterIndex("docs", "field")
	if err := st.ListWhere("docs", "field", "f_1", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("re-registration duplicated entries: %+v", out)
	}
}

// Re-listing into a non-nil slice must not leave stale elements (regression
// guard for the single-pass List rewrite).
func TestListResetsTargetSlice(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	seedLW(t, st, 3)
	out := []lwDoc{{ID: "stale"}}
	if err := st.List("docs", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3, got %+v", out)
	}
	if _, err := st.Delete("docs", "d_0"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete("docs", "d_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete("docs", "d_2"); err != nil {
		t.Fatal(err)
	}
	if err := st.List("docs", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("stale elements after delete: %+v", out)
	}
}

func TestDebouncedPersistenceFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.EnableDebouncedPersistence()
	if err := st.Put("docs", "a", lwDoc{ID: "a", Field: "x"}); err != nil {
		t.Fatal(err)
	}
	// Flush forces the pending write synchronously.
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Fatalf("flush did not write file: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var d lwDoc
	ok, err := st2.Get("docs", "a", &d)
	if err != nil || !ok {
		t.Fatalf("reopen lost doc: ok=%v err=%v", ok, err)
	}
	// Without Flush the async timer also lands the write.
	st.EnableDebouncedPersistence()
	if err := st.Put("docs", "b", lwDoc{ID: "b", Field: "y"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st3, err := Open(path)
		if err == nil {
			var d2 lwDoc
			if ok, _ := st3.Get("docs", "b", &d2); ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("debounced write never reached disk")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Synchronous default: without EnableDebouncedPersistence a Put is durable
// immediately (restart-safety contract pinned by the nip tests).
func TestSyncPersistenceRemainsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("docs", "a", lwDoc{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var d lwDoc
	ok, err := st2.Get("docs", "a", &d)
	if err != nil || !ok {
		t.Fatalf("sync write not immediately durable: ok=%v err=%v", ok, err)
	}
}
