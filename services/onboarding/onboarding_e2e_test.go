package main

import (
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

// --- O2: state machine ---

func TestTransitionTable(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		{"registered", "nin_verified", true},
		{"registered", "pending_review", true},
		{"registered", "graduated", false},
		{"registered", "tin_provisioned", false},
		{"pending_review", "registered", true},
		{"nin_verified", "tin_provisioned", true},
		{"nin_verified", "registered", false},
		{"tin_provisioned", "graduated", true},
		{"tin_provisioned", "nin_verified", false},
		{"graduated", "registered", false},
		{"rejected", "registered", true},
		{"registered", "registered", true}, // idempotent
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s,%s) = %v want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestRegistryTransitionAuditTrail(t *testing.T) {
	st, _ := store.Open("")
	reg := NewRegistry(st)
	op := Operator{NINHash: NINHash("12345678901"), FullName: "A", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if err := reg.Transition(&op, "graduated", "test"); err == nil {
		t.Fatal("expected illegal transition registered->graduated to fail")
	}
	if err := reg.Transition(&op, "nin_verified", "test"); err != nil {
		t.Fatal(err)
	}
	trail, err := reg.StatusAuditTrail(op.ID)
	if err != nil || len(trail) != 1 {
		t.Fatalf("trail=%+v err=%v", trail, err)
	}
	if trail[0].From != "registered" || trail[0].To != "nin_verified" || trail[0].Actor != "test" {
		t.Fatalf("audit record: %+v", trail[0])
	}
}

// --- O1: durable workflow redrive ---

type flakyVerifier struct{ fail bool }

func (f *flakyVerifier) VerifyNIN(nin string) (NINVerification, error) {
	if f.fail {
		return NINVerification{}, &outageError{"nimc adapter: connection refused"}
	}
	return NIMCSimulator{}.VerifyNIN(nin)
}

type outageError struct{ msg string }

func (e *outageError) Error() string { return e.msg }

func TestTINProvisionRedriveAfterOutage(t *testing.T) {
	st, _ := store.Open("")
	reg := NewRegistry(st)
	fv := &flakyVerifier{fail: true}
	wf := NewWorkflows(st, reg, fv, LocalTINProvisioner{}, NewConsentService(st), ledger.NewDevClient(), events.NewInprocBus())
	op := Operator{NINHash: NINHash("12345678901"), FullName: "A", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	run := wf.TINProvision(op.ID, "12345678901")
	if run.Status != "failed" {
		t.Fatalf("expected failed run during outage, got %+v", run)
	}
	got, _, _ := reg.Get(op.ID)
	if got.TIN != "" {
		t.Fatal("no TIN may be issued during an adapter outage")
	}
	fv.fail = false // adapter recovers
	run2, err := wf.Redrive(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run2.Status != "completed" || run2.Attempt != 2 {
		t.Fatalf("redrive: %+v", run2)
	}
	got, _, _ = reg.Get(op.ID)
	if got.Status != "tin_provisioned" || got.TIN == "" {
		t.Fatalf("expected provisioned after redrive, got %+v", got)
	}
	// a second redrive is an idempotent no-op error (already completed)
	if _, err := wf.Redrive(run.ID); err == nil {
		t.Fatal("expected already-completed error")
	}
}

// --- O5: agent registry ---

func TestAgentRegistryVettingAndValidation(t *testing.T) {
	st, _ := store.Open("")
	ar := NewAgentRegistry(st)
	ag, err := ar.Register(Agent{FullName: "Agent One", Phone: "+23480", State: "Lagos", LGA: "Ikeja", AssociationID: "assoc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ag.ID, "ag_") || ag.VettingStatus != "pending" {
		t.Fatalf("agent: %+v", ag)
	}
	// pending agent cannot capture
	if _, err := ar.ValidateForCapture(ag.ID); err == nil {
		t.Fatal("expected pending agent to be rejected for capture")
	}
	if _, err := ar.SetVetting(ag.ID, "approved", "kyc ok"); err != nil {
		t.Fatal(err)
	}
	if warn, err := ar.ValidateForCapture(ag.ID); err != nil || warn != "" {
		t.Fatalf("approved agent should validate: warn=%s err=%v", warn, err)
	}
	if _, err := ar.SetVetting(ag.ID, "rejected", ""); err == nil {
		t.Fatal("approved->rejected must be illegal")
	}
	if _, err := ar.SetVetting(ag.ID, "suspended", "fraud review"); err != nil {
		t.Fatal(err)
	}
	if _, err := ar.ValidateForCapture(ag.ID); err == nil {
		t.Fatal("suspended agent must not capture")
	}
}

func TestAgentValidationProdFailClosed(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	defer t.Setenv("APP_PROFILE", "")
	st, _ := store.Open("")
	ar := NewAgentRegistry(st)
	for _, bad := range []string{"", "unknown-agent", "free-text-agent"} {
		if _, err := ar.ValidateForCapture(bad); err == nil {
			t.Fatalf("agent_id %q must be rejected in prod profile", bad)
		}
	}
	t.Setenv("APP_PROFILE", "")
	if warn, err := ar.ValidateForCapture("unknown-agent"); err != nil || warn == "" {
		t.Fatalf("dev profile should warn+allow: warn=%q err=%v", warn, err)
	}
}

// --- O4: documents + resumption + review ---

func newDocTestStack(t *testing.T) (*store.Store, *Registry, *DocService) {
	t.Helper()
	st, _ := store.Open("")
	reg := NewRegistry(st)
	backend := newFSDocBackend(t.TempDir())
	return st, reg, NewDocService(st, reg, backend)
}

func TestDocumentPresignCompleteFlow(t *testing.T) {
	_, reg, docs := newDocTestStack(t)
	op := Operator{NINHash: NINHash("12345678901"), FullName: "A", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	res, err := docs.Presign(op.ID, "nin_slip", "nin.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if res.UploadURL == "" || res.Method != "PUT" || res.Backend != "dev_fs" {
		t.Fatalf("presign: %+v", res)
	}
	if _, err := docs.Presign(op.ID, "bogus_kind", "x"); err == nil {
		t.Fatal("invalid kind must be rejected")
	}
	doc, err := docs.Complete(op.ID, res.DocID, "deadbeef", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "uploaded" {
		t.Fatalf("doc: %+v", doc)
	}
	got, _, _ := reg.Get(op.ID)
	if len(got.Documents) != 1 || got.Documents[0].Kind != "nin_slip" {
		t.Fatalf("doc ref not attached: %+v", got.Documents)
	}
	// idempotent complete
	doc2, err := docs.Complete(op.ID, res.DocID, "deadbeef", 1024)
	if err != nil || doc2.Status != "uploaded" {
		t.Fatalf("idempotent complete: %+v err=%v", doc2, err)
	}
}

func TestOnboardingResumptionStatus(t *testing.T) {
	_, reg, docs := newDocTestStack(t)
	op := Operator{NINHash: NINHash("12345678901"), FullName: "A", CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	st0 := docs.Status(op)
	if st0.CurrentStep != "identity_verification" {
		t.Fatalf("step: %+v", st0)
	}
	joined := strings.Join(st0.MissingItems, ",")
	if !strings.Contains(joined, "nimc_verification") || !strings.Contains(joined, "ndpa_consent") {
		t.Fatalf("missing items: %v", st0.MissingItems)
	}
	// progress the record; step must advance
	if err := reg.Transition(&op, "pending_review", "test"); err != nil {
		t.Fatal(err)
	}
	st1 := docs.Status(op)
	if st1.CurrentStep != "review" || !strings.Contains(strings.Join(st1.MissingItems, ","), "review_decision") {
		t.Fatalf("pending_review view: %+v", st1)
	}
}

func TestReviewApproveReject(t *testing.T) {
	st, reg, _ := newDocTestStack(t)
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), ledger.NewDevClient(), events.NewInprocBus())
	_ = wf
	op := Operator{NINHash: NINHash("12345678901"), FullName: "A", CapturedAt: nowRFC3339(), ReviewStatus: "pending"}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if err := reg.Transition(&op, "pending_review", "capture:offline_age"); err != nil {
		t.Fatal(err)
	}
	// reject path
	if err := reg.Transition(&op, "rejected", "review:admin"); err != nil {
		t.Fatal(err)
	}
	op.ReviewStatus = "rejected"
	if err := reg.Update(op); err != nil {
		t.Fatal(err)
	}
	// re-onboard path: rejected -> registered is legal
	if err := reg.Transition(&op, "registered", "review:admin"); err != nil {
		t.Fatal(err)
	}
}

func TestMinIOPresignShape(t *testing.T) {
	m := &minioDocBackend{endpoint: "http://minio:9000", region: "us-east-1", bucket: "onboarding-worm", accessKey: "AK", secretKey: "SK", worm: true}
	doc := DocRef{ID: "doc_1", ObjectKey: "onboarding/op_1/doc_1"}
	res, err := m.Presign(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.UploadURL, "http://minio:9000/onboarding-worm/onboarding/op_1/doc_1?") {
		t.Fatalf("url: %s", res.UploadURL)
	}
	for _, want := range []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Signature=", "X-Amz-Credential=AK%2F"} {
		if !strings.Contains(res.UploadURL, want) {
			t.Fatalf("missing %q in %s", want, res.UploadURL)
		}
	}
	if res.Backend != "minio_worm" {
		t.Fatalf("backend: %s", res.Backend)
	}
}

func TestDocBackendProdFailClosed(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("MINIO_ENDPOINT", "")
	defer t.Setenv("APP_PROFILE", "")
	if _, err := NewDocBackendFromEnv(t.TempDir()); err == nil {
		t.Fatal("prod without MINIO_ENDPOINT must fail closed")
	}
}

var _ = workflowx.IsProdProfile // keep import used if tests change
