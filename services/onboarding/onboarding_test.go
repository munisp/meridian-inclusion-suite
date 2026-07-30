package main

import (
	"testing"
	"time"

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
