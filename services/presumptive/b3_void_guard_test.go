package main

import (
	"strings"
	"testing"
)

// B3 #6 regression: void must be reachable only from pre-capture states.
// Pre-fix: captured_awaiting_post, capture_in_flight, compensated and
// disputed payments could be voided — releasing the ledger hold while the
// provider already holds the money.

func TestB3VoidGuardByStatus(t *testing.T) {
	ts := newTestStack(t)
	p := ts.mkIntent(t, "tinhash-voidguard")

	// pre-capture: voidable
	p.Status = "authorised"
	if err := ts.st.Put("payments", p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.pay.Void(p.ID); err != nil {
		t.Fatalf("authorised payment must be voidable: %v", err)
	}

	for _, st := range []string{
		"capture_in_flight", "captured_awaiting_post", "captured",
		"compensated", "disputed", "charged_back", "settled",
	} {
		q := ts.mkIntent(t, "tinhash-voidguard-"+st)
		q.Status = st
		if err := ts.st.Put("payments", q.ID, q); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.pay.Void(q.ID); err == nil {
			t.Fatalf("status %s must NOT be voidable", st)
		} else if st != "captured" && !strings.Contains(err.Error(), "not voidable") {
			t.Fatalf("status %s: unexpected error %v", st, err)
		}
		// the durable record must be unchanged
		var got Payment
		ok, err := ts.pay.st.Get("payments", q.ID, &got)
		if err != nil || !ok {
			t.Fatal("payment lookup failed")
		}
		if got.Status != st {
			t.Fatalf("status %s mutated to %s by rejected void", st, got.Status)
		}
	}

	// voided stays idempotent
	r := ts.mkIntent(t, "tinhash-voidguard-done")
	r.Status = "voided"
	if err := ts.st.Put("payments", r.ID, r); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.pay.Void(r.ID); err != nil {
		t.Fatalf("voided must be idempotent: %v", err)
	}
}
