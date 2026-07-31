package main

import (
	"fmt"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
)

// Dispute / chargeback model (G11). Nigerian PSSPs emit dispute lifecycle
// events (Paystack: charge.dispute.create / charge.dispute.resolve) and CBN
// mandates resolution of disputed POS/Web transactions within 72h (CBN
// revised timelines for reversals/refunds, June 2020:
// https://proshare.co/articles/cbn-revises-timelines-for-dispense-errors-refund-complaints).
//
// On dispute create the disputed amount is HELD on the ledger (pending
// transfer collections -> dispute-hold, code 6 hold) and an alert is
// published on nrs.payments.dispute.v1. Resolution is outcome-dependent:
// won -> the hold is voided (funds released back to collections, payment
// restored to its prior status); lost -> the hold is posted (the chargeback
// debit lands on the dispute-hold account) and the payment is charged_back.

// nsPSPDisputeHold is the §1.5 namespace for the disputed-funds hold account
// on ledger 200.
const nsPSPDisputeHold uint64 = 200000000003

// cbnDisputeResolutionWindow is the CBN 72h resolution clock for disputed
// POS/Web transactions; tracked on every dispute record.
const cbnDisputeResolutionWindow = 72 * time.Hour

// Dispute is a card dispute / chargeback lifecycle record.
type Dispute struct {
	ID              string `json:"id"` // deterministic: dsp:<paymentID>
	PaymentID       string `json:"payment_id"`
	Reference       string `json:"reference"` // PSSP reference
	Provider        string `json:"provider"`
	Reason          string `json:"reason"`
	AmountKobo      uint64 `json:"amount_kobo"`
	Status          string `json:"status"`            // open|won|lost
	PriorStatus     string `json:"prior_status"`      // payment status before the dispute (restore on win)
	HoldTransferID  string `json:"hold_transfer_id"`  // ledger hold (pending) transfer id
	EvidenceDueAt   string `json:"evidence_due_at"`   // opened + 24h: our evidence submission target
	ResolutionDueAt string `json:"resolution_due_at"` // opened + 72h: CBN resolution clock
	CreatedAt       string `json:"created_at"`
	ResolvedAt      string `json:"resolved_at,omitempty"`
}

// disputeHoldAccountID returns (creating if needed) the disputed-funds hold
// account on ledger 200. Holds and chargeback debits land here so the
// dispute is visible to recon instead of silently debiting settlement.
func (s *PaymentService) disputeHoldAccountID() (string, error) {
	id := ledger.AccountID(nsPSPDisputeHold, 1)
	if _, err := s.lc.Balance(id); err == nil {
		return id, nil
	}
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerPSMPayments, Code: 2, UserData: "nrs-psm-dispute-hold",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// OpenDispute opens a dispute for a captured payment: holds the disputed
// amount on the ledger, marks the payment disputed and publishes the
// nrs.payments.dispute.v1 alert. Idempotent per payment: a duplicate
// charge.dispute.create returns the already-open dispute unchanged.
func (s *PaymentService) OpenDispute(p Payment, reason string) (Dispute, error) {
	id := "dsp:" + p.ID
	var existing Dispute
	if ok, err := s.st.Get("disputes", id, &existing); err == nil && ok {
		return existing, nil // idempotent: dispute already recorded
	}
	switch p.Status {
	case "captured", "captured_awaiting_post", "settled":
	default:
		return Dispute{}, fmt.Errorf("payment %s is %s; cannot open dispute", p.ID, p.Status)
	}
	collections, err := s.collectionsAccountID()
	if err != nil {
		return Dispute{}, err
	}
	holdAcct, err := s.disputeHoldAccountID()
	if err != nil {
		return Dispute{}, err
	}
	// Funds hold: pending transfer of the disputed amount out of collections.
	// Deterministic id -> a retried open replays instead of double-holding.
	holdID, err := s.lc.PendingTransfer(ledger.Transfer{
		ID:              ledger.DeterministicTransferID("psm-dispute-hold:" + p.ID),
		DebitAccountID:  collections,
		CreditAccountID: holdAcct,
		Ledger:          ledger.LedgerPSMPayments,
		Code:            ledger.CodeHold,
		Amount:          p.AmountKobo,
		UserData:        "psm-dispute-hold:" + p.ID,
	})
	if err != nil {
		return Dispute{}, fmt.Errorf("dispute hold: %w", err)
	}
	now := time.Now().UTC()
	if reason == "" {
		reason = "unspecified"
	}
	d := Dispute{
		ID:              id,
		PaymentID:       p.ID,
		Reference:       p.PSSPRef,
		Provider:        p.Provider,
		Reason:          reason,
		AmountKobo:      p.AmountKobo,
		Status:          "open",
		PriorStatus:     p.Status,
		HoldTransferID:  holdID,
		EvidenceDueAt:   now.Add(24 * time.Hour).Format(time.RFC3339),
		ResolutionDueAt: now.Add(cbnDisputeResolutionWindow).Format(time.RFC3339),
		CreatedAt:       now.Format(time.RFC3339),
	}
	if err := s.st.Put("disputes", d.ID, d); err != nil {
		return Dispute{}, err
	}
	p.Status = "disputed"
	p.DisputeID = d.ID
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return Dispute{}, err
	}
	s.publishDisputeEvent(p, d, "opened")
	return d, nil
}

