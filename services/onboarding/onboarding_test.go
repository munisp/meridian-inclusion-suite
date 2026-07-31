package main

import (
	"strings"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/crdtx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func newTestStack(t *testing.T) (*store.Store, *Registry, *Workflows) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	bus := events.NewInprocBus()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, bus)
	return st, reg, wf
}

func TestNIMCSimulator(t *testing.T) {
	v, err := NIMCSimulator{}.VerifyNIN("12345678901")
	if err != nil || !v.Verified {
		t.Fatalf("expected verified, got %+v err=%v", v, err)
	}
	if v.NINHash == "" || v.Reference == "" {
		t.Fatal("expected nin_hash + reference")
	}
	bad, _ := NIMCSimulator{}.VerifyNIN("123")
	if bad.Verified {
		t.Fatal("expected rejection for short NIN")
	}
	miss, _ := NIMCSimulator{}.VerifyNIN("12345670000")
	if miss.Verified {
		t.Fatal("expected simulated miss for NIN ending 0000")
	}
}

func TestTINHashPseudonymisation(t *testing.T) {
	h1 := TINHash("2000000001")
	h2 := TINHash("2000000001")
	if h1 != h2 || len(h1) != 64 {
		t.Fatal("tin_hash must be deterministic HMAC-SHA256 hex")
	}
	if NINHash("12345678901") == TINHash("12345678901") {
		t.Fatal("nin and tin keys must differ")
	}
}

func TestCaptureIngestIdempotencyAndConflicts(t *testing.T) {
	st, reg, _ := newTestStack(t)
	cap := NewCaptureService(st, reg, NIMCSimulator{})
	now := time.Now().UTC()
	items := []CaptureItem{{
		ClientRef: "ref-1", NIN: "12345678901", FullName: "Adaeze Okafor",
		Phone: "08030000001", State: "Lagos", LGA: "Ikeja", TradeCategory: "tailoring",
		CapturedAt: now.Add(-80 * time.Hour).Format(time.RFC3339), // >72h offline
	}}
	b1, err := cap.Ingest("agent-1", "idem-key-1", items)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Results[0].Outcome != "created" {
		t.Fatalf("expected created, got %+v", b1.Results[0])
	}
	if b1.Results[0].OfflineAgeHours < 72 {
		t.Fatal("expected offline age > 72h to be reported")
	}
	// idempotent replay
	b2, err := cap.Ingest("agent-1", "idem-key-1", items)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Status != "duplicate" || b2.ID != b1.ID {
		t.Fatalf("expected duplicate replay of batch %s, got %+v", b1.ID, b2)
	}
	// same client_ref in a new batch -> duplicate_client_ref
	b3, err := cap.Ingest("agent-1", "idem-key-2", items)
	if err != nil {
		t.Fatal(err)
	}
	if b3.Results[0].Outcome != "duplicate_client_ref" {
		t.Fatalf("expected duplicate_client_ref, got %+v", b3.Results[0])
	}
	// conflict: same NIN, newer captured_at wins
	items[0].ClientRef = "ref-2"
	items[0].FullName = "Adaeze Okafor-Nwosu"
	items[0].CapturedAt = now.Format(time.RFC3339)
	b4, err := cap.Ingest("agent-2", "idem-key-3", items)
	if err != nil {
		t.Fatal(err)
	}
	if b4.Results[0].Outcome != "conflict_resolved" {
		t.Fatalf("expected conflict_resolved, got %+v", b4.Results[0])
	}
	op, ok, _ := reg.FindByNINHash(NINHash("12345678901"))
	if !ok || op.FullName != "Adaeze Okafor-Nwosu" {
		t.Fatalf("expected last-writer-wins update, got %+v", op)
	}
}

