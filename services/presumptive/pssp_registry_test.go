package main

import (
	"strings"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func TestPSSPRegistrySeedAndOnboard(t *testing.T) {
	st, _ := store.Open("")
	hub := NewPSSPHub()
	reg := NewPSSPRegistry(st, hub)
	list, err := reg.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("seeded pssps: %v err=%v", list, err)
	}
	for _, p := range list {
		if p.Status != "sandbox" || !p.Sim || p.SecretPreview == "" {
			t.Fatalf("seed: %+v", p)
		}
		if strings.Contains(p.SecretPreview, "whsec_") && len(p.SecretPreview) > 12 {
			t.Fatalf("secret must be redacted: %s", p.SecretPreview)
		}
	}
	// onboard a new PSSP -> own secret, own sim adapter
	res, err := reg.Onboard(OnboardRequest{Name: "Paystack", CallbackURL: "https://ps.example/hook", FeeBps: 160})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.WebhookSecret, "whsec_") || res.Status != "sandbox" {
		t.Fatalf("onboard: %+v", res)
	}
	if _, err := hub.Adapter("paystack"); err != nil {
		t.Fatalf("per-PSSP sim adapter not keyed: %v", err)
	}
	// duplicate name rejected
	if _, err := reg.Onboard(OnboardRequest{Name: "paystack", CallbackURL: "https://x"}); err == nil {
		t.Fatal("duplicate PSSP name must be rejected")
	}
}

func TestPSSPPerProviderWebhookSecrets(t *testing.T) {
	st, _ := store.Open("")
	reg := NewPSSPRegistry(st, NewPSSPHub())
	res, _ := reg.Onboard(OnboardRequest{Name: "moniepoint", CallbackURL: "https://m.example/hook"})
	body := []byte(`{"reference":"MNP-1","event":"charge.successful"}`)
	good := hmacHex(res.WebhookSecret, string(body))
	if !reg.VerifyWebhook("moniepoint", good, body) {
		t.Fatal("per-PSSP signature must validate")
	}
	// legacy shared secret must NOT validate for a registered PSSP
	legacy := hmacHex(webhookSecret(), string(body))
	if reg.VerifyWebhook("moniepoint", legacy, body) {
		t.Fatal("shared secret must not validate a registered PSSP")
	}
	// a DIFFERENT PSSP's secret must not validate either
	other, _ := reg.Onboard(OnboardRequest{Name: "opay", CallbackURL: "https://o.example/hook"})
	if reg.VerifyWebhook("moniepoint", hmacHex(other.WebhookSecret, string(body)), body) {
		t.Fatal("cross-PSSP secret must not validate")
	}
	// rotation invalidates the old secret
	rot, err := reg.RotateSecret(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.SecretVersion != 2 || rot.WebhookSecret == res.WebhookSecret {
		t.Fatalf("rotation: %+v", rot)
	}
	if reg.VerifyWebhook("moniepoint", good, body) {
		t.Fatal("rotated-out secret must be invalid")
	}
	if !reg.VerifyWebhook("moniepoint", hmacHex(rot.WebhookSecret, string(body)), body) {
		t.Fatal("new secret must validate after rotation")
	}
}

func TestPSSPStatusPromotion(t *testing.T) {
	st, _ := store.Open("")
	reg := NewPSSPRegistry(st, NewPSSPHub())
	res, _ := reg.Onboard(OnboardRequest{Name: "teamapt", CallbackURL: "https://t.example/hook"})
	if _, err := reg.SetStatus(res.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetStatus(res.ID, "sandbox"); err != nil { // active->sandbox allowed (demotion)
		t.Fatal(err)
	}
	if _, err := reg.SetStatus(res.ID, "bogus"); err == nil {
		t.Fatal("illegal status must be rejected")
	}
}

func TestPSSPWebhookUnknownProviderProdRejected(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("PSSP_WEBHOOK_SECRET", "prod-shared-secret")
	defer t.Setenv("APP_PROFILE", "")
	st, _ := store.Open("")
	reg := NewPSSPRegistry(st, NewPSSPHub())
	body := []byte(`{"reference":"X","event":"charge.successful"}`)
	if reg.VerifyWebhook("ghost", hmacHex(webhookSecret(), string(body)), body) {
		t.Fatal("unregistered provider must be rejected in prod")
	}
	t.Setenv("APP_PROFILE", "")
	if !reg.VerifyWebhook("ghost", hmacHex(webhookSecret(), string(body)), body) {
		t.Fatal("dev fallback to legacy shared secret expected for unknown provider")
	}
}
