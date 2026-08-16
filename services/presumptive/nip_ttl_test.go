package main

import (
	"testing"
	"time"
)


// R4-2: idempotency TTL — an expired key is treated as new: a fresh transfer
// with a new session id is created.
func TestExpiredNIPIdempotencyKeyTreatedAsNew(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	a, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	// force the stored record's window closed
	var rec NIPTransfer
	if ok, _ := svc.st.Get("nip_transfers", "idem:"+a.IdempotencyKey, &rec); !ok {
		t.Fatal("idem record missing")
	}
	rec.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := svc.st.Put("nip_transfers", "idem:"+rec.IdempotencyKey, rec); err != nil {
		t.Fatal(err)
	}
	b, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if b.SessionID == a.SessionID || b.ID == a.ID {
		t.Fatal("expired key must start a fresh transfer")
	}
}

// R4-2: sweeper purges expired records only in terminal state; in_flight
// expired records are retained for TSQ resolution.
func TestNIPIdempotencyPurgeTerminalOnly(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	done, err := svc.Payout(payoutReq("0123456789")) // sim: success terminal
	if err != nil {
		t.Fatal(err)
	}
	req := payoutReq("9876543210")
	inflight, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, idem := range []string{done.IdempotencyKey, inflight.IdempotencyKey} {
		var rec NIPTransfer
		if ok, _ := svc.st.Get("nip_transfers", "idem:"+idem, &rec); !ok {
			t.Fatal("missing", idem)
		}
		rec.ExpiresAt = past
		if rec.ID == inflight.ID {
			rec.Status = NIPStatusInFlight
		}
		if err := svc.st.Put("nip_transfers", "idem:"+idem, rec); err != nil {
			t.Fatal(err)
		}
	}
	n, err := svc.PurgeExpiredIdempotency()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged (terminal only), got %d", n)
	}
	if ok, _ := svc.st.Get("nip_transfers", "idem:"+done.IdempotencyKey, &NIPTransfer{}); ok {
		t.Fatal("terminal expired record should be purged")
	}
	if ok, _ := svc.st.Get("nip_transfers", "idem:"+inflight.IdempotencyKey, &NIPTransfer{}); !ok {
		t.Fatal("in_flight expired record must be retained")
	}
}
