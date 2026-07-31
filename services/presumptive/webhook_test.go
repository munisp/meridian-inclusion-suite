package main

import (
	"errors"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
)

// authorisedPayment runs intent -> PSSP authorise and returns the payment in
// "authorised" state, ready for webhook-driven capture.
func authorisedPayment(t *testing.T, ts *testStack, tin string) Payment {
	t.Helper()
	ts.openGate(t)
	p, err := ts.pay.CreateIntent(IntentRequest{
		TINHash: tin, State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, auth, err := ts.pay.Authorise(p.ID)
	if err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	if p.PSSPRef == "" {
		t.Fatal("expected pssp ref after authorise")
	}
	return p
}

// TestWebhookCaptureHappyPath: a charge.successful webhook whose values match
// the intent verifies against the provider and captures.
func TestWebhookCaptureHappyPath(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-happy")
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{
		Reference: p.PSSPRef, Event: "charge.successful",
		AmountKobo: p.AmountKobo, Currency: "NGN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "captured" || got.CertificateSerial == "" {
		t.Fatalf("expected captured + certificate, got %+v", got)
	}
}

// TestWebhookAmountTampered (G1): a webhook echoing a different amount than
// the stored intent is rejected (ErrWebhookMismatch -> 409) with NO state
// change, and an alert is published.
func TestWebhookAmountTampered(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-tamper")
	_, err := ts.pay.HandleWebhook("remita", WebhookPayload{
		Reference: p.PSSPRef, Event: "charge.successful",
		AmountKobo: p.AmountKobo + 100, Currency: "NGN",
	})
	if !errors.Is(err, ErrWebhookMismatch) {
		t.Fatalf("tampered amount must be rejected, got %v", err)
	}
	got, _, _ := ts.pay.get(p.ID)
	if got.Status != "authorised" || got.CertificateSerial != "" {
		t.Fatalf("no state change expected on mismatch, got %+v", got)
	}
	if n := len(ts.pay.bus.(*events.InprocBus).Published("nrs.payments.alerts.v1")); n != 1 {
		t.Fatalf("expected 1 mismatch alert, got %d", n)
	}
}

// TestWebhookCurrencyMismatch (G1): a webhook settling in a different
// currency than the NGN-locked intent is rejected with no state change.
func TestWebhookCurrencyMismatch(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-ccy")
	_, err := ts.pay.HandleWebhook("remita", WebhookPayload{
		Reference: p.PSSPRef, Event: "charge.successful",
		AmountKobo: p.AmountKobo, Currency: "USD",
	})
	if !errors.Is(err, ErrWebhookMismatch) {
		t.Fatalf("currency mismatch must be rejected, got %v", err)
	}
	got, _, _ := ts.pay.get(p.ID)
	if got.Status != "authorised" {
		t.Fatalf("no state change expected on mismatch, got %s", got.Status)
	}
}

// TestWebhookDuplicateDelivery (G2): a redelivered charge.successful is a 200
// idempotent no-op (never a 422), not a second capture.
func TestWebhookDuplicateDelivery(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-dup")
	payload := WebhookPayload{Reference: p.PSSPRef, Event: "charge.successful", AmountKobo: p.AmountKobo, Currency: "NGN"}
	first, err := ts.pay.HandleWebhook("remita", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.pay.HandleWebhook("remita", payload)
	if err != nil {
		t.Fatalf("duplicate delivery must be acked (200), got %v", err)
	}
	if second.Status != "captured" || second.CertificateSerial != first.CertificateSerial {
		t.Fatalf("duplicate must be a no-op replay: first=%+v second=%+v", first, second)
	}
	// the dedup record exists, keyed provider:reference:event
	var rec ProcessedWebhookEvent
	ok, err := ts.st.Get("webhook_events", webhookEventID("remita", p.PSSPRef, "charge.successful"), &rec)
	if err != nil || !ok || rec.Outcome != "applied" {
		t.Fatalf("expected applied dedup record, got %+v ok=%v err=%v", rec, ok, err)
	}
}

// TestWebhookOutOfOrderAuthAfterCapture (G2): a late authorisation.successful
// arriving after capture must NOT downgrade the payment back to authorised
// (monotonic state machine) — it is parked and acked.
func TestWebhookOutOfOrderAuthAfterCapture(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-ooo")
	if _, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.successful", AmountKobo: p.AmountKobo}); err != nil {
		t.Fatal(err)
	}
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "authorisation.successful"})
	if err != nil {
		t.Fatalf("out-of-order event must be acked, got %v", err)
	}
	if got.Status != "captured" {
		t.Fatalf("late authorisation must not downgrade captured payment, got %s", got.Status)
	}
	var rec ProcessedWebhookEvent
	ok, _ := ts.st.Get("webhook_events", webhookEventID("remita", p.PSSPRef, "authorisation.successful"), &rec)
	if !ok || rec.Outcome != "parked" {
		t.Fatalf("expected parked dedup record, got %+v ok=%v", rec, ok)
	}
	// and the payment cannot be re-captured through a second charge webhook
	// (idempotent ack, not a second saga run)
	again, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.successful"})
	if err != nil {
		t.Fatalf("redelivery after success must be acked (200), got %v", err)
	}
	if again.Status != "captured" {
		t.Fatalf("redelivery must not regress state, got %s", again.Status)
	}
}
