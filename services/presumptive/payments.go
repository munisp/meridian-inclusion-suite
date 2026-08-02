package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// §1.5 namespaces for psm payment accounts (ledger 200).
const (
	nsPSMPayer       uint64 = 200000100000 // payer (operator) holding accounts
	nsPSMCollections uint64 = 200000000001 // NRS presumptive collections account
	nsPSPFeeIncome   uint64 = 200000000002 // PSSP fee income (fee leg, audit fix #4)
)

// idempotencyTTL bounds how long an idempotency-key replay window stays open.
const idempotencyTTL = 24 * time.Hour

// IdempotencyRecord binds a caller-supplied Idempotency-Key to the payment
// it created, so client retries replay the original response instead of
// creating a second payment + second pending hold.
type IdempotencyRecord struct {
	Key       string `json:"key"`
	PaymentID string `json:"payment_id"`
	CreatedAt string `json:"created_at"`
}

// lookupIdempotency returns the payment id previously created for key, if
// the record exists and is younger than idempotencyTTL.
func (s *PaymentService) lookupIdempotency(key string) (string, bool) {
	var rec IdempotencyRecord
	ok, err := s.st.Get("idempotency", key, &rec)
	if err != nil || !ok {
		return "", false
	}
	if ts, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil && time.Since(ts) > idempotencyTTL {
		return "", false // expired: a fresh attempt is allowed
	}
	return rec.PaymentID, true
}

// PaymentService runs the payment lifecycle:
// intent -> pending transfer (ledger 200, code 1 authorise) -> PSSP authorise
// -> capture (post pending, code 2) / void (code 3) -> certificate.
type PaymentService struct {
	st     *store.Store
	lc     ledger.Client
	hub    *PSSPHub
	engine *BandEngine
	gates  *GateClient
	certs  *CertificateService
	bus    events.Bus
}

func NewPaymentService(st *store.Store, lc ledger.Client, hub *PSSPHub, eng *BandEngine, gates *GateClient, certs *CertificateService, bus events.Bus) *PaymentService {
	return &PaymentService{st: st, lc: lc, hub: hub, engine: eng, gates: gates, certs: certs, bus: bus}
}

