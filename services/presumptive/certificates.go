package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// certHMACKey signs certificate payloads (SPEC §4 T12: HMAC-signed payload).
// Resolved via keyx: fails closed with no dev default in profile=prod.
func certHMACKey() string {
	return keyx.MustKey("CERT_HMAC_KEY", "meridian-dev-cert-key")
}

// certSerial issues human-verifiable serials: PSM-YYYY-XXXXXXXXXX.
// Deterministic per payment id so certificate issuance is idempotent: a
// retried/recovered capture re-issues the SAME certificate instead of a
// duplicate serial for one payment.
func certSerial(paymentID string) string {
	sum := sha256.Sum256([]byte("psm-cert:" + paymentID))
	return fmt.Sprintf("PSM-%d-%s", time.Now().UTC().Year(), strings.ToUpper(hex.EncodeToString(sum[:])[:10]))
}

// canonicalCertPayload is the signed byte string (pipe-delimited, stable).
func canonicalCertPayload(c Certificate) string {
	return strings.Join([]string{
		c.Serial, c.TINHash, c.State, c.Band,
		fmt.Sprint(c.AmountKobo), c.Period, c.PaymentID, c.IssuedAt, c.RulePackVersion,
	}, "|")
}

// SignCertificate HMAC-signs the canonical payload.
func SignCertificate(c Certificate) string {
	mac := hmac.New(sha256.New, []byte(certHMACKey()))
	mac.Write([]byte(canonicalCertPayload(c)))
	return hex.EncodeToString(mac.Sum(nil))
}

// CertificateService issues + verifies certificates.
type CertificateService struct {
	st *store.Store
}

func NewCertificateService(st *store.Store) *CertificateService {
	return &CertificateService{st: st}
}

// Issue mints a certificate for a captured payment. Idempotent per payment:
// if a certificate already exists for p.ID it is returned unchanged.
func (s *CertificateService) Issue(p Payment) (Certificate, error) {
	serial := certSerial(p.ID)
	var existing Certificate
	if ok, err := s.st.Get("certificates", serial, &existing); err == nil && ok && existing.PaymentID == p.ID {
		return existing, nil // idempotent replay
	}
	c := Certificate{
		Serial:          serial,
		TINHash:         p.TINHash,
		State:           p.State,
		Band:            p.TurnoverBand,
		AmountKobo:      p.AmountKobo,
		Period:          p.Period,
		PaymentID:       p.ID,
		IssuedAt:        nowRFC3339(),
		RulePackVersion: p.RulePackVersion,
	}
	c.Signature = SignCertificate(c)
	if err := s.st.Put("certificates", c.Serial, c); err != nil {
		return Certificate{}, err
	}
	return c, nil
}

// Verify checks a serial: certificate exists AND signature is valid.
func (s *CertificateService) Verify(serial string) (Certificate, bool, error) {
	var c Certificate
	ok, err := s.st.Get("certificates", strings.ToUpper(strings.TrimSpace(serial)), &c)
	if err != nil || !ok {
		return Certificate{}, false, err
	}
	valid := hmac.Equal([]byte(c.Signature), []byte(SignCertificate(c)))
	return c, valid, nil
}

// RateLimiter is a simple per-key token bucket (for the public verify endpoint).
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	perSec   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter: capacity tokens, refilled at perSec.
func NewRateLimiter(capacity, perSec float64) *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, capacity: capacity, perSec: perSec}
}

// Allow consumes one token for key; false when exhausted (HTTP 429).
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.capacity, last: time.Now()}
		r.buckets[key] = b
	}
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * r.perSec
	if b.tokens > r.capacity {
		b.tokens = r.capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
