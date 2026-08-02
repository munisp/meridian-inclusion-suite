package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
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

// --- PSSP webhook replay guard (audit funds-flow #5) ---

// signedWebhookRequest builds a properly signed PSSP webhook HTTP request
// with the given timestamp header.
func signedWebhookRequest(t *testing.T, p Payment, ts string) *http.Request {
	t.Helper()
	body := []byte(`{"reference":"` + p.PSSPRef + `","event":"charge.successful","amount_kobo":` +
		fmt.Sprint(p.AmountKobo) + `,"currency":"NGN"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/pssp/webhook/remita", strings.NewReader(string(body)))
	req.SetPathValue("provider", "remita")
	req.Header.Set("X-PSSP-Signature", hmacHex(webhookSecret(), string(body)))
	if ts != "" {
		req.Header.Set("X-PSSP-Timestamp", ts)
	}
	return req
}

func guardedServer(ts *testStack) *server {
	return &server{
		pay:   ts.pay,
		guard: webhookguard.NewGuard("X-PSSP-Timestamp", "X-PSSP-Signature", true, nil),
	}
}

// A valid signed webhook with a fresh timestamp is accepted; the exact
// replay (same signature nonce) dedups to 409.
func TestPSSPWebhookReplayGuardDedups(t *testing.T) {
	ts := newTestStack(t)
	p := authorisedPayment(t, ts, "wh-replay")
	srv := guardedServer(ts)
	now := fmt.Sprint(time.Now().Unix())

	rec := httptest.NewRecorder()
	srv.psspWebhook(rec, signedWebhookRequest(t, p, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh webhook must be accepted, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.psspWebhook(rec2, signedWebhookRequest(t, p, now))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("replayed webhook must dedup to 409, got %d (%s)", rec2.Code, rec2.Body.String())
	}
}

// A signed webhook with a timestamp outside the ±5 min tolerance is
// rejected 401, and a missing timestamp (prod fail-closed) is also 401.
func TestPSSPWebhookReplayGuardStaleAndMissing(t *testing.T) {
	ts := newTestStack(t)
	srv := guardedServer(ts)

	p1 := authorisedPayment(t, ts, "wh-stale")
	stale := fmt.Sprint(time.Now().Add(-10 * time.Minute).Unix())
	rec := httptest.NewRecorder()
	srv.psspWebhook(rec, signedWebhookRequest(t, p1, stale))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp must be 401, got %d (%s)", rec.Code, rec.Body.String())
	}

	p2 := authorisedPayment(t, ts, "wh-missing")
	rec2 := httptest.NewRecorder()
	srv.psspWebhook(rec2, signedWebhookRequest(t, p2, ""))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("missing timestamp must fail closed with 401, got %d (%s)", rec2.Code, rec2.Body.String())
	}

	// payments untouched by rejected webhooks
	got, _, _ := ts.pay.get(p1.ID)
	if got.Status != "authorised" {
		t.Fatalf("rejected webhook must not change state, got %+v", got)
	}
}