func (s *PaymentService) collectionsAccountID() (string, error) {
	id := ledger.AccountID(nsPSMCollections, 1)
	if _, err := s.lc.Balance(id); err == nil {
		return id, nil
	}
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerPSMPayments, Code: 2, UserData: "nrs-psm-collections",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

func (s *PaymentService) payerAccountID(tinHash string) (string, error) {
	id := ledger.AccountID(nsPSMPayer, agentSerial(tinHash))
	if _, err := s.lc.Balance(id); err == nil {
		return id, nil
	}
	meta := tinHash
	if len(meta) > 16 {
		meta = meta[:16]
	}
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerPSMPayments, Code: 1, UserData: "payer:" + meta,
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// IntentRequest starts a payment.
type IntentRequest struct {
	TINHash            string `json:"tin_hash"`
	State              string `json:"state"`
	TradeCategory      string `json:"trade_category"`
	AnnualTurnoverKobo uint64 `json:"annual_turnover_kobo"`
	Period             string `json:"period"`   // e.g. "2026"
	Provider           string `json:"provider"` // remita|etranzact|flutterwave
	Monthly            bool   `json:"monthly"`  // pay monthly instalment instead of annual
	// IdempotencyKey dedupes client retries (Idempotency-Key header):
	// a replayed key returns the originally created payment (200 replay),
	// never a second pending hold.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// CreateIntent enforces the presumptive gate, evaluates the band engine and
// creates the payment + pending transfer (ledger 200).
func (s *PaymentService) CreateIntent(in IntentRequest) (Payment, error) {
	open, err := s.gates.CollectionsOpen()
	if err != nil {
		return Payment{}, fmt.Errorf("gate check: %w", err)
	}
	if !open {
		return Payment{}, ErrGateClosed
	}
	if in.TINHash == "" || in.State == "" {
		return Payment{}, fmt.Errorf("tin_hash and state are required")
	}
	if in.IdempotencyKey != "" {
		if pid, ok := s.lookupIdempotency(in.IdempotencyKey); ok {
			p, found, err := s.get(pid)
			if err == nil && found {
				return p, nil // idempotent replay (200)
			}
		}
	}
	if _, err := s.hub.Adapter(in.Provider); err != nil {
		return Payment{}, err
	}
	if in.Period == "" {
		in.Period = fmt.Sprint(timeNowYear())
	}
	eval := s.engine.Evaluate(in.State, in.TradeCategory, in.AnnualTurnoverKobo, false, 0)
	if eval.Exempt {
		return Payment{}, fmt.Errorf("operator is exempt from presumptive levy: %s", eval.ExemptReason)
	}
	if eval.Graduate {
		return Payment{}, fmt.Errorf("turnover above presumptive ceiling: route to standard regime (MBS)")
	}
	amount := eval.AnnualLevyKobo
	if in.Monthly {
		amount = eval.MonthlyLevyKobo
	}
	amount += eval.AdminFeeKobo
	if amount == 0 {
		return Payment{}, fmt.Errorf("computed levy is zero; nothing to collect")
	}

	payer, err := s.payerAccountID(in.TINHash)
	if err != nil {
		return Payment{}, err
	}
	collections, err := s.collectionsAccountID()
	if err != nil {
		return Payment{}, err
	}
	p := Payment{
		ID:              ids.WithPrefix("pay"),
		TINHash:         in.TINHash,
		State:           in.State,
		TradeCategory:   in.TradeCategory,
		TurnoverBand:    eval.Band,
		AmountKobo:      amount,
		Currency:        "NGN",
		Period:          in.Period,
		Provider:        in.Provider,
		Status:          "intent",
		RulePackVersion: eval.PackID + "@" + eval.PackVersion,
		CreatedAt:       nowRFC3339(),
		UpdatedAt:       nowRFC3339(),
	}
	// pending transfer = authorise hold on ledger 200. Deterministic id:
	// a retried PendingTransfer with the same id replays instead of
	// creating a second hold (ledger dedup on client-supplied ids).
	ptID, err := s.lc.PendingTransfer(ledger.Transfer{
		ID:              ledger.DeterministicTransferID("psm-intent:" + p.ID),
		DebitAccountID:  payer,
		CreditAccountID: collections,
		Ledger:          ledger.LedgerPSMPayments,
		Code:            ledger.CodeAuthorise,
		Amount:          amount,
		UserData:        "psm-intent:" + p.ID,
	})
	if err != nil {
		return Payment{}, fmt.Errorf("pending transfer: %w", err)
	}
	p.PendingTransferID = ptID
	p.Status = "pending_authorisation"
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return Payment{}, err
	}
	if in.IdempotencyKey != "" {
		_ = s.st.Put("idempotency", in.IdempotencyKey, IdempotencyRecord{
			Key: in.IdempotencyKey, PaymentID: p.ID, CreatedAt: nowRFC3339(),
		})
	}
	s.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": p.Status, "amount_kobo": p.AmountKobo, "tin_hash": p.TINHash, "band": p.TurnoverBand,
	}))
	return p, nil
}

func timeNowYear() int {
	return time.Now().UTC().Year()
}

// Authorise runs PSSP authorisation for a payment intent.
func (s *PaymentService) Authorise(paymentID string) (Payment, AuthoriseResponse, error) {
	p, ok, err := s.get(paymentID)
	if err != nil || !ok {
		return Payment{}, AuthoriseResponse{}, fmt.Errorf("payment %s not found", paymentID)
	}
	if p.Status != "pending_authorisation" {
		return Payment{}, AuthoriseResponse{}, fmt.Errorf("payment %s is %s; cannot authorise", paymentID, p.Status)
	}
	adapter, _ := s.hub.Adapter(p.Provider)
	res, err := adapter.Authorise(AuthoriseRequest{
		PaymentID:  p.ID,
		AmountKobo: p.AmountKobo,
		PayerRef:   p.TINHash,
		Narration:  fmt.Sprintf("Presumptive levy %s (%s band, %s)", p.Period, p.TurnoverBand, p.State),
	})
	if err != nil {
		return p, res, err
	}
	p.PSSPRef = res.Reference
	if res.Status == "authorised" {
		p.Status = "authorised"
	} else {
		p.Status = "failed"
		p.FailReason = res.Detail
		// release the pending hold
		_, _ = s.lc.VoidPending(p.PendingTransferID)
	}
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return p, res, err
	}
	return p, res, nil
}

