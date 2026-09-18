package main

// commission_r4_test.go — R4-S3#15 regression: clawback for refunded/voided
// source payments, tenant-bound idempotency keys, ≥35-day replay TTL, and
// canonical-pack loading with prod fail-closed.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// TestR4CommissionClawback covers the refund/void path: a clawed-back
// commission is reversed from the agent's payable account into the pool,
// the record becomes terminal, and BOTH a repeated clawback and a replayed
// accrual of the same reference are idempotent no-ops.
func TestR4CommissionClawback(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	ag := seedAgent(t, reg, "solo", "t1")

	const base = uint64(1_000_000)
	recs, err := eng.Accrue(ag.ID, "txn-refund", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Status != "posted" {
		t.Fatalf("accrue: %+v", recs)
	}
	agentBal, err := lc.Balance(recs[0].AccountID)
	if err != nil || agentBal.CreditsPosted != 25_000 {
		t.Fatalf("agent credited: %+v err=%v", agentBal, err)
	}

	// The source payment is refunded: claw the commission back.
	clawed, err := eng.Clawback("t1", "txn-refund", "source payment refunded")
	if err != nil {
		t.Fatal(err)
	}
	if len(clawed) != 1 || clawed[0].Status != "clawed_back" {
		t.Fatalf("clawback: %+v", clawed)
	}
	// Money moved back to the pool (agent payable debited 25,000; the pool
	// funding credit nets the clawback credit).
	agentBal, err = lc.Balance(recs[0].AccountID)
	if err != nil || agentBal.DebitsPosted != 25_000 {
		t.Fatalf("agent not debited back: %+v err=%v", agentBal, err)
	}
	pool, err := lc.Balance(ledger.AccountID(nsCommissionsPool, 1))
	// pool: funded 25k at accrue, re-credited 25k by the clawback
	if err != nil || pool.CreditsPosted != 50_000 {
		t.Fatalf("pool not re-credited: %+v err=%v", pool, err)
	}

	// Idempotent: a second clawback changes nothing.
	again, err := eng.Clawback("t1", "txn-refund", "source payment refunded")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("clawback replay: %+v", again)
	}
	agentBal, _ = lc.Balance(recs[0].AccountID)
	if agentBal.DebitsPosted != 25_000 {
		t.Fatalf("clawback replay double-debited: %+v", agentBal)
	}

	// Terminal: replaying the accrual of the refunded reference must NOT
	// re-accrue (the pre-fix record-TTL/status gap re-posted it).
	replay, err := eng.Accrue(ag.ID, "txn-refund", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 1 || replay[0].Status != "clawed_back" {
		t.Fatalf("accrue replay after clawback re-accrued: %+v", replay)
	}
	agentBal, _ = lc.Balance(recs[0].AccountID)
	if agentBal.CreditsPosted != 25_000 {
		t.Fatalf("replay re-credited the agent: %+v", agentBal)
	}
}

// TestR4CommissionClawbackTenantScoped: a tenant-scoped clawback never
// touches another tenant's records for the same reference string.
func TestR4CommissionClawbackTenantScoped(t *testing.T) {
	eng, reg, lc := newCommissionEngine(t)
	a := seedAgent(t, reg, "a", "t1")
	b := seedAgent(t, reg, "b", "t2")

	ra, err := eng.Accrue(a.ID, "shared-ref", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := eng.Accrue(b.ID, "shared-ref", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// Tenant-bound keys mean the same reference accrues independently per
	// tenant (pre-fix, the (reference, level) key would have 409-conflicted).
	if len(ra) != 1 || len(rb) != 1 {
		t.Fatalf("per-tenant accrual: %d / %d", len(ra), len(rb))
	}

	// Claw back only t1's record: t2's commission is untouched.
	clawed, err := eng.Clawback("t1", "shared-ref", "t1 refund")
	if err != nil {
		t.Fatal(err)
	}
	if len(clawed) != 1 || clawed[0].TenantID != "t1" {
		t.Fatalf("tenant-scoped clawback: %+v", clawed)
	}
	bBal, _ := lc.Balance(rb[0].AccountID)
	if bBal.DebitsPosted != 0 {
		t.Fatalf("cross-tenant record debited: %+v", bBal)
	}
	var recB CommissionRecord
	if ok, _ := eng.st.Get("commission_records", recordKey("t2", "shared-ref", 1), &recB); !ok || recB.Status != "posted" {
		t.Fatalf("t2 record must stay posted: %+v ok=%v", recB, ok)
	}
}

// TestR4CommissionReplayTTL: the replay window covers the monthly period.
func TestR4CommissionReplayTTL(t *testing.T) {
	if got := commissionRecordTTL.Hours(); got < 35*24 {
		t.Fatalf("commission record TTL = %vh, want >= %vh", got, 35*24.0)
	}
	if got := commissionPayoutTTL.Hours(); got < 35*24 {
		t.Fatalf("payout marker TTL = %vh, want >= %vh", got, 35*24.0)
	}
}

// TestR4CommissionPackLoading: canonical pack preferred; prod fails closed
// when it is absent; embedded copy remains the dev/offline fallback.
func TestR4CommissionPackLoading(t *testing.T) {
	st, _ := store.Open("")
	reg := NewAgentRegistry(st)
	lc := ledger.NewDevClient()

	// 1) canonical pack file wins over the embedded copy.
	dir := t.TempDir()
	packFile := filepath.Join(dir, "rp-commissions-ng.json")
	canonical := []byte(`{
	  "id": "rp-commissions-ng", "version": "2.0.0",
	  "rules": {
	    "currency": "NGN", "unit": "kobo",
	    "levels": [
	      {"level": 1, "rate_bps": 500, "narrate": "capturing agent"},
	      {"level": 2, "rate_bps": 100, "narrate": "upline level 2"},
	      {"level": 3, "rate_bps": 50, "narrate": "upline level 3"}
	    ]
	  }
	}`)
	if err := os.WriteFile(packFile, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMISSION_PACK_FILE", packFile)
	eng, err := LoadCommissionEngine(st, NewHierarchy(reg), lc)
	if err != nil {
		t.Fatal(err)
	}
	if eng.PackVersion() != "rp-commissions-ng@2.0.0" {
		t.Fatalf("canonical pack not used: %s", eng.PackVersion())
	}
	ag := seedAgent(t, reg, "x", "t1")
	recs, err := eng.Accrue(ag.ID, "txn-pack", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].AmountKobo != 50_000 { // 500bps on ₦10,000.00
		t.Fatalf("canonical bps not applied: %+v", recs)
	}

	// 2) prod fail-closed: required pack absent -> load error.
	t.Setenv("COMMISSION_PACK_FILE", filepath.Join(dir, "missing.json"))
	t.Setenv("COMMISSION_PACK_REQUIRED", "true")
	if _, err := LoadCommissionEngine(st, NewHierarchy(reg), lc); err == nil {
		t.Fatal("prod with missing canonical pack must fail closed")
	}

	// 3) required with NO path configured at all -> also fail closed.
	t.Setenv("COMMISSION_PACK_FILE", "")
	t.Setenv("RULE_PACKS_DIR", "")
	if _, err := LoadCommissionEngine(st, NewHierarchy(reg), lc); err == nil {
		t.Fatal("prod with no pack path must fail closed (no silent embedded fallback)")
	}

	// 4) dev without the requirement: embedded fallback still loads.
	t.Setenv("COMMISSION_PACK_REQUIRED", "false")
	if _, err := LoadCommissionEngine(st, NewHierarchy(reg), lc); err != nil {
		t.Fatalf("dev embedded fallback must load: %v", err)
	}
}
