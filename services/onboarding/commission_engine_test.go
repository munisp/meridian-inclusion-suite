package main

// commission_engine_test.go — I6: rule-pack commission computation (hand-
// checked kobo math), ledger hold->post ordering, payload-bound idempotent
// replay, and fail-closed pack-version gating.

import (
	"errors"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func newCommissionEngine(t *testing.T) (*CommissionEngine, *AgentRegistry, *ledger.DevClient) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewAgentRegistry(st)
	lc := ledger.NewDevClient()
	eng, err := LoadCommissionEngine(st, NewHierarchy(reg), lc)
	if err != nil {
		t.Fatal(err)
	}
	return eng, reg, lc
}

// TestCommissionCalcHandChecked pins the pack math: 250bps on ₦100,000.00
// (10,000,000 kobo) = ₦2,500.00 (250,000 kobo) exactly; upline levels at
// 100bps = ₦1,000.00 and 50bps = ₦500.00. Integer kobo math only.
func TestCommissionCalcHandChecked(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	root := seedAgent(t, eng.hierarchy.agents, "root", "t1")
	mid := seedAgent(t, reg, "mid", "t1")
	leaf := seedAgent(t, reg, "leaf", "t1")
	mustAttach(t, eng.hierarchy, mid.ID, root.ID)
	mustAttach(t, eng.hierarchy, leaf.ID, mid.ID)

	const n100k = uint64(10_000_000) // ₦100,000.00 in kobo
	recs, err := eng.Accrue(leaf.ID, "txn-1", n100k)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("want 3 level records, got %d", len(recs))
	}
	want := map[int]uint64{1: 250_000, 2: 100_000, 3: 50_000} // exact kobo
	byLevel := map[int]CommissionRecord{}
	for _, r := range recs {
		byLevel[r.Level] = r
		if r.RulePackVersion != "rp-commissions-ng@1.0.0" {
			t.Fatalf("pack tag: %q", r.RulePackVersion)
		}
	}
	for lvl, amt := range want {
		if byLevel[lvl].AmountKobo != amt {
			t.Fatalf("L%d: got %d kobo, want %d", lvl, byLevel[lvl].AmountKobo, amt)
		}
		if byLevel[lvl].RateBPS != map[int]uint64{1: 250, 2: 100, 3: 50}[lvl] {
			t.Fatalf("L%d bps: %d", lvl, byLevel[lvl].RateBPS)
		}
	}
	// computeKobo sanity: pure integer math.
	if got := computeKobo(n100k, 250); got != 250_000 {
		t.Fatalf("computeKobo: got %d, want 250000", got)
	}
	// Ledger: each earner's commission payable account holds exactly the
	// posted amount; pool was debited the total.
	for lvl, r := range byLevel {
		bal, err := lc.Balance(r.AccountID)
		if err != nil || bal.CreditsPosted != want[lvl] {
			t.Fatalf("L%d balance: %+v err=%v", lvl, bal, err)
		}
	}
	pool, err := lc.Balance(ledger.AccountID(nsCommissionsPool, 1))
	if err != nil || pool.DebitsPosted != 400_000 {
		t.Fatalf("pool: %+v err=%v", pool, err)
	}
}

// TestCommissionHoldThenPostOrdering proves the funds flow is a hold -> post
// saga: the pending hold exists with code 6, the post consumed it under a
// deterministic id, and the durable record was written only after the post
// (post -> mark ordering).
func TestCommissionHoldThenPostOrdering(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	ag := seedAgent(t, reg, "solo", "t1")
	recs, err := eng.Accrue(ag.ID, "txn-ord", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record (no upline), got %d", len(recs))
	}
	rec := recs[0]
	hold, err := lc.LookupTransfer(rec.HoldTransferID)
	if err != nil {
		t.Fatalf("hold lookup: %v", err)
	}
	if hold.Code != ledger.CodeHold || hold.Pending {
		t.Fatalf("hold must be a consumed code-6 pending transfer: %+v", hold)
	}
	post, err := lc.LookupTransfer(rec.PostTransferID)
	if err != nil {
		t.Fatalf("post lookup: %v", err)
	}
	if post.Pending || post.Amount != rec.AmountKobo ||
		post.CreditAccountID != commissionAccountID(ag.ID) {
		t.Fatalf("post mismatch: %+v vs rec %+v", post, rec)
	}
	// The durable record is only ever written after the post landed; verify
	// it is terminal and bound to the post id.
	if rec.Status != "posted" || rec.PostTransferID == "" || rec.ExpiresAt == "" {
		t.Fatalf("record not in posted/terminal form: %+v", rec)
	}
}

