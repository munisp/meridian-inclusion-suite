package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression (B3 #4 repair, V2 round): the refund dest_account was
// caller-chosen and NOT bound to the source payout. V2 refunded a victim's
// successful payout into an attacker's account (9999999999) and it
// SUCCEEDED. The refund destination is now bound to the source payout's
// destination account; a caller-supplied dest that differs is rejected.

func TestRefundDestBoundToSourcePayout(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	src, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != NIPStatusSuccess {
		t.Fatalf("source payout: %+v", src)
	}

	// Theft-shaped attack: refund the victim's payout to an attacker's account.
	attack := payoutReq("9999999999")
	attack.IdempotencyKey = "refund-theft"
	attack.SourceSessionID = src.SessionID
	if _, err := svc.Refund(attack); !errors.Is(err, ErrRefundDestMismatch) {
		t.Fatalf("refund to attacker account = %v, want ErrRefundDestMismatch", err)
	}

	// Mismatched bank code is also rejected.
	badBank := payoutReq("0123456789")
	badBank.IdempotencyKey = "refund-badbank"
	badBank.DestBankCode = "999"
	badBank.SourceSessionID = src.SessionID
	if _, err := svc.Refund(badBank); !errors.Is(err, ErrRefundDestMismatch) {
		t.Fatalf("refund with wrong bank code = %v, want ErrRefundDestMismatch", err)
	}

	// Matching dest (or omitted dest) is allowed and bound to the source.
	ok := payoutReq("0123456789")
	ok.IdempotencyKey = "refund-ok"
	ok.AmountKobo = 100
	ok.SourceSessionID = src.SessionID
	if _, err := svc.Refund(ok); err != nil {
		t.Fatalf("refund to source dest: %v", err)
	}
	omit := payoutReq("")
	omit.IdempotencyKey = "refund-omit"
	omit.SourceSessionID = src.SessionID
	omit.AmountKobo = 100
	tr, err := svc.Refund(omit)
	if err != nil {
		t.Fatalf("refund with omitted dest: %v", err)
	}
	if tr.DestAccount != src.DestAccount || tr.DestBankCode != src.DestBankCode {
		t.Fatalf("refund not bound to source dest: %+v", tr)
	}
}

func TestRefundDestMismatchHTTP400(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	src, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	h := nipHTTP{svc: svc}
	body := `{"amount_kobo":100,"dest_account":"9999999999","dest_bank_code":"058","idempotency_key":"http-theft","source_session_id":"` + src.SessionID + `"}`
	req := httptest.NewRequest("POST", "/v1/nip/refund", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.refund(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("refund to attacker dest over HTTP = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
}