// Capture captures an authorised payment: PSSP capture + post pending ledger
// transfer + certificate issuance. SAGA (audit fix #5): after the PSSP
// capture succeeds the saga persists "captured_awaiting_post"; failures in
// the ledger/certificate legs run compensating actions (ledger reversal +
// PSSP refund) instead of leaving the payer charged without a certificate.
func (s *PaymentService) Capture(paymentID string) (Payment, Certificate, error) {
	p, ok, err := s.get(paymentID)
	if err != nil || !ok {
		return Payment{}, Certificate{}, fmt.Errorf("payment %s not found", paymentID)
	}
	if p.Status != "authorised" {
		return Payment{}, Certificate{}, fmt.Errorf("payment %s is %s; cannot capture", paymentID, p.Status)
	}
	adapter, _ := s.hub.Adapter(p.Provider)
	// Idempotency-Key on the PSSP capture: the payment id scopes the key so
	// a retried capture (crash between PSSP capture and state persist) is a
	// replay at the provider, not a second charge.
	capRes, err := adapter.Capture(p.PSSPRef, p.AmountKobo, "capture:"+p.ID)
	if err != nil {
		// Transport error (audit funds-flow #4): the provider may or may not
		// have captured. Do NOT mark failed blind — verify server-side first
		// (mirrors the NIP TSQ pattern).
		vr, verr := adapter.Verify(p.PSSPRef)
		switch {
		case verr == nil && vr.Status == "captured":
			// The provider DID capture despite the transport error: continue
			// the saga with the verified result.
			capRes = vr
		case verr == nil:
			// Confirmed NOT captured: safe to fail terminally and void the
			// hold — no money moved at the provider.
			s.failCaptureConfirmed(&p, "capture rejected at provider (verified): "+vr.Detail)
			return p, Certificate{}, fmt.Errorf("pssp capture failed: %s", vr.Detail)
		default:
			// Indeterminate: leave in a swept state. The recovery sweeper
			// re-verifies and either resumes the capture or fails+voids.
			p.Status = "capture_in_flight"
			p.FailReason = "capture transport error, provider state indeterminate: " + err.Error()
			p.UpdatedAt = nowRFC3339()
			_ = s.st.Put("payments", p.ID, p)
			return p, Certificate{}, fmt.Errorf("pssp capture indeterminate (sweeper will resolve): %w", err)
		}
	} else if capRes.Status != "captured" {
		// Definitive decline from the provider: fail and void the hold.
		s.failCaptureConfirmed(&p, capRes.Detail)
		return p, Certificate{}, fmt.Errorf("pssp capture failed: %s", capRes.Detail)
	}
	// Saga point of no return: the PSSP has the money. Persist the
	// intermediate state AND the deterministic post/fee transfer ids in ONE
	// durable write — a crash after the ledger post is then fully resumable
	// by the recovery sweeper (which re-runs PostPendingAs idempotently),
	// and compensation always knows the post transfer id.
	p.Status = "captured_awaiting_post"
	p.PostTransferID = ledger.DeterministicTransferID("psm-post:" + p.ID)
	p.FeeKobo = p.AmountKobo - capRes.SettledKobo
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		_ = s.compensateCapture(&p, "persist captured_awaiting_post: "+err.Error())
		return p, Certificate{}, fmt.Errorf("persist capture state: %w", err)
	}
	if _, err := s.lc.PostPendingAs(p.PendingTransferID, p.PostTransferID, p.AmountKobo); err != nil {
		comp := s.compensateCapture(&p, "post pending transfer: "+err.Error())
		return p, Certificate{}, fmt.Errorf("post pending transfer: %w (compensation: %s)", err, comp)
	}
	// Fee leg (audit fix #4): the PSSP settles amount-fee; post the fee from
	// collections to the dedicated PSSP fee-income account so 3-way recon
	// balances by construction. Deterministic id -> retry-safe.
	if err := s.postFeeLeg(&p); err != nil {
		comp := s.compensateCapture(&p, "post fee leg: "+err.Error())
		return p, Certificate{}, fmt.Errorf("post fee leg: %w (compensation: %s)", err, comp)
	}
	cert, err := s.certs.Issue(p)
	if err != nil {
		comp := s.compensateCapture(&p, "issue certificate: "+err.Error())
		return p, Certificate{}, fmt.Errorf("issue certificate: %w (compensation: %s)", err, comp)
	}
	p.CertificateSerial = cert.Serial
	p.Status = "captured"
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return p, cert, err
	}
	s.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "captured", "amount_kobo": p.AmountKobo, "tin_hash": p.TINHash,
		"certificate_serial": cert.Serial, "pssp_ref": p.PSSPRef,
	}))
	return p, cert, nil
}

