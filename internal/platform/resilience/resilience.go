// Package resilience implements the shared H4 resilience policy for external
// adapters: 3-retry exponential backoff + circuit breaker
// (5 consecutive failures → open for 30s, half-open probe afterwards).
package resilience

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when the breaker is open and the call is
// short-circuited.
var ErrCircuitOpen = errors.New("circuit breaker open")

// Breaker is a simple failure-count circuit breaker.
type Breaker struct {
	Threshold int           // failures before opening (default 5)
	Cooldown  time.Duration // open duration (default 30s)

	mu       sync.Mutex
	failures int
	openTill time.Time
}

func (b *Breaker) threshold() int {
	if b.Threshold <= 0 {
		return 5
	}
	return b.Threshold
}

func (b *Breaker) cooldown() time.Duration {
	if b.Cooldown <= 0 {
		return 30 * time.Second
	}
	return b.Cooldown
}

// Allow reports whether a call may proceed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures >= b.threshold() {
		if time.Now().Before(b.openTill) {
			return false
		}
		// half-open: allow one probe
		b.failures = b.threshold() - 1
	}
	return true
}

// Success records a successful call (resets the breaker).
func (b *Breaker) Success() {
	b.mu.Lock()
	b.failures = 0
	b.openTill = time.Time{}
	b.mu.Unlock()
}

// Failure records a failed call.
func (b *Breaker) Failure() {
	b.mu.Lock()
	b.failures++
	if b.failures >= b.threshold() {
		b.openTill = time.Now().Add(b.cooldown())
	}
	b.mu.Unlock()
}

// Do runs fn with the breaker; the caller supplies its own retry policy.
func (b *Breaker) Do(fn func() error) error {
	if !b.Allow() {
		return ErrCircuitOpen
	}
	err := fn()
	if err != nil {
		b.Failure()
	} else {
		b.Success()
	}
	return err
}

// Retry executes fn up to attempts times (default 3) with exponential
// backoff (200ms, 400ms, 800ms...) between attempts. Each attempt is
// breaker-guarded. Returns the last error.
func (b *Breaker) Retry(attempts int, fn func() error) error {
	if attempts <= 0 {
		attempts = 3
	}
	var err error
	backoff := 200 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		err = b.Do(fn)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrCircuitOpen) {
			return err
		}
	}
	return err
}
