package main

import (
	"fmt"
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
)

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
		Period:          in.Period,
		Provider:        in.Provider,
		Status:          "intent",
		RulePackVersion: eval.PackID + "@" + eval.PackVersion,
		CreatedAt:       nowRFC3339(),
		UpdatedAt:       nowRFC3339(),
	}
	// pending transfer = authorise hold on ledger 200
	ptID, err := s.lc.PendingTransfer(ledger.Transfer{
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
	capRes, err := adapter.Capture(p.PSSPRef, p.AmountKobo)
	if err != nil || capRes.Status != "captured" {
		p.Status = "failed"
		p.FailReason = capRes.Detail
		p.UpdatedAt = nowRFC3339()
		_ = s.st.Put("payments", p.ID, p)
		return p, Certificate{}, fmt.Errorf("pssp capture failed: %s", capRes.Detail)
	}
	// Saga point of no return: the PSSP has the money. Persist the
	// intermediate state first so a crash here is recoverable by a recon
	// worker, then run the ledger + certificate steps; any failure triggers
	// the compensating actions (ledger reversal + PSSP refund).
	p.Status = "captured_awaiting_post"
	p.UpdatedAt = nowRFC3339()
	if err := s.st.Put("payments", p.ID, p); err != nil {
		_ = s.compensateCapture(&p, "persist captured_awaiting_post: "+err.Error())
		return p, Certificate{}, fmt.Errorf("persist capture state: %w", err)
	}
	postID, err := s.lc.PostPending(p.PendingTransferID, p.AmountKobo)
	if err != nil {
		comp := s.compensateCapture(&p, "post pending transfer: "+err.Error())
		return p, Certificate{}, fmt.Errorf("post pending transfer: %w (compensation: %s)", err, comp)
	}
	p.PostTransferID = postID
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
func (s *PaymentService) compensateCapture(p *Payment, cause string) string {
	comp := Compensation{
		ID:        ids.WithPrefix("cmp"),
		PaymentID: p.ID,
		Cause:     cause,
		CreatedAt: nowRFC3339(),
	}
	// 1) ledger reversal (only if the pending transfer was posted)
	if p.PostTransferID != "" {
		if payer, err := s.payerAccountID(p.TINHash); err == nil {
			if collections, err := s.collectionsAccountID(); err == nil {
				revID, err := s.lc.Transfer(ledger.Transfer{
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

// HandleWebhook applies a PSSP webhook callback (authorised|captured|failed).
func (s *PaymentService) HandleWebhook(provider string, payload struct {
	Reference string `json:"reference"`
	Event     string `json:"event"` // authorisation.successful|charge.successful|charge.failed|authorisation.voided
	PaymentID string `json:"payment_id"`
}) (Payment, error) {
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
	switch payload.Event {
	case "charge.successful":
		p, _, err := s.Capture(target.ID)
		return p, err
	case "charge.failed", "authorisation.voided":
		return s.Void(target.ID)
	case "authorisation.successful":
		target.Status = "authorised"
		target.UpdatedAt = nowRFC3339()
		return target, s.st.Put("payments", target.ID, target)
	default:
		return target, fmt.Errorf("unknown webhook event %q", payload.Event)
	}
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