// Compensation is the durable record of a saga compensation so operations
// and the recon worker can audit/repair post-capture failures.
type Compensation struct {
	ID             string `json:"id"`
	PaymentID      string `json:"payment_id"`
	Cause          string `json:"cause"`
	LedgerReversal string `json:"ledger_reversal,omitempty"` // reversal transfer id
	PSSPRefund     string `json:"pssp_refund"`               // ok|failed:<err>|skipped
	CreatedAt      string `json:"created_at"`
}

// compensateCapture runs the saga compensating actions when a downstream
// step fails AFTER the PSSP capture succeeded: (1) reverse any posted ledger
// transfer back to the payer, (2) refund (fallback: void) the PSSP capture,
// (3) persist a Compensation record and mark the payment "compensated".
// Returns a human-readable summary of the compensation outcome.
// failCaptureConfirmed terminally fails a payment whose capture is
// CONFIRMED not to have happened at the provider, voiding the pending hold
// so the payer's funds are released (audit funds-flow #4). Only call this
// when the provider state is known — indeterminate captures go to
// capture_in_flight for the sweeper.
func (s *PaymentService) failCaptureConfirmed(p *Payment, reason string) {
	if p.PendingTransferID != "" {
		if t, err := s.lc.LookupTransfer(p.PendingTransferID); err == nil && t.Pending {
			if _, err := s.lc.VoidPending(p.PendingTransferID); err != nil {
				log.Printf("capture fail %s: void hold: %v (sweeper retries)", p.ID, err)
			}
		}
	}
	p.Status = "failed"
	p.FailReason = reason
	p.UpdatedAt = nowRFC3339()
	_ = s.st.Put("payments", p.ID, *p)
	s.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "failed", "fail_reason": reason, "tin_hash": p.TINHash,
	}))
}

func (s *PaymentService) compensateCapture(p *Payment, cause string) string {
	comp := Compensation{
		ID:        ids.WithPrefix("cmp"),
		PaymentID: p.ID,
		Cause:     cause,
		CreatedAt: nowRFC3339(),
	}
	// 1) ledger reversal — only if the post actually landed on the ledger.
	// PostTransferID is persisted BEFORE PostPending runs (single durable
	// write), so its presence is not proof of posting; verify with a lookup
	// so a failed/crashed post is not "reversed" into free money for the
	// payer. The reversal itself uses a deterministic id (retry-safe).
	if p.PostTransferID != "" {
		if post, err := s.lc.LookupTransfer(p.PostTransferID); err == nil && !post.Pending {
			if payer, err := s.payerAccountID(p.TINHash); err == nil {
				if collections, err := s.collectionsAccountID(); err == nil {
					revID, err := s.lc.Transfer(ledger.Transfer{
						ID:              ledger.DeterministicTransferID("psm-compensation:" + p.ID),
						DebitAccountID:  collections,
						CreditAccountID: payer,
						Ledger:          ledger.LedgerPSMPayments,
						Code:            ledger.CodeVoid,
						Amount:          p.AmountKobo,
						UserData:        "psm-compensation:" + p.ID,
					})
					if err == nil {
						comp.LedgerReversal = revID
					}
				}
			}
		}
	}
	// 2) PSSP refund (fallback: void)
	if adapter, err := s.hub.Adapter(p.Provider); err == nil && p.PSSPRef != "" {
		if err := adapter.Refund(p.PSSPRef, p.AmountKobo); err != nil {
			if verr := adapter.Void(p.PSSPRef); verr != nil {
				comp.PSSPRefund = "failed: " + err.Error()
			} else {
				comp.PSSPRefund = "ok (void fallback)"
			}
		} else {
			comp.PSSPRefund = "ok"
		}
	} else {
		comp.PSSPRefund = "skipped: no pssp reference"
	}
	// 3) durable record + status
	_ = s.st.Put("compensations", comp.ID, comp)
	p.Status = "compensated"
	p.FailReason = "capture saga compensated: " + cause
	p.UpdatedAt = nowRFC3339()
	_ = s.st.Put("payments", p.ID, *p)
	s.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "compensated", "amount_kobo": p.AmountKobo, "tin_hash": p.TINHash,
		"compensation_id": comp.ID, "cause": cause, "pssp_refund": comp.PSSPRefund,
	}))
	return fmt.Sprintf("compensation %s (ledger_reversal=%s pssp_refund=%s)", comp.ID, comp.LedgerReversal, comp.PSSPRefund)
}

