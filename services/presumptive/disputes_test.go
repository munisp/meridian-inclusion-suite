package main

import (
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
)

// capturedPayment runs the full intent -> authorise -> capture flow.
func capturedPayment(t *testing.T, ts *testStack, tin string) Payment {
	t.Helper()
	p := authorisedPayment(t, ts, tin)
	p, _, err := ts.pay.Capture(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "captured" {
		t.Fatalf("expected captured, got %s", p.Status)
	}
	return p
}

func disputeByID(t *testing.T, ts *testStack, id string) Dispute {
	t.Helper()
	var d Dispute
	ok, err := ts.st.Get("disputes", id, &d)
	if err != nil || !ok {
		t.Fatalf("dispute %s not found: %v", id, err)
	}
	return d
}

// TestDisputeOpensHold (G11): charge.dispute.create opens a dispute record,
// holds the disputed amount on the ledger, marks the payment disputed, sets
// the CBN 72h resolution clock and publishes the nrs.payments.dispute.v1
// alert.
func TestDisputeOpensHold(t *testing.T) {
	ts := newTestStack(t)
	p := capturedPayment(t, ts, "dsp-open")
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{
		Reference: p.PSSPRef, Event: "charge.dispute.create", Reason: "unrecognised transaction",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "disputed" || got.DisputeID == "" {
		t.Fatalf("payment must be disputed, got %+v", got)
	}
	d := disputeByID(t, ts, got.DisputeID)
	if d.Status != "open" || d.AmountKobo != p.AmountKobo || d.Reason != "unrecognised transaction" {
		t.Fatalf("bad dispute record: %+v", d)
	}
	if d.PriorStatus != "captured" {
		t.Fatalf("prior status must be captured, got %s", d.PriorStatus)
	}
	// CBN 72h clock: resolution due ~72h after creation
	due, err := time.Parse(time.RFC3339, d.ResolutionDueAt)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(due); until < 71*time.Hour || until > 72*time.Hour {
		t.Fatalf("CBN 72h resolution clock wrong: %v", until)
	}
	// ledger hold is pending for the full disputed amount
	hold, err := ts.lc.LookupTransfer(d.HoldTransferID)
	if err != nil || !hold.Pending || hold.Amount != p.AmountKobo {
		t.Fatalf("expected pending hold of %d, got %+v err=%v", p.AmountKobo, hold, err)
	}
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.DebitsPending != p.AmountKobo {
		t.Fatalf("collections must carry the pending hold, got %+v", bal)
	}
	// auto-alert published
	evs := ts.pay.bus.(*events.InprocBus).Published("nrs.payments.dispute.v1")
	if len(evs) != 1 {
		t.Fatalf("expected 1 dispute alert, got %d", len(evs))
	}
	// duplicate create is idempotent: same dispute, no second hold
	again, err := ts.pay.HandleWebhook("remita", WebhookPayload{
		Reference: p.PSSPRef, Event: "charge.dispute.create", Reason: "unrecognised transaction",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.DisputeID != got.DisputeID {
		t.Fatalf("duplicate create must reuse the dispute, got %s", again.DisputeID)
	}
	bal, _ = ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.DebitsPending != p.AmountKobo {
		t.Fatalf("duplicate create must not double-hold, got %+v", bal)
	}
}

// TestDisputeResolveWonReleases (G11): resolving won voids the hold (funds
// released back to collections) and restores the payment to captured.
func TestDisputeResolveWonReleases(t *testing.T) {
	ts := newTestStack(t)
	p := capturedPayment(t, ts, "dsp-won")
	if _, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.dispute.create", Reason: "fraud claim"}); err != nil {
		t.Fatal(err)
	}
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.dispute.resolve", Outcome: "won"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "captured" {
		t.Fatalf("won dispute must restore captured, got %s", got.Status)
	}
	d := disputeByID(t, ts, "dsp:"+p.ID)
	if d.Status != "won" || d.ResolvedAt == "" {
		t.Fatalf("dispute must be won+resolved, got %+v", d)
	}
	// hold released: nothing pending, collections whole again
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.DebitsPending != 0 || bal.CreditsPosted != p.AmountKobo {
		t.Fatalf("hold must be released, got %+v", bal)
	}
	holdBal, err := ts.lc.Balance(ledger.AccountID(nsPSPDisputeHold, 1))
	if err != nil || holdBal.CreditsPosted != 0 {
		t.Fatalf("nothing must land in the hold account on a win, got %+v", holdBal)
	}
	// idempotent re-resolution with the same outcome
	if _, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.dispute.resolve", Outcome: "won"}); err != nil {
		t.Fatalf("re-resolve same outcome must be a no-op, got %v", err)
	}
}

// TestDisputeResolveLostReverses (G11): resolving lost posts the hold — the
// disputed amount leaves collections for the dispute-hold account — and the
// payment becomes charged_back.
func TestDisputeResolveLostReverses(t *testing.T) {
	ts := newTestStack(t)
	p := capturedPayment(t, ts, "dsp-lost")
	if _, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.dispute.create", Reason: "chargeback"}); err != nil {
		t.Fatal(err)
	}
	got, err := ts.pay.HandleWebhook("remita", WebhookPayload{Reference: p.PSSPRef, Event: "charge.dispute.resolve", Outcome: "lost"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "charged_back" {
		t.Fatalf("lost dispute must mark charged_back, got %s", got.Status)
	}
	d := disputeByID(t, ts, "dsp:"+p.ID)
	if d.Status != "lost" {
		t.Fatalf("dispute must be lost, got %+v", d)
	}
	// the chargeback debit landed: collections debited, hold account credited
	bal, _ := ts.lc.Balance(ledger.AccountID(nsPSMCollections, 1))
	if bal.DebitsPending != 0 || bal.DebitsPosted != p.FeeKobo+p.AmountKobo {
		t.Fatalf("chargeback debit must post, got %+v", bal)
	}
	holdBal, _ := ts.lc.Balance(ledger.AccountID(nsPSPDisputeHold, 1))
	if holdBal.CreditsPosted != p.AmountKobo {
		t.Fatalf("disputed amount must land in hold account, got %+v", holdBal)
	}
	// conflicting re-resolution is rejected (direct service call: the webhook
	// dedup key would ack an identical redelivery before reaching here)
	if err := ts.pay.ResolveDispute(got, "won"); err == nil {
		t.Fatal("conflicting re-resolution must be rejected")
	}
}
