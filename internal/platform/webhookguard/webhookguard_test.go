package webhookguard

import (
	"errors"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestTimestampTolerance(t *testing.T) {
	g := NewGuard("X-TS", "X-Nonce", true, nil)
	now := time.Now()
	g.Now = func() time.Time { return now }

	mk := func(ts, nonce string) error {
		r := httptest.NewRequest("POST", "/webhook", nil)
		if ts != "" {
			r.Header.Set("X-TS", ts)
		}
		if nonce != "" {
			r.Header.Set("X-Nonce", nonce)
		}
		return g.Check(r)
	}

	// fresh (unix seconds + RFC3339 both accepted)
	if err := mk(strconv.FormatInt(now.Unix(), 10), "n1"); err != nil {
		t.Fatalf("fresh unix ts: %v", err)
	}
	if err := mk(now.Format(time.RFC3339), "n2"); err != nil {
		t.Fatalf("fresh rfc3339 ts: %v", err)
	}
	// within tolerance
	if err := mk(strconv.FormatInt(now.Add(-4*time.Minute).Unix(), 10), "n3"); err != nil {
		t.Fatalf("4min old: %v", err)
	}
	// expired -> ErrStaleTimestamp (401)
	if err := mk(strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10), "n4"); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("10min old: got %v, want ErrStaleTimestamp", err)
	}
	// future beyond tolerance
	if err := mk(strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10), "n5"); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("10min future: got %v, want ErrStaleTimestamp", err)
	}
	// malformed
	if err := mk("not-a-time", "n6"); !errors.Is(err, ErrBadTimestamp) {
		t.Fatalf("bad ts: got %v, want ErrBadTimestamp", err)
	}
	// missing headers in prod mode -> ErrMissingHeaders
	if err := mk("", ""); !errors.Is(err, ErrMissingHeaders) {
		t.Fatalf("missing headers: got %v, want ErrMissingHeaders", err)
	}
}

func TestReplayCache(t *testing.T) {
	store := NewInProcStore()
	g := NewGuard("X-TS", "X-Nonce", true, store)
	now := time.Now()
	g.Now = func() time.Time { return now }
	store.now = g.Now // shared fake clock
	ts := strconv.FormatInt(now.Unix(), 10)

	mk := func(nonce string) error {
		r := httptest.NewRequest("POST", "/webhook", nil)
		r.Header.Set("X-TS", ts)
		r.Header.Set("X-Nonce", nonce)
		return g.Check(r)
	}
	if err := mk("nonce-1"); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// replayed -> ErrReplay (409)
	if err := mk("nonce-1"); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: got %v, want ErrReplay", err)
	}
	// different nonce ok
	if err := mk("nonce-2"); err != nil {
		t.Fatalf("new nonce: %v", err)
	}
	// after TTL the same nonce is accepted again (fresh timestamp)
	g.Now = func() time.Time { return now.Add(16 * time.Minute) }
	store.now = g.Now
	r := httptest.NewRequest("POST", "/webhook", nil)
	r.Header.Set("X-TS", strconv.FormatInt(now.Add(16*time.Minute).Unix(), 10))
	r.Header.Set("X-Nonce", "nonce-1")
	if err := g.Check(r); err != nil {
		t.Fatalf("nonce after TTL: %v", err)
	}
}

func TestDevModeMissingHeadersSkips(t *testing.T) {
	g := NewGuard("X-TS", "X-Nonce", false, nil)
	r := httptest.NewRequest("POST", "/webhook", nil)
	if err := g.Check(r); err != nil {
		t.Fatalf("dev missing headers must skip: %v", err)
	}
}