// feeAccountID returns (creating if needed) the PSSP fee-income account on
// ledger 200. Fee credits land here so ledger-vs-treasury recon nets out.
func (s *PaymentService) feeAccountID() (string, error) {
	id := ledger.AccountID(nsPSPFeeIncome, 1)
	if _, err := s.lc.Balance(id); err == nil {
		return id, nil
	}
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerPSMPayments, Code: 2, UserData: "nrs-pssp-fee-income",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// postFeeLeg moves the PSSP fee (gross amount minus settled amount) from
// collections to the fee-income account. Idempotent via deterministic id.
func (s *PaymentService) postFeeLeg(p *Payment) error {
	if p.FeeKobo == 0 {
		return nil
	}
	collections, err := s.collectionsAccountID()
	if err != nil {
		return err
	}
	feeAcct, err := s.feeAccountID()
	if err != nil {
		return err
	}
	_, err = s.lc.Transfer(ledger.Transfer{
		ID:              ledger.DeterministicTransferID("psm-fee:" + p.ID),
		DebitAccountID:  collections,
		CreditAccountID: feeAcct,
		Ledger:          ledger.LedgerPSMPayments,
		Code:            ledger.CodeSettle,
		Amount:          p.FeeKobo,
		UserData:        "psm-fee:" + p.ID,
	})
	return err
}

// Void voids a payment before capture (PSSP void + ledger void, code 3).
func (s *PaymentService) Void(paymentID string) (Payment, error) {
	p, ok, err := s.get(paymentID)
	if err != nil || !ok {
		return Payment{}, fmt.Errorf("payment %s not found", paymentID)
	}
	if p.Status == "captured" {
		return Payment{}, fmt.Errorf("payment %s already captured; use refund flow", paymentID)
	}
	if p.Status == "voided" {
		return p, nil // idempotent
	}
	if p.PSSPRef != "" {
		if adapter, err := s.hub.Adapter(p.Provider); err == nil {
			_ = adapter.Void(p.PSSPRef) // best-effort; ledger void is authoritative
		}
	}
	if p.PendingTransferID != "" {
		if _, err := s.lc.VoidPending(p.PendingTransferID); err != nil {
			return Payment{}, fmt.Errorf("void pending transfer: %w", err)
		}
	}
	p.Status = "voided"
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		return Payment{}, err
	}
	s.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "voided", "tin_hash": p.TINHash,
	}))
	return p, nil
}

// WebhookPayload is a PSSP webhook callback. AmountKobo/Currency are echoed
// by real providers (Paystack data.amount, Flutterwave data.amount/currency)
// and are verified against the stored intent before any value is given (G1).
type WebhookPayload struct {
	Reference  string `json:"reference"`
	Event      string `json:"event"` // authorisation.successful|charge.successful|charge.failed|authorisation.voided|charge.dispute.create|charge.dispute.resolve
	PaymentID  string `json:"payment_id"`
	AmountKobo uint64 `json:"amount_kobo,omitempty"`
	Currency   string `json:"currency,omitempty"`
	Reason     string `json:"reason,omitempty"`  // dispute reason (charge.dispute.create)
	Outcome    string `json:"outcome,omitempty"` // won|lost (charge.dispute.resolve)
}

