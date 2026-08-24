package main

import (
	"errors"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func newTestRegistry(t *testing.T) *PSSPRegistry {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	return NewPSSPRegistry(st, NewPSSPHub())
}

// B4-11 regression: name < 3 chars must return a validation error, not panic.
func TestOnboardShortNameNoPanic(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := reg.Onboard(OnboardRequest{Name: "ab", CallbackURL: "https://x.example/hook"})
	var ve *OnboardValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("short name must yield OnboardValidationError, got %v", err)
	}
}

// B4-12 regression: callback_url scheme/host validation.
func TestOnboardCallbackURLValidation(t *testing.T) {
	reg := newTestRegistry(t)
	cases := []string{
		"not-a-url",
		"ftp://x.example/hook",
		"https:///no-host",
		"",
	}
	for _, cb := range cases {
		if _, err := reg.Onboard(OnboardRequest{Name: "validname", CallbackURL: cb}); err == nil {
			t.Fatalf("callback %q must be rejected", cb)
		}
	}
	if _, err := reg.Onboard(OnboardRequest{Name: "okname", CallbackURL: "https://ok.example/hook"}); err != nil {
		t.Fatalf("valid https callback must onboard: %v", err)
	}
}

// B4-12 regression: prod refuses http, loopback and RFC1918 callback hosts.
// (validateCallbackURL tested directly: full Onboard in prod profile
// log.Fatals on unrelated missing keys via keyx.MustKey.)
func TestOnboardCallbackURLProdRefusesInternal(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	bad := []string{
		"http://ok.example/hook",     // plain http in prod
		"https://localhost/hook",     // loopback name
		"https://127.0.0.1/hook",     // loopback
		"https://10.1.2.3/hook",      // RFC1918
		"https://192.168.1.10/hook",  // RFC1918
		"https://169.254.169.254/md", // link-local metadata
	}
	for _, cb := range bad {
		if err := validateCallbackURL(cb); err == nil {
			t.Fatalf("prod must refuse callback %q", cb)
		}
	}
	if err := validateCallbackURL("https://hooks.example.com/pssp"); err != nil {
		t.Fatalf("prod must allow public https callback: %v", err)
	}
}
