package main

import (
	"log"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
)

// recovery.go — the capture-saga recovery worker promised by the saga
// comment in payments.go (audit Flow 1: "the recon worker referenced in the
// comment does not exist"). It closes the crash windows in the capture
// saga:
//
//   captured_awaiting_post  -> resume: idempotently re-post the pending
//                              transfer (PostPendingAs with the persisted
//                              deterministic post id), post the fee leg,
//                              issue the certificate, mark captured.
//                              If the post leg fails permanently the saga
//                              compensates (ledger reversal only if the post
//                              actually landed + PSSP refund).
//   pending_authorisation / intent older than intentTTL
//                           -> expire: void the pending hold so abandoned
//                              intents do not hold funds forever.
//
// The sweeper runs once at boot and then on an interval (StartRecovery).

const (
	// intentTTL is how long an unauthorised intent may hold a pending
	// transfer before the sweeper voids it.
	intentTTL = 30 * time.Minute
	// recoveryInterval is the sweep cadence in the background worker.
	recoveryInterval = 60 * time.Second
)

// RecoverySweeper resumes or compensates interrupted capture sagas.
type RecoverySweeper struct {
	pay *PaymentService
	st  interface {
		List(coll string, out any) error
	}
	lc  ledger.Client
	bus events.Bus
}

func NewRecoverySweeper(pay *PaymentService, lc ledger.Client, bus events.Bus) *RecoverySweeper {
	return &RecoverySweeper{pay: pay, st: pay.st, lc: lc, bus: bus}
}

// SweepOnce performs one recovery pass. Returns counts for observability.
func (r *RecoverySweeper) SweepOnce() (resumed, compensated, expired int, err error) {
	var payments []Payment
	if err := r.st.List("payments", &payments); err != nil {
		return 0, 0, 0, err
	}
	for _, p := range payments {
		switch p.Status {
		case "captured_awaiting_post":
			switch r.resumeCapture(&p) {
			case "resumed":
				resumed++
			case "compensated":
				compensated++
			}
		case "intent", "pending_authorisation":
			if r.expireIntent(&p) {
				expired++
			}
		}
	}
	return resumed, compensated, expired, nil
}

// resumeCapture finishes (or compensates) one interrupted capture saga.
// Every step is idempotent so the sweep can run any number of times.
func (r *RecoverySweeper) resumeCapture(p *Payment) string {
	if p.PostTransferID == "" {
		// Legacy record from before the single-durable-write fix: we cannot
		// know whether a post happened. Compensate WITHOUT a ledger reversal
		// (PostTransferID empty -> compensateCapture skips reversal) and
		// refund the payer.
		r.pay.compensateCapture(p, "recovery: legacy record without post transfer id")
		return "compensated"
	}
	if _, err := r.lc.PostPendingAs(p.PendingTransferID, p.PostTransferID, p.AmountKobo); err != nil {
		// If the pending is already consumed and our post id exists this is
		// a replay (handled inside PostPendingAs); any other error means the
		// post genuinely failed -> compensate.
		log.Printf("recovery: payment %s post failed (%v); compensating", p.ID, err)
		r.pay.compensateCapture(p, "recovery: post pending transfer: "+err.Error())
		return "compensated"
	}
	if err := r.pay.postFeeLeg(p); err != nil {
		r.pay.compensateCapture(p, "recovery: post fee leg: "+err.Error())
		return "compensated"
	}
	cert, err := r.pay.certs.Issue(*p)
	if err != nil {
		r.pay.compensateCapture(p, "recovery: issue certificate: "+err.Error())
		return "compensated"
	}
	p.CertificateSerial = cert.Serial
	p.Status = "captured"
	p.UpdatedAt = nowRFC3339()
	if err := r.pay.st.Put("payments", p.ID, *p); err != nil {
		return "error"
	}
	r.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "captured", "recovered": true,
		"amount_kobo": p.AmountKobo, "tin_hash": p.TINHash, "certificate_serial": cert.Serial,
	}))
	return "resumed"
}

// expireIntent voids the pending hold of an abandoned intent.
func (r *RecoverySweeper) expireIntent(p *Payment) bool {
	ts, err := time.Parse(time.RFC3339, p.UpdatedAt)
	if err != nil || time.Since(ts) < intentTTL {
		return false
	}
	if p.PendingTransferID != "" {
		if t, err := r.lc.LookupTransfer(p.PendingTransferID); err == nil && t.Pending {
			if _, err := r.lc.VoidPending(p.PendingTransferID); err != nil {
				log.Printf("recovery: expire payment %s: void pending: %v", p.ID, err)
				return false
			}
		}
	}
	p.Status = "expired"
	p.FailReason = "pending authorisation expired (ttl " + intentTTL.String() + ")"
	p.UpdatedAt = nowRFC3339()
	if err := r.pay.st.Put("payments", p.ID, *p); err != nil {
		return false
	}
	r.bus.Publish("nrs.psm.payments.v1", events.New("nrs.psm.payments.v1", serviceName, "", p.RulePackVersion, map[string]any{
		"payment_id": p.ID, "status": "expired", "tin_hash": p.TINHash,
	}))
	return true
}

// StartRecovery runs a sweep immediately (boot recovery) and then every
// interval until stop is closed. Called once from main.
func (r *RecoverySweeper) StartRecovery(stop <-chan struct{}) {
	resumed, compensated, expired, err := r.SweepOnce()
	log.Printf("recovery: boot sweep resumed=%d compensated=%d expired=%d err=%v", resumed, compensated, expired, err)
	go func() {
		tick := time.NewTicker(recoveryInterval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if resumed, compensated, expired, err := r.SweepOnce(); err != nil {
					log.Printf("recovery: sweep error: %v", err)
				} else if resumed+compensated+expired > 0 {
					log.Printf("recovery: sweep resumed=%d compensated=%d expired=%d", resumed, compensated, expired)
				}
			}
		}
	}()
}