// ErrWebhookMismatch marks a webhook whose amount/currency disagrees with the
// stored intent or with the provider's own verify response. Handlers map it
// to HTTP 409 and NO state change is applied (G1).
var ErrWebhookMismatch = fmt.Errorf("webhook payload does not match stored intent")

// paymentRank orders the monotonic payment state machine
// (authorised < captured < settled). Terminal/side states (voided, failed,
// compensated, disputed, charged_back) are out of the monotonic chain and
// handled by explicit transitions only (G2).
func paymentRank(status string) (int, bool) {
	switch status {
	case "intent":
		return 0, true
	case "pending_authorisation":
		return 1, true
	case "authorised":
		return 2, true
	case "captured_awaiting_post":
		return 3, true
	case "captured":
		return 4, true
	case "settled":
		return 5, true
	}
	return -1, false
}

// ProcessedWebhookEvent is the persisted dedup record for a delivered PSSP
// webhook. The id is deterministic — provider:reference:event — so a
// redelivery (Paystack retries for up to ~72h) hits the same record and is
// acked as an idempotent no-op instead of re-running the transition (G2).
type ProcessedWebhookEvent struct {
	ID          string `json:"id"` // provider:reference:event
	PaymentID   string `json:"payment_id"`
	Event       string `json:"event"`
	Outcome     string `json:"outcome"` // applied|parked (parked = out-of-order, acked without state change)
	ProcessedAt string `json:"processed_at"`
}

func webhookEventID(provider, reference, event string) string {
	return provider + ":" + reference + ":" + event
}

// HandleWebhook applies a PSSP webhook callback. Guarantees (G1/G2):
//   - amount/currency in the payload AND the provider's own Verify response
//     are checked against the stored intent before capture/certificate;
//     a mismatch is rejected (409 + alert event) with no state change;
//   - deliveries are deduped on (provider, reference, event): a redelivery
//     of an applied event is a 200 idempotent no-op;
//   - the state machine is monotonic: an out-of-order event that would
//     regress state (e.g. authorisation.successful after capture) is parked
//     and acked without touching the payment.
func (s *PaymentService) HandleWebhook(provider string, payload WebhookPayload) (Payment, error) {
	var target Payment
	found := false
	if payload.PaymentID != "" {
		p, ok, err := s.get(payload.PaymentID)
		if err == nil && ok {
			target, found = p, true
		}
	}
	if !found {
		// resolve by pssp ref
		var all []Payment
		if err := s.st.List("payments", &all); err != nil {
			return Payment{}, err
		}
		for _, p := range all {
			if p.PSSPRef == payload.Reference && p.Provider == provider {
				target, found = p, true
				break
			}
		}
	}
	if !found {
		return Payment{}, fmt.Errorf("no payment matches webhook reference %s", payload.Reference)
	}
	// Dedup (G2): a previously applied/parked delivery of this exact event is
	// acked as a no-op — providers must get 200 so they stop retrying.
	eventID := webhookEventID(provider, payload.Reference, payload.Event)
	var seen ProcessedWebhookEvent
	if ok, err := s.st.Get("webhook_events", eventID, &seen); err == nil && ok {
		return target, nil // idempotent redelivery: 200 no-op
	}
	markProcessed := func(outcome string) {
		_ = s.st.Put("webhook_events", eventID, ProcessedWebhookEvent{
			ID: eventID, PaymentID: target.ID, Event: payload.Event, Outcome: outcome, ProcessedAt: nowRFC3339(),
		})
	}
	switch payload.Event {
	case "charge.successful":
		// Already captured by an earlier delivery/flow: idempotent ack.
		if r, ok := paymentRank(target.Status); ok && r >= 3 {
			markProcessed("parked")
			return target, nil
		}
		if err := s.verifyWebhookValue(provider, target, payload); err != nil {
			return target, err // 409 + alert; no state change
		}
		p, _, err := s.Capture(target.ID)
		if err != nil {
			return p, err
		}
		markProcessed("applied")
		return p, nil
	case "charge.failed", "authorisation.voided":
		p, err := s.Void(target.ID)
		if err != nil {
			return p, err
		}
		markProcessed("applied")
		return p, nil
	case "authorisation.successful":
		// Monotonic guard (G2): a late/duplicate authorisation must never
		// downgrade a captured/settled payment back to authorised.
		if r, ok := paymentRank(target.Status); !ok || r >= 3 {
			markProcessed("parked")
			return target, nil // out-of-order: ack, no regression
		}
		if target.Status == "authorised" {
			markProcessed("parked")
			return target, nil // duplicate: no-op
		}
		target.Status = "authorised"
		target.UpdatedAt = nowRFC3339()
		if err := s.st.Put("payments", target.ID, target); err != nil {
			return target, err
		}
		markProcessed("applied")
		return target, nil
	case "charge.dispute.create":
		d, err := s.OpenDispute(target, payload.Reason)
		if err != nil {
			return target, err
		}
		markProcessed("applied")
		_ = d
		p, _, _ := s.get(target.ID)
		return p, nil
	case "charge.dispute.resolve":
		if err := s.ResolveDispute(target, payload.Outcome); err != nil {
			return target, err
		}
		markProcessed("applied")
		p, _, _ := s.get(target.ID)
		return p, nil
	default:
		return target, fmt.Errorf("unknown webhook event %q", payload.Event)
	}
}

