package main

import (
	"strings"
	"testing"
)

// B4-10 regression: per-user salt + iterated HMAC PIN storage.
func TestPINSaltedIteratedStorage(t *testing.T) {
	mgr := NewPINManager(nil, nil)
	phone := "+2348000000001"
	if err := mgr.SetPIN(phone, "4321"); err != nil {
		t.Fatal(err)
	}
	rec, ok := mgr.store.Get(phone)
	if !ok {
		t.Fatal("record missing")
	}
	if rec.Salt == "" {
		t.Fatal("B4-10: PIN record must carry a per-user random salt")
	}
	if rec.Hash == mgr.hash(phone, "4321") {
		t.Fatal("B4-10: stored hash must not equal the legacy single-pass HMAC")
	}
	if err := mgr.Verify(phone, "4321"); err != nil {
		t.Fatalf("verify with correct PIN: %v", err)
	}
	if err := mgr.Verify(phone, "0000"); err == nil || !strings.Contains(err.Error(), "incorrect PIN") {
		t.Fatalf("wrong PIN must fail: %v", err)
	}
}

// B4-10 regression: distinct salts for identical PINs on different users.
func TestPINSaltsDifferPerUser(t *testing.T) {
	mgr := NewPINManager(nil, nil)
	_ = mgr.SetPIN("+2348000000002", "1234")
	_ = mgr.SetPIN("+2348000000003", "1234")
	a, _ := mgr.store.Get("+2348000000002")
	b, _ := mgr.store.Get("+2348000000003")
	if a.Salt == b.Salt || a.Hash == b.Hash {
		t.Fatal("identical PINs on different users must produce different salts/hashes")
	}
}

// B4-10 regression: a legacy (unsalted, single-pass) record still verifies
// and is transparently upgraded to salted storage.
func TestPINLegacyRecordUpgrade(t *testing.T) {
	mgr := NewPINManager(nil, nil)
	phone := "+2348000000004"
	mgr.store.Put(phone, PINRecord{Hash: mgr.hash(phone, "7777")})
	if err := mgr.Verify(phone, "7777"); err != nil {
		t.Fatalf("legacy record must still verify: %v", err)
	}
	rec, _ := mgr.store.Get(phone)
	if rec.Salt == "" {
		t.Fatal("successful legacy verify must upgrade the record to salted storage")
	}
	if err := mgr.Verify(phone, "7777"); err != nil {
		t.Fatalf("upgraded record must verify: %v", err)
	}
}