func TestWorkflowTINProvisionAndCommission(t *testing.T) {
	_, reg, wf := newTestStack(t)
	op := Operator{NINHash: NINHash("12345678901"), FullName: "Test Op", AgentID: "agent-9", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	run := wf.TINProvision(op.ID, "12345678901")
	if run.Status != "completed" {
		t.Fatalf("workflow failed: %s", run.Error)
	}
	got, _, _ := reg.Get(op.ID)
	if got.Status != "tin_provisioned" || got.TIN == "" || got.TINHash == "" {
		t.Fatalf("expected provisioned operator, got %+v", got)
	}
	// commission settlement on dev ledger 700
	settle := wf.CommissionSettlement()
	if settle.Status != "completed" {
		t.Fatalf("settlement failed: %s", settle.Error)
	}
	res := settle.Result.(map[string]any)
	if res["total_kobo"].(uint64) != commissionPerVerifiedKobo {
		t.Fatalf("expected %d kobo settled, got %v", commissionPerVerifiedKobo, res["total_kobo"])
	}
}

func TestConsentLocalFallback(t *testing.T) {
	st, _, _ := newTestStack(t)
	cs := NewConsentService(st)
	rec, err := cs.Capture("op_1", "tax_onboarding", "agent_pwa", true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Receipt == "" || rec.Source != "local_fallback" {
		t.Fatalf("expected receipt + fallback source, got %+v", rec)
	}
	rev, err := cs.Revoke(rec.ID)
	if err != nil || !rev.Revoked {
		t.Fatalf("expected revoked consent, got %+v err=%v", rev, err)
	}
}

// TestTwoBatchesWithDifferentKeysBothIngest is the regression test for the
// PWA idempotency-key-reuse data-loss bug (Capture.tsx persisted one batchId
// forever): two batches with DIFFERENT Idempotency-Keys must both ingest.
func TestTwoBatchesWithDifferentKeysBothIngest(t *testing.T) {
	st, reg, _ := newTestStack(t)
	cap := NewCaptureService(st, reg, NIMCSimulator{})
	now := time.Now().UTC().Format(time.RFC3339)
	b1, err := cap.Ingest("agent-1", "batch-key-A", []CaptureItem{{
		ClientRef: "ref-a", NIN: "12345678901", FullName: "First Operator", CapturedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b1.Results[0].Outcome != "created" {
		t.Fatalf("batch A must create, got %+v", b1.Results[0])
	}
	// Second batch, new key (what the fixed PWA now sends per sync attempt).
	b2, err := cap.Ingest("agent-1", "batch-key-B", []CaptureItem{{
		ClientRef: "ref-b", NIN: "12345678902", FullName: "Second Operator", CapturedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b2.Status != "processed" || b2.Results[0].Outcome != "created" {
		t.Fatalf("batch B with a fresh key must ingest, got status=%s result=%+v", b2.Status, b2.Results[0])
	}
	// And the server still dedups a retried batch on the SAME key.
	b2r, err := cap.Ingest("agent-1", "batch-key-B", []CaptureItem{{
		ClientRef: "ref-b", NIN: "12345678902", FullName: "Second Operator", CapturedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b2r.Status != "duplicate" {
		t.Fatalf("same key must still replay as duplicate, got %s", b2r.Status)
	}
	ops, _ := reg.List()
	if len(ops) != 2 {
		t.Fatalf("expected 2 operators (one per batch), got %d", len(ops))
	}
}

// TestCommissionSummaryServerSide (audit fix #2): commissions are computed
// server-side from the registry at the pack rate, keyed to the agent id the
// server is given (which the HTTP layer binds to the authenticated identity).
func TestCommissionSummaryServerSide(t *testing.T) {
	_, reg, _ := newTestStack(t)
	mk := func(id, agent, status string) {
		op := Operator{NINHash: NINHash("1234567890" + id[len(id)-1:]), FullName: "Op " + id, AgentID: agent, Status: status, CapturedAt: nowRFC3339()}
		if err := reg.Create(&op); err != nil {
			t.Fatal(err)
		}
	}
	mk("a1", "agent-1", "nin_verified")
	mk("a2", "agent-1", "tin_provisioned")
	mk("a3", "agent-1", "registered")
	mk("b1", "agent-2", "graduated")
	sum, err := CommissionSummaryFor(reg, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Captured != 3 || sum.Verified != 2 {
		t.Fatalf("bad counts: %+v", sum)
	}
	if sum.AccruedKobo != 2*commissionPerVerifiedKobo || sum.RateKobo != commissionPerVerifiedKobo {
		t.Fatalf("bad accrual: %+v", sum)
	}
	// another agent's records are never attributed
	sum2, _ := CommissionSummaryFor(reg, "agent-2")
	if sum2.Verified != 1 || sum2.AccruedKobo != commissionPerVerifiedKobo {
		t.Fatalf("cross-agent attribution leak: %+v", sum2)
	}
}

// TestAssociationBulkOnboarding (I16): CSV roster enrolment with dedup on
// re-upload (deterministic client_ref) and NIN-hash conflict handling.
func TestAssociationBulkOnboarding(t *testing.T) {
	st, reg, _ := newTestStack(t)
	cap := NewCaptureService(st, reg, NIMCSimulator{})
	as := NewAssociationService(st, reg, cap)
	a, err := as.Create(Association{Name: "Balogun Market Union", State: "Lagos", AdminName: "Iya Oja"})
	if err != nil {
		t.Fatal(err)
	}
	csv1 := "nin,full_name,phone,trade_category\n12345678901,Adaeze Okafor,08030000001,retail\n12345678902,Musa Bello,08030000002,food_vendor\n"
	res, err := as.EnrollCSV(a.ID, "agent-1", strings.NewReader(csv1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.Rows != 2 {
		t.Fatalf("expected 2 created, got %+v", res)
	}
	// re-upload same roster: everything dedups (no double enrolment)
	res2, err := as.EnrollCSV(a.ID, "agent-1", strings.NewReader(csv1))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created != 0 || res2.Duplicates != 2 {
		t.Fatalf("re-upload must dedup, got %+v", res2)
	}
	ops, _ := reg.List()
	if len(ops) != 2 {
		t.Fatalf("dedup failed: %d operators", len(ops))
	}
	// same NIN via a DIFFERENT association: conflict-resolved, still one record
	a2, _ := as.Create(Association{Name: "Artisan Guild", State: "Lagos", AdminName: "Chairman"})
	csv2 := "nin,full_name\n12345678901,Adaeze Okafor-Nwosu\n"
	res3, err := as.EnrollCSV(a2.ID, "agent-2", strings.NewReader(csv2))
	if err != nil {
		t.Fatal(err)
	}
	if res3.Results[0].Outcome != "conflict_resolved" {
		t.Fatalf("cross-association NIN dedup failed: %+v", res3.Results[0])
	}
	ops, _ = reg.List()
	if len(ops) != 2 {
		t.Fatalf("cross-association dedup failed: %d operators", len(ops))
	}
}

// TestCRDTMergeEndpointLogic (I18): the server merge is idempotent under
// duplicate + out-of-order op delivery.
func TestCRDTMergeEndpointLogic(t *testing.T) {
	m := NewCRDTMergeService()
	clk := crdtx.NewClock("agent-1")
	add := crdtx.Op{ID: "op-1", Kind: "add", Element: "ref-1", Tag: clk.Now()}
	rm := crdtx.Op{ID: "op-2", Kind: "remove", Element: "ref-1", Tag: add.Tag}
	r1 := m.Merge([]crdtx.Op{add, add, rm}) // duplicate add in one batch
	if r1.Applied != 2 || r1.Ignored != 1 {
		t.Fatalf("bad merge accounting: %+v", r1)
	}
	if len(r1.Elements) != 0 {
		t.Fatalf("removed element must not be live: %+v", r1.Elements)
	}
	r2 := m.Merge([]crdtx.Op{rm, add}) // replay, reversed
	if r2.Applied != 0 || r2.Ignored != 2 || len(r2.Elements) != 0 {
		t.Fatalf("replay must be a no-op: %+v", r2)
	}
}