// verifyWebhookValue (G1) defends against forged/underpaid/misconfigured
// webhooks before value is given: (1) any amount/currency echoed in the
// payload must equal the stored intent; (2) the provider's own Verify
// endpoint must confirm the transaction for the stored amount/currency.
// On any disagreement it publishes an alert and returns ErrWebhookMismatch —
// the handler answers 409 and the payment is untouched.
func (s *PaymentService) verifyWebhookValue(provider string, p Payment, payload WebhookPayload) error {
	mismatch := func(format string, args ...any) error {
		detail := fmt.Sprintf(format, args...)
		s.bus.Publish("nrs.payments.alerts.v1", events.New("nrs.payments.alerts.v1", serviceName, "", p.RulePackVersion, map[string]any{
			"alert": "webhook_value_mismatch", "payment_id": p.ID, "provider": provider,
			"reference": payload.Reference, "detail": detail,
		}))
		return fmt.Errorf("%w: %s", ErrWebhookMismatch, detail)
	}
	if payload.AmountKobo != 0 && payload.AmountKobo != p.AmountKobo {
		return mismatch("payload amount %d kobo != intent amount %d kobo", payload.AmountKobo, p.AmountKobo)
	}
	if payload.Currency != "" && !strings.EqualFold(payload.Currency, p.Currency) {
		return mismatch("payload currency %q != intent currency %q", payload.Currency, p.Currency)
	}
	// Re-verify with the provider server-side (Paystack/Flutterwave both
	// mandate verify-before-fulfil). A verify failure fails closed: the
	// webhook is rejected (provider will retry) and nothing is captured.
	adapter, err := s.hub.Adapter(provider)
	if err != nil {
		return fmt.Errorf("webhook verify: %w", err)
	}
	vr, err := adapter.Verify(payload.Reference)
	if err != nil {
		return fmt.Errorf("webhook verify: provider verification unavailable: %w", err)
	}
	if vr.Status == "failed" {
		return mismatch("provider verify: reference %s not successful (%s)", payload.Reference, vr.Detail)
	}
	if vr.AmountKobo != 0 && vr.AmountKobo != p.AmountKobo {
		return mismatch("provider verify amount %d kobo != intent amount %d kobo", vr.AmountKobo, p.AmountKobo)
	}
	if vr.Currency != "" && !strings.EqualFold(vr.Currency, p.Currency) {
		return mismatch("provider verify currency %q != intent currency %q", vr.Currency, p.Currency)
	}
	return nil
}

func (s *PaymentService) get(id string) (Payment, bool, error) {
	var p Payment
	ok, err := s.st.Get("payments", id, &p)
	return p, ok, err
}

func (s *PaymentService) List() ([]Payment, error) {
	var all []Payment
	if err := s.st.List("payments", &all); err != nil {
		return nil, err
	}
	return all, nil
}
