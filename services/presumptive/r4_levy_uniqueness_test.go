package main

import (
	"errors"
	"testing"
)

// flowResult extracts the (certificate, payment) pair from a completed
// PaymentFlow run.
func flowResult(t *testing.T, run PSMWorkflowRun) (Certificate, Payment) {
	t.Helper()
	m, ok := run.Result.(map[string]any)
	if !ok {
		t.Fatalf("run.Result is %T, want map", run.Result)
	}
	cert, _ := m["certificate"].(Certificate)
	pay, _ := m["payment"].(Payment)
	return cert, pay
}

// R4-S1b#9 regression: (tin, period, levy-class) uniqueness across channels.
// Before the fix, a second intent with a different idempotency key (e.g. a
// USSD redial AND a POS double-pay of the same levy) created a second
// pending hold and ultimately a second capture + certificate.

func TestR4DuplicateLevyRejected(t *testing.T) {
	ts := newTestStack(t)
	base := IntentRequest{
		TINHash: "tinhash-r4-dup", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: "pos-channel-key",
	}
	first, err := ts.pay.CreateIntent(base)
	if err != nil {
		t.Fatal(err)
	}

	// 1) Cross-channel double-pay: different key, same (tin, period, class)
	//    from the "PWA channel" must be rejected.
	other := base
	other.IdempotencyKey = "pwa-channel-key"
	if _, err := ts.pay.CreateIntent(other); !errors.Is(err, ErrDuplicateLevy) {
		t.Fatalf("cross-channel double-pay: want ErrDuplicateLevy, got %v", err)
	}

	// 2) Same-channel no-key attempt (e.g. raw API) is also rejected.
	noKey := base
	noKey.IdempotencyKey = ""
	if _, err := ts.pay.CreateIntent(noKey); !errors.Is(err, ErrDuplicateLevy) {
		t.Fatalf("no-key duplicate: want ErrDuplicateLevy, got %v", err)
	}

	// 3) Idempotent replay of the ORIGINAL key still returns the original
	//    payment (not a duplicate error).
	again, err := ts.pay.CreateIntent(base)
	if err != nil {
		t.Fatalf("same-key replay must replay, got %v", err)
	}
	if again.ID != first.ID {
		t.Fatal("same-key replay returned a different payment")
	}

	// 4) A different period is a different levy — allowed.
	nextYear := base
	nextYear.Period = "2027"
	nextYear.IdempotencyKey = "pwa-channel-key"
	if _, err := ts.pay.CreateIntent(nextYear); err != nil {
		t.Fatalf("different period must be allowed: %v", err)
	}

	// 5) After the first payment is voided, the levy can be paid again.
	if _, err := ts.pay.Void(first.ID); err != nil {
		t.Fatal(err)
	}
	repay := base
	repay.IdempotencyKey = "pwa-after-void"
	if _, err := ts.pay.CreateIntent(repay); err != nil {
		t.Fatalf("re-pay after void must be allowed: %v", err)
	}
}

// TestR4WorkflowRedialResumes covers the USSD session-drop scenario: the
// first trigger captures; the redial (same deterministic key) replays the
// SAME capture + certificate — never a second payment.
func TestR4WorkflowRedialResumes(t *testing.T) {
	ts := newTestStack(t)
	input := map[string]any{
		"tin_hash": "tinhash-r4-ussd", "state": "Lagos", "trade_category": "retail",
		"annual_turnover_kobo": float64(300000000), "provider": "remita",
		"period": "2026", "idempotency_key": "ussd:deterministic-key-1",
	}
	run1 := ts.wf.PaymentFlow(input)
	if run1.Status != "completed" {
		t.Fatalf("first run: %s (%s)", run1.Status, run1.Error)
	}
	cert1, pay1 := flowResult(t, run1)

	// Redial: same key -> idempotent replay, same payment + certificate.
	run2 := ts.wf.PaymentFlow(input)
	if run2.Status != "completed" {
		t.Fatalf("redial run: %s (%s)", run2.Status, run2.Error)
	}
	cert2, pay2 := flowResult(t, run2)
	if pay2.ID != pay1.ID || cert2.Serial != cert1.Serial {
		t.Fatalf("redial produced a different capture/cert: %s/%s vs %s/%s",
			pay2.ID, cert2.Serial, pay1.ID, cert1.Serial)
	}

	// Exactly one payment exists.
	var payments []Payment
	if err := ts.st.List("payments", &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 {
		t.Fatalf("%d payments after redial; want exactly 1", len(payments))
	}

	// Cross-channel double-pay through the workflow (different key) is
	// rejected by the (tin, period, class) guard.
	cross := map[string]any{}
	for k, v := range input {
		cross[k] = v
	}
	cross["idempotency_key"] = "pos:another-channel"
	run3 := ts.wf.PaymentFlow(cross)
	if run3.Status == "completed" {
		t.Fatal("cross-channel double-pay via workflow must not complete")
	}
	var payments2 []Payment
	if err := ts.st.List("payments", &payments2); err != nil {
		t.Fatal(err)
	}
	if len(payments2) != 1 {
		t.Fatalf("%d payments after cross-channel attempt; want exactly 1", len(payments2))
	}
}

// TestR4WorkflowResumeAfterAuthorise simulates a session drop AFTER the PSSP
// authorise but before capture: the redial (same key) must resume at the
// capture leg, not fail "cannot authorise" and not open a new intent.
func TestR4WorkflowResumeAfterAuthorise(t *testing.T) {
	ts := newTestStack(t)
	in := IntentRequest{
		TINHash: "tinhash-r4-resume", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: "ussd:resume-key",
	}
	p, err := ts.pay.CreateIntent(in)
	if err != nil {
		t.Fatal(err)
	}
	// Session drops here, post-authorise.
	p, auth, err := ts.pay.Authorise(p.ID)
	if err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %v %+v", err, auth)
	}

	// Redial: the workflow replays the key, finds the payment authorised,
	// and resumes at capture.
	run := ts.wf.PaymentFlow(map[string]any{
		"tin_hash": in.TINHash, "state": in.State, "trade_category": in.TradeCategory,
		"annual_turnover_kobo": float64(in.AnnualTurnoverKobo), "provider": in.Provider,
		"period": in.Period, "idempotency_key": in.IdempotencyKey,
	})
	if run.Status != "completed" {
		t.Fatalf("resume run: %s (%s)", run.Status, run.Error)
	}
	cert, rp := flowResult(t, run)
	if rp.ID != p.ID || rp.Status != "captured" || cert.Serial == "" {
		t.Fatalf("resume produced %+v / cert %+v", rp, cert)
	}
	var payments []Payment
	if err := ts.st.List("payments", &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 {
		t.Fatalf("%d payments after resume; want exactly 1", len(payments))
	}
}
