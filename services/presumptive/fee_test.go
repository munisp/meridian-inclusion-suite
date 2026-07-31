package main

import "testing"

// TestFeeScheduleCapBinds (G12): on large amounts the naira cap binds instead
// of the linear percentage; below the cap the plain rate applies. Uses the
// documented CBN MSC norm (0.5%, capped ₦2,000 = 200,000 kobo).
func TestFeeScheduleCapBinds(t *testing.T) {
	// below cap: 0.5% of ₦20,000 (2,000,000 kobo) = ₦100 -> uncapped
	if fee := DefaultMSCSchedule.Fee(2000000); fee != 10000 {
		t.Fatalf("uncapped fee wrong: %d", fee)
	}
	// at the knee: 0.5% of ₦400,000 = ₦2,000 -> exactly the cap
	if fee := DefaultMSCSchedule.Fee(40000000); fee != 200000 {
		t.Fatalf("knee fee wrong: %d", fee)
	}
	// large amount: 0.5% of ₦10m would be ₦50,000 -> capped at ₦2,000
	if fee := DefaultMSCSchedule.Fee(1000000000); fee != 200000 {
		t.Fatalf("cap must bind on large amounts, got %d", fee)
	}
	// uncapped schedule (cap 0) stays linear
	lin := FeeSchedule{RateBps: 50}
	if fee := lin.Fee(1000000000); fee != 5000000 {
		t.Fatalf("cap=0 must be uncapped, got %d", fee)
	}
}

// TestSimCaptureAppliesCappedFee (G12): the simulated PSSP settles gross
// minus the CAPPED fee, so FeeKobo (gross - settled) matches what the real
// provider would settle and recon stays balanced on large levies.
func TestSimCaptureAppliesCappedFee(t *testing.T) {
	sim := newPSSPSim("flutterwave", "FLW-%s", FeeSchedule{RateBps: 140, CapKobo: 200000})
	// large levy: ₦1,000,000 -> 1.4% would be ₦14,000; cap binds at ₦2,000
	auth, err := sim.Authorise(AuthoriseRequest{PaymentID: "p1", AmountKobo: 100000000, PayerRef: "tin"})
	if err != nil || auth.Status != "authorised" {
		t.Fatalf("authorise: %+v %v", auth, err)
	}
	capRes, err := sim.Capture(auth.Reference, 100000000, "capture:p1")
	if err != nil || capRes.Status != "captured" {
		t.Fatalf("capture: %+v %v", capRes, err)
	}
	if fee := 100000000 - capRes.SettledKobo; fee != 200000 {
		t.Fatalf("cap must bind: fee %d != 200000 kobo (N2,000)", fee)
	}
	// small levy: ₦10,000 -> 1.4% = ₦140, below the cap -> uncapped
	auth2, _ := sim.Authorise(AuthoriseRequest{PaymentID: "p2", AmountKobo: 1000000, PayerRef: "tin"})
	capRes2, err := sim.Capture(auth2.Reference, 1000000, "capture:p2")
	if err != nil {
		t.Fatal(err)
	}
	if fee := 1000000 - capRes2.SettledKobo; fee != 14000 {
		t.Fatalf("below-cap fee must be linear: %d != 14000", fee)
	}
}

// TestFeeScheduleEnvOverride (G12): the per-PSSP schedule is configurable via
// env so a real provider's agreed schedule can be enforced without a deploy.
func TestFeeScheduleEnvOverride(t *testing.T) {
	t.Setenv("PSSP_FEE_RATE_BPS_REMITA", "50")
	t.Setenv("PSSP_FEE_CAP_KOBO_REMITA", "100000")
	s := feeScheduleFor("remita", FeeSchedule{RateBps: 100, CapKobo: 200000})
	if s.RateBps != 50 || s.CapKobo != 100000 {
		t.Fatalf("env override not applied: %+v", s)
	}
	if fee := s.Fee(1000000000); fee != 100000 {
		t.Fatalf("overridden cap must bind, got %d", fee)
	}
	// without env the registered default stands
	if d := feeScheduleFor("etranzact", FeeSchedule{RateBps: 75, CapKobo: 200000}); d.RateBps != 75 {
		t.Fatalf("default must stand without env, got %+v", d)
	}
}