// TestCommissionIdempotentReplay proves durable idempotency: a replay with
// the same payload returns the posted record WITHOUT moving money again
// (replay-only-if-posted), and a replay with a different payload under the
// same reference is a conflict.
func TestCommissionIdempotentReplay(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	ag := seedAgent(t, reg, "solo", "t1")

	recs1, err := eng.Accrue(ag.ID, "txn-rep", 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	bal1, _ := lc.Balance(commissionAccountID(ag.ID))

	// Same payload replay: no new ledger movement, same record returned.
	recs2, err := eng.Accrue(ag.ID, "txn-rep", 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if recs2[0].PostTransferID != recs1[0].PostTransferID || recs2[0].PayloadHash != recs1[0].PayloadHash {
		t.Fatalf("replay must return the posted record: %+v vs %+v", recs2[0], recs1[0])
	}
	bal2, _ := lc.Balance(commissionAccountID(ag.ID))
	if bal2.CreditsPosted != bal1.CreditsPosted {
		t.Fatalf("replay double-posted: %d -> %d", bal1.CreditsPosted, bal2.CreditsPosted)
	}
	// Pool funding leg is idempotent per reference as well.
	pool1, _ := lc.Balance(ledger.AccountID(nsCommissionsPool, 1))
	if pool1.CreditsPosted != recs1[0].AmountKobo {
		t.Fatalf("pool funded more than once: %+v", pool1)
	}

	// Different amount under the same reference: payload-hash conflict.
	if _, err := eng.Accrue(ag.ID, "txn-rep", 3_000_000); !errors.Is(err, ErrCommissionConflict) {
		t.Fatalf("payload conflict: want ErrCommissionConflict, got %v", err)
	}
}

// TestCommissionReplayOnlyIfPosted: when the durable record is lost after a
// post (crash between post and mark), the re-run replays the ledger saga
// idempotently under the deterministic ids and re-marks — no double post.
func TestCommissionReplayOnlyIfPosted(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	ag := seedAgent(t, reg, "solo", "t1")
	recs, err := eng.Accrue(ag.ID, "txn-crash", 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate crash-after-post: drop the durable record, keep the ledger.
	if _, err := eng.st.Delete("commission_records", recordKey("txn-crash", 1)); err != nil {
		t.Fatal(err)
	}
	balBefore, _ := lc.Balance(commissionAccountID(ag.ID))
	recs2, err := eng.Accrue(ag.ID, "txn-crash", 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	balAfter, _ := lc.Balance(commissionAccountID(ag.ID))
	if balAfter.CreditsPosted != balBefore.CreditsPosted {
		t.Fatalf("crash replay double-posted: %d -> %d", balBefore.CreditsPosted, balAfter.CreditsPosted)
	}
	if recs2[0].PostTransferID != recs[0].PostTransferID {
		t.Fatalf("crash replay must reuse the deterministic post id")
	}
}

// TestCommissionPackVersionFailClosed: COMMISSION_PACK_VERSION asking for a
// version the build does not carry refuses to load (never compute against an
// unverified table).
func TestCommissionPackVersionFailClosed(t *testing.T) {
	t.Setenv("COMMISSION_PACK_VERSION", "9.9.9")
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	if _, err := LoadCommissionEngine(st, NewHierarchy(reg), ledger.NewDevClient()); err == nil {
		t.Fatal("unknown COMMISSION_PACK_VERSION must fail closed")
	}
}

// TestCommissionDepthCapAlignment: the pack carries no rate beyond the
// hierarchy depth cap, so deeper uplines simply earn nothing (no leak).
func TestCommissionDepthCapAlignment(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	chain := make([]Agent, 4)
	for i := range chain {
		chain[i] = seedAgent(t, reg, string(rune('a'+i)), "t1")
		if i > 0 {
			mustAttach(t, eng.hierarchy, chain[i].ID, chain[i-1].ID)
		}
	}
	recs, err := eng.Accrue(chain[3].ID, "txn-depth", 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 { // levels 1..3 only; the depth-cap root earns nothing
		t.Fatalf("want 3 records within depth cap, got %d", len(recs))
	}
	rootBal, _ := lc.Balance(commissionAccountID(chain[0].ID))
	if rootBal.CreditsPosted != 0 {
		t.Fatalf("root beyond pack levels must earn nothing: %+v", rootBal)
	}
}
