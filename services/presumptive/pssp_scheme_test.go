package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"testing"
)

// TestPaystackSHA512Scheme (G3): an HMAC-SHA512-signed payload verifies under
// the paystack scheme; a sha256 signature (wrong scheme) is rejected.
func TestPaystackSHA512Scheme(t *testing.T) {
	hub := NewPSSPHub()
	if got := hub.WebhookScheme("paystack"); got != SchemeHMACSHA512 {
		t.Fatalf("paystack scheme must be hmac-sha512, got %s", got)
	}
	body := []byte(`{"event":"charge.successful","data":{"reference":"PSK-1"}}`)
	mac := hmac.New(sha512.New, []byte(webhookSecretFor("paystack")))
	mac.Write(body)
	sig512 := hex.EncodeToString(mac.Sum(nil))
	if !hub.VerifyWebhookSignatureFor("paystack", sig512, body) {
		t.Fatal("valid HMAC-SHA512 signature must verify under paystack scheme")
	}
	// wrong scheme: a sha256 HMAC of the same body must NOT verify
	mac256 := hmac.New(sha256.New, []byte(webhookSecretFor("paystack")))
	mac256.Write(body)
	if hub.VerifyWebhookSignatureFor("paystack", hex.EncodeToString(mac256.Sum(nil)), body) {
		t.Fatal("sha256 signature must be rejected under the sha512 scheme")
	}
	// and the sha512 signature must not verify under the sha256 scheme
	if hub.VerifyWebhookSignatureFor("remita", sig512, body) {
		t.Fatal("sha512 signature must be rejected under the sha256 scheme")
	}
}

// TestFlutterwaveVerifHashScheme (G3): the Flutterwave verif-hash scheme is
// shared-secret equality (constant time), not an HMAC of the body.
func TestFlutterwaveVerifHashScheme(t *testing.T) {
	hub := NewPSSPHub()
	if got := hub.WebhookScheme("flutterwave"); got != SchemeVerifHash {
		t.Fatalf("flutterwave scheme must be verif-hash, got %s", got)
	}
	body := []byte(`{"event":"charge.completed","data":{"tx_ref":"FLW-1"}}`)
	if !hub.VerifyWebhookSignatureFor("flutterwave", webhookSecretFor("flutterwave"), body) {
		t.Fatal("correct verif-hash secret must verify")
	}
	if hub.VerifyWebhookSignatureFor("flutterwave", "not-the-secret", body) {
		t.Fatal("wrong verif-hash must be rejected")
	}
	if hub.VerifyWebhookSignatureFor("flutterwave", "", body) {
		t.Fatal("empty verif-hash must fail closed")
	}
}

// TestSchemeAndSecretOverrides (G3): scheme and secret are selectable per
// registered PSSP via env, and unknown schemes fail closed.
func TestSchemeAndSecretOverrides(t *testing.T) {
	t.Setenv("PSSP_WEBHOOK_SCHEME_REMITA", "hmac-sha512")
	t.Setenv("PSSP_WEBHOOK_SECRET_REMITA", "remita-specific-secret")
	hub := NewPSSPHub()
	body := []byte(`{"reference":"RRR-1","event":"charge.successful"}`)
	mac := hmac.New(sha512.New, []byte("remita-specific-secret"))
	mac.Write(body)
	if !hub.VerifyWebhookSignatureFor("remita", hex.EncodeToString(mac.Sum(nil)), body) {
		t.Fatal("env-selected scheme+secret must verify")
	}
	// the generic fallback secret must not verify for the overridden provider
	mac2 := hmac.New(sha512.New, []byte(webhookSecret()))
	mac2.Write(body)
	if hub.VerifyWebhookSignatureFor("remita", hex.EncodeToString(mac2.Sum(nil)), body) {
		t.Fatal("fallback secret must not verify once a per-PSSP secret is set")
	}
	// unknown scheme fails closed
	t.Setenv("PSSP_WEBHOOK_SCHEME_ETRANZACT", "md5")
	if hub.VerifyWebhookSignatureFor("etranzact", "whatever", body) {
		t.Fatal("unknown scheme must fail closed")
	}
}