// ResolveDispute resolves an open dispute by outcome:
//   - "won":  the ledger hold is voided (funds released back to collections)
//     and the payment is restored to its pre-dispute status;
//   - "lost": the hold is posted (the chargeback debit lands on the
//     dispute-hold account) and the payment becomes charged_back.
//
// Idempotent: resolving an already-resolved dispute with the same outcome is
// a no-op; a conflicting outcome is rejected.
func (s *PaymentService) ResolveDispute(p Payment, outcome string) error {
	if outcome != "won" && outcome != "lost" {
		return fmt.Errorf("dispute outcome must be won|lost, got %q", outcome)
	}
	var d Dispute
	ok, err := s.st.Get("disputes", "dsp:"+p.ID, &d)
	if err != nil || !ok {
		return fmt.Errorf("no dispute recorded for payment %s", p.ID)
	}
	if d.Status != "open" {
		if d.Status == outcome {
			return nil // idempotent redelivery of the resolution
		}
		return fmt.Errorf("dispute %s already resolved %s; cannot re-resolve %s", d.ID, d.Status, outcome)
	}
	switch outcome {
	case "won":
		// Release the hold: collections is made whole again.
		if _, err := s.lc.VoidPending(d.HoldTransferID); err != nil {
			return fmt.Errorf("release dispute hold: %w", err)
		}
		p.Status = d.PriorStatus
	case "lost":
		// Post the hold: the disputed amount leaves collections for the
		// dispute-hold account (chargeback debit), visible to recon.
		if _, err := s.lc.PostPendingAs(d.HoldTransferID, ledger.DeterministicTransferID("psm-dispute-debit:"+p.ID), d.AmountKobo); err != nil {
			return fmt.Errorf("post dispute debit: %w", err)
		}
		p.Status = "charged_back"
	}
	d.Status = outcome
	d.ResolvedAt = nowRFC3339()
	if err := s.st.Put("disputes", d.ID, d); err != nil {
		return err
	}
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return err
	}
	s.publishDisputeEvent(p, d, "resolved_"+outcome)
	return nil
}

// publishDisputeEvent emits the auto-alert on nrs.payments.dispute.v1 so
// operations is paged the moment a dispute opens and can track the CBN 72h
// clock.
func (s *PaymentService) publishDisputeEvent(p Payment, d Dispute, action string) {
	s.bus.Publish("nrs.payments.dispute.v1", events.New("nrs.payments.dispute.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"dispute_id": d.ID, "payment_id": p.ID, "action": action, "status": d.Status,
		"amount_kobo": d.AmountKobo, "reason": d.Reason, "provider": d.Provider,
		"reference": d.Reference, "resolution_due_at": d.ResolutionDueAt,
	}))
}
