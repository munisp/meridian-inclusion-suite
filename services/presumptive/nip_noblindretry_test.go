package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// B3 #18 regression: funds-transfer must be dispatched EXACTLY ONCE even
// when the attempt fails ambiguously — the TSQ sweeper (not a blind
// retry) resolves the outcome. Retried POSTs risk a double debit.

func TestFundsTransferNeverBlindRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// simulate rail-side failure after accepting the transfer
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	a, err := NewNIPHTTPAdapter(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.FundsTransfer(NIPTransferRequest{SessionID: "SES-B3B-1", AmountKobo: 1000})
	if err != nil {
		t.Fatalf("ambiguous outcome must surface as in_flight, not error: %v", err)
	}
	if res.Status != NIPStatusInFlight {
		t.Fatalf("status = %q, want in_flight", res.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("funds-transfer dispatched %d times, want exactly 1 (never blind retry)", got)
	}
}

func TestNameEnquiryStillRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"account_name":"Ada","account_number":"0123456789","bank_code":"999"}`))
	}))
	defer srv.Close()

	a, err := NewNIPHTTPAdapter(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.NameEnquiry("0123456789", "999")
	if err != nil {
		t.Fatalf("idempotent enquiry should retry and succeed: %v", err)
	}
	if !out.Verified {
		t.Fatal("expected verified enquiry")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("enquiry calls = %d, want 3 (retries preserved for idempotent queries)", got)
	}
}
