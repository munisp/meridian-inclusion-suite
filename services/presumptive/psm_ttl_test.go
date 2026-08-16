package main

import (
	"testing"
	"time"
)


// ---- R4-2: PSM intent idempotency TTL ----

func psmIntentReq(key string) IntentRequest {
	return IntentRequest{
		TINHash: "tinhash-ttl", State: "Lagos", TradeCategory: "retail",
		AnnualTurnoverKobo: 300000000, Provider: "remita", Period: "2026",
		IdempotencyKey: key,
	}
}

func TestPSMIntentIdempotencyTTLAndExpiredKeyReuse(t *testing.T) {
	ts := newTestStack(t)
	a, err := ts.pay.CreateIntent(psmIntentReq("k-ttl"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ts.pay.CreateIntent(psmIntentReq("k-ttl"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("replay within TTL must return the original payment")
	}
	// record must carry an explicit expires_at ~7d out
	var rec IdempotencyRecord
	if ok, _ := ts.st.Get("idempotency", "k-ttl", &rec); !ok {
		t.Fatal("idempotency record missing")
	}
	if rec.ExpiresAt == "" {
		t.Fatal("expires_at must be set")
	}
	exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("TTL not ~7 days: %v", d)
	}
	// expire it: reused key is treated as new
	rec.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := ts.st.Put("idempotency", "k-ttl", rec); err != nil {
		t.Fatal(err)
	}
	c, err := ts.pay.CreateIntent(psmIntentReq("k-ttl"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == a.ID {
		t.Fatal("expired key must create a fresh payment")
	}
}

func TestPSMIdempotencyPurgeTerminalOnly(t *testing.T) {
	ts := newTestStack(t)
	p, err := ts.pay.CreateIntent(psmIntentReq("k-purge"))
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	// expired but non-terminal (pending_authorisation): retained
	var rec IdempotencyRecord
	if ok, _ := ts.st.Get("idempotency", "k-purge", &rec); !ok {
		t.Fatal("missing record")
	}
	rec.ExpiresAt = past
	if err := ts.st.Put("idempotency", "k-purge", rec); err != nil {
		t.Fatal(err)
	}
	n, err := ts.pay.PurgeExpiredIdempotency()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("non-terminal record must be retained, purged %d", n)
	}
	// mark terminal -> purged
	p.Status = "failed"
	if err := ts.st.Put("payments", p.ID, p); err != nil {
		t.Fatal(err)
	}
	n, err = ts.pay.PurgeExpiredIdempotency()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purge, got %d", n)
	}
	if ok, _ := ts.st.Get("idempotency", "k-purge", &IdempotencyRecord{}); ok {
		t.Fatal("terminal expired record should be purged")
	}
}
