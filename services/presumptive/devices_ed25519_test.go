package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// devices_ed25519_test.go — regression tests for S1b#10: offline receipts
// move from symmetric server-stored MAC keys to device-held ed25519 keys;
// the server stores ONLY public keys.

func newDeviceService(t *testing.T) *DeviceService {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	return NewDeviceService(st)
}

func sampleReceipt(agentID, deviceID string) ReceiptVerifyRequest {
	return ReceiptVerifyRequest{
		Serial: "RCPT-ED-1", AgentID: agentID, DeviceID: deviceID,
		PayerName: "Musa Bello", AmountKobo: 500000, Purpose: "presumptive levy",
		IssuedAt: "2026-01-01T10:00:00Z",
	}
}

// 1. A receipt signed with the device ed25519 private key verifies against
// the enrolled public key.
func TestEd25519ReceiptVerifies(t *testing.T) {
	ds := newDeviceService(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dk, err := ds.EnrollEd25519("agent-1", "dev-ed1", hex.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	if dk.Key != "" || dk.Legacy {
		t.Fatal("ed25519-enrolled device must not persist any symmetric key material")
	}
	req := sampleReceipt("agent-1", "dev-ed1")
	req.Signature = SignReceiptEd25519(priv, CanonicalReceiptPayload(req))
	valid, detail, err := ds.VerifyReceipt(req)
	if err != nil || !valid {
		t.Fatalf("ed25519 receipt must verify: valid=%v detail=%s err=%v", valid, detail, err)
	}
}

// 2. A tampered receipt fails verification.
func TestEd25519TamperedReceiptFails(t *testing.T) {
	ds := newDeviceService(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ds.EnrollEd25519("agent-1", "dev-ed2", hex.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	req := sampleReceipt("agent-1", "dev-ed2")
	req.Signature = SignReceiptEd25519(priv, CanonicalReceiptPayload(req))
	bad := req
	bad.AmountKobo = 900000 // tampered after signing
	if valid, d, _ := ds.VerifyReceipt(bad); valid {
		t.Fatalf("tampered receipt must not verify (detail=%s)", d)
	}
	// signature from a DIFFERENT device key must not verify either
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	bad = req
	bad.Signature = SignReceiptEd25519(otherPriv, CanonicalReceiptPayload(req))
	if valid, _, _ := ds.VerifyReceipt(bad); valid {
		t.Fatal("signature from another key must not verify")
	}
}

// 3. Post-cutover (grace disabled, as fail-closed in prod) a legacy device
// cannot have new receipts verified; it is forced to re-enrol with ed25519,
// after which its ed25519 receipts verify. During the grace window legacy
// MAC receipts still verify via the old path.
func TestLegacyDeviceCutover(t *testing.T) {
	ds := newDeviceService(t)
	if _, err := ds.Enroll("agent-2", "dev-legacy", "legacy-secret-key-123"); err != nil {
		t.Fatal(err)
	}
	req := sampleReceipt("agent-2", "dev-legacy")
	req.Signature = signReceipt("legacy-secret-key-123", CanonicalReceiptPayload(req))

	// grace window open: old MAC path still verifies
	t.Setenv("DEVICE_LEGACY_GRACE", "true")
	if valid, d, _ := ds.VerifyReceipt(req); !valid {
		t.Fatalf("legacy receipt must verify during grace, got detail=%s", d)
	}

	// cutover: grace closed -> new legacy-MAC receipts fail closed
	t.Setenv("DEVICE_LEGACY_GRACE", "false")
	if valid, d, _ := ds.VerifyReceipt(req); valid {
		t.Fatalf("legacy device must not issue verifiable receipts post-cutover (detail=%s)", d)
	}

	// forced re-enrolment with ed25519 upgrades the device
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dk, err := ds.EnrollEd25519("agent-2", "dev-legacy", hex.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	if dk.Legacy {
		t.Fatal("re-enrolled device must no longer be flagged legacy")
	}
	req2 := sampleReceipt("agent-2", "dev-legacy")
	req2.Signature = SignReceiptEd25519(priv, CanonicalReceiptPayload(req2))
	if valid, d, _ := ds.VerifyReceipt(req2); !valid {
		t.Fatalf("post-cutover ed25519 receipt must verify, got detail=%s", d)
	}
	// ...and the old MAC credential is dead for new receipts
	if valid, _, _ := ds.VerifyReceipt(req); valid {
		t.Fatal("old MAC receipt must not verify after ed25519 re-enrolment cutover")
	}
}

// 4. Server key-store compromise yields no signing capability: an attacker
// with full read access to the device store cannot forge a valid receipt
// for an ed25519-enrolled device.
func TestServerCompromiseCannotForge(t *testing.T) {
	ds := newDeviceService(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ds.EnrollEd25519("agent-3", "dev-ed3", hex.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	_ = priv // attacker does NOT have this

	// Attacker reads the stored record straight from the DB.
	stored, ok, err := ds.PublicKey("agent-3", "dev-ed3")
	if err != nil || !ok {
		t.Fatal("public key lookup failed")
	}
	var raw DeviceKey
	if ok, err := ds.st.Get("devices", deviceStoreID("agent-3", "dev-ed3"), &raw); err != nil || !ok {
		t.Fatal("attacker DB read failed")
	}
	if raw.Key != "" {
		t.Fatal("no symmetric key material may be stored for ed25519 devices")
	}

	req := sampleReceipt("agent-3", "dev-ed3")
	// Attacker forges using everything the DB gave them (the public key).
	forged := signReceipt(stored, CanonicalReceiptPayload(req))
	req.Signature = forged
	if valid, _, _ := ds.VerifyReceipt(req); valid {
		t.Fatal("attacker with DB read access must not be able to forge a receipt")
	}
	// Even a correctly-shaped 64-byte "signature" derived from public data
	// cannot verify.
	sig := make([]byte, ed25519.SignatureSize)
	copy(sig, []byte(stored))
	req.Signature = hex.EncodeToString(sig)
	if valid, _, _ := ds.VerifyReceipt(req); valid {
		t.Fatal("public-data-derived signature must not verify")
	}
}
