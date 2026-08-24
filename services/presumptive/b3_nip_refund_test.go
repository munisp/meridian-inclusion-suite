package main

import (
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// B3 #4 regression: NIP refunds must be bound to a successful prior payout
// (store lookup), capped at the original amount, and every payout/refund
// must post a double-entry ledger leg.
//
// Pre-fix: Refund = Payout with purpose flipped — any amount to any
// account, no source check, no ledger leg.

func nipLedgerService(t *testing.T, rail NIPRail) (*NIPService, *ledger.DevClient) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	lc := ledger.NewDevClient()
	return NewNIPService(rail, st, nil, true).WithLedger(lc), lc
}

func TestB3RefundWithoutSourceRejected(t *testing.T) {
	svc, _ := nipLedgerService(t, NewNIPSim())
	req := payoutReq("0123456789")
	if _, err := svc.Refund(req); err == nil ||
		!strings.Contains(err.Error(), "source_session_id") {
		t.Fatalf("refund without source must be rejected, got %v", err)
	}
	req.SourceSessionID = "000000000000000000000000000999"
	if _, err := svc.Refund(req); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("refund against unknown source must be rejected, got %v", err)
	}
}

func TestB3RefundCapAndLedgerLegs(t *testing.T) {
	svc, lc := nipLedgerService(t, NewNIPSim())
	src, err := svc.Payout(payoutReq("0123456789"))
	if err != nil || src.Status != NIPStatusSuccess {
		t.Fatalf("payout: %v %+v", err, src)
	}
	if src.LedgerPendingID == "" {
		t.Fatal("payout has no ledger leg")
	}
	// payout leg posted: collections debited, clearing credited
	clr := ledger.AccountID(nsNIPClearing, 1)
	bal, err := lc.Balance(clr)
	if err != nil || bal.CreditsPosted != 150000 {
		t.Fatalf("clearing balance after payout: %+v err=%v", bal, err)
	}

	// partial refund ok
	r1 := payoutReq("0123456789")
	r1.IdempotencyKey = "rf-1"
	r1.AmountKobo = 100000
	r1.SourceSessionID = src.SessionID
	if _, err := svc.Refund(r1); err != nil {
		t.Fatalf("partial refund: %v", err)
	}
	// second refund that would exceed the original amount: rejected
	r2 := payoutReq("0123456789")
	r2.IdempotencyKey = "rf-2"
	r2.AmountKobo = 60000 // 100000 + 60000 > 150000
	r2.SourceSessionID = src.SessionID
	if _, err := svc.Refund(r2); err == nil || !strings.Contains(err.Error(), "cap exceeded") {
		t.Fatalf("over-refund must be rejected, got %v", err)
	}
	// exact remainder ok
	r2.AmountKobo = 50000
	if _, err := svc.Refund(r2); err != nil {
		t.Fatalf("remainder refund: %v", err)
	}
	// anything further is over the cap
	r3 := payoutReq("0123456789")
	r3.IdempotencyKey = "rf-3"
	r3.AmountKobo = 1
	r3.SourceSessionID = src.SessionID
	if _, err := svc.Refund(r3); err == nil {
		t.Fatal("third refund beyond original amount must be rejected")
	}
	// every money movement has a posted ledger leg: 150000 payout + 150000 refunds
	bal, _ = lc.Balance(clr)
	if bal.CreditsPosted != 300000 {
		t.Fatalf("clearing credits %d; want 300000 (all legs posted)", bal.CreditsPosted)
	}
	// refund of a failed payout is rejected
	failed := payoutReq("0000000000") // sim rejects name enquiry -> blocked
	if _, err := svc.Payout(failed); err == nil {
		t.Fatal("expected name-enquiry block")
	}
}

func TestB3RefundBoundToFailedPayoutRejected(t *testing.T) {
	svc, _ := nipLedgerService(t, NewNIPSim())
	src, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	// reverse the source at the rail, then a refund must fail (not success)
	if _, err := svc.Reversal(src.SessionID, "test"); err == nil {
		t.Fatal("reversal of a successful transfer must be rejected (use refund flow)")
	}
	req := payoutReq("0123456789")
	req.IdempotencyKey = "rf-x"
	req.SourceSessionID = src.SessionID + "-nonexistent"
	if _, err := svc.Refund(req); err == nil {
		t.Fatal("refund bound to nonexistent session must be rejected")
	}
}
