package resilience

import (
	"errors"
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := &Breaker{Threshold: 5, Cooldown: 30 * time.Second}
	boom := errors.New("boom")
	for i := 0; i < 5; i++ {
		if err := b.Do(func() error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: want boom, got %v", i, err)
		}
	}
	if err := b.Do(func() error { return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("breaker should be open, got %v", err)
	}
	// retries short-circuit while open
	if err := b.Retry(3, func() error { return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("retry should short-circuit on open breaker, got %v", err)
	}
}

func TestBreakerHalfOpenRecovery(t *testing.T) {
	b := &Breaker{Threshold: 2, Cooldown: 20 * time.Millisecond}
	boom := errors.New("boom")
	_ = b.Do(func() error { return boom })
	_ = b.Do(func() error { return boom })
	if !b.Allow() == true {
		// open
	}
	if err := b.Do(func() error { return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("want open, got %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := b.Retry(3, func() error { return nil }); err != nil {
		t.Fatalf("breaker should recover after cooldown, got %v", err)
	}
	if b.failures != 0 {
		t.Fatalf("failures should reset, got %d", b.failures)
	}
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	b := &Breaker{Threshold: 10, Cooldown: time.Second}
	n := 0
	err := b.Retry(3, func() error {
		n++
		if n < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("want success on 3rd attempt, got err=%v n=%d", err, n)
	}
}
