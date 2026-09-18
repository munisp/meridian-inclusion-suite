package main

import (
	"errors"
	"strings"
	"testing"
)

// R4-S1a#10 regression: the rule pack pinned at intent must still govern the
// capture. A pack bump between authorise and capture must park the payment
// for review — never settle a stale amount.

// bumpLagosPack mutates the loaded lagos pack in-place: version bump and a
// doubled micro/retail levy, simulating a mid-flight regazette.
func bumpLagosPack(t *testing.T, ts *testStack, newVersion string, doubleLevy bool) {
	t.Helper()
	p := ts.engine.packs["rp-presumptive-lagos"]
	p.Version = newVersion
	if doubleLevy {
		table := p.Rules["levy_table_annual_kobo"].(map[string]any)
		for band, rowAny := range table {
			row := rowAny.(map[string]any)
			for cat, v := range row {
				if f, ok := v.(float64); ok {
					row[cat] = f * 2
				}
			}
			table[band] = row
		}
	}
	ts.engine.packs["rp-presumptive-lagos"] = p
}

func TestR4PackBumpBlocksStaleSettle(t *testing.T) {
	t.Run("version bump with amount change", func(t *testing.T) {
		ts := newTestStack(t)
		p := ts.mkIntent(t, "tinhash-pack-pin-1")
		pinnedVersion := p.RulePackVersion
		pinnedAmount := p.AmountKobo
		if _, auth, err := ts.pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
			t.Fatalf("authorise: %v %+v", err, auth)
		}

		// Pack regazettes mid-flight: new version, doubled levy.
		bumpLagosPack(t, ts, "2999.0.0", true)

		p2, _, err := ts.pay.Capture(p.ID)
		if !errors.Is(err, ErrPackChangedMidFlight) {
			t.Fatalf("want ErrPackChangedMidFlight, got %v", err)
		}
		if p2.Status != "capture_in_flight" {
			t.Fatalf("payment status = %s, want capture_in_flight (parked)", p2.Status)
		}
		// No money moved: no post transfer, no certificate, stale amount
		// still the pinned one, and the reason names both packs.
		if p2.PostTransferID != "" || p2.CertificateSerial != "" {
			t.Fatalf("stale settle happened: %+v", p2)
		}
		if p2.AmountKobo != pinnedAmount {
			t.Fatalf("amount mutated: %d -> %d", pinnedAmount, p2.AmountKobo)
		}
		if !strings.Contains(p2.FailReason, pinnedVersion) || !strings.Contains(p2.FailReason, "2999.0.0") {
			t.Fatalf("fail reason must name pinned+current packs: %s", p2.FailReason)
		}
	})

	t.Run("same version but amount drift", func(t *testing.T) {
		ts := newTestStack(t)
		p := ts.mkIntent(t, "tinhash-pack-pin-2")
		if _, auth, err := ts.pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
			t.Fatalf("authorise: %v %+v", err, auth)
		}
		// Silent table edit without a version bump must ALSO park.
		bumpLagosPack(t, ts, ts.engine.packs["rp-presumptive-lagos"].Version, true)
		if _, _, err := ts.pay.Capture(p.ID); !errors.Is(err, ErrPackChangedMidFlight) {
			t.Fatalf("amount drift without version bump: want ErrPackChangedMidFlight, got %v", err)
		}
	})

	t.Run("unchanged pack captures normally", func(t *testing.T) {
		ts := newTestStack(t)
		p := ts.mkIntent(t, "tinhash-pack-pin-3")
		if _, auth, err := ts.pay.Authorise(p.ID); err != nil || auth.Status != "authorised" {
			t.Fatalf("authorise: %v %+v", err, auth)
		}
		p2, cert, err := ts.pay.Capture(p.ID)
		if err != nil {
			t.Fatalf("capture with unchanged pack: %v", err)
		}
		if p2.Status != "captured" || cert.Serial == "" {
			t.Fatalf("capture with unchanged pack: %+v cert %+v", p2, cert)
		}
	})
}
