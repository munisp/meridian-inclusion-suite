package main

import (
	"errors"
	"sync"
	"testing"
)

// B3 #7 regression: CreateIntent idempotency must be atomic. Pre-fix the
// check-then-act let concurrent same-key requests each create a payment
// and a ledger hold (double hold on the payer).

func TestB3ConcurrentSameKeySingleHold(t *testing.T) {
	ts := newTestStack(t)
	const n = 8
	req := IntentRequest{
		TINHash: "tinhash-b3-idem", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: "idem-b3-race",
	}
	var wg sync.WaitGroup
	results := make([]Payment, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p, err := ts.pay.CreateIntent(req)
			results[i], errs[i] = p, err
		}(i)
	}
	close(start)
	wg.Wait()

	payIDs := map[string]int{}
	conflicts := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			if errors.Is(errs[i], ErrIdempotencyConflict) {
				conflicts++ // in-progress claim: client retries and replays
				continue
			}
			t.Fatalf("unexpected error: %v", errs[i])
		}
		payIDs[results[i].ID]++
	}
	if len(payIDs) != 1 {
		t.Fatalf("concurrent same-key requests created %d distinct payments", len(payIDs))
	}
	// exactly one payment and exactly one pending hold exist
	var payments []Payment
	if err := ts.st.List("payments", &payments); err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 {
		t.Fatalf("%d payments stored; want 1", len(payments))
	}
	payerID, _ := ts.pay.payerAccountID("tinhash-b3-idem")
	bal, err := ts.lc.Balance(payerID)
	if err != nil {
		t.Fatal(err)
	}
	if bal.DebitsPending != payments[0].AmountKobo {
		t.Fatalf("payer pending hold %d; want exactly one hold of %d",
			bal.DebitsPending, payments[0].AmountKobo)
	}
	// sequential retry replays the same payment
	again, err := ts.pay.CreateIntent(req)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != payments[0].ID {
		t.Fatal("same-key retry must replay the original payment")
	}
}

func TestB3StorePutIfAbsentAtomic(t *testing.T) {
	ts := newTestStack(t)
	ok, err := ts.st.PutIfAbsent("idempotency", "k1", map[string]any{"v": 1})
	if err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	ok, err = ts.st.PutIfAbsent("idempotency", "k1", map[string]any{"v": 2})
	if err != nil || ok {
		t.Fatalf("second claim must lose: %v %v", ok, err)
	}
}
