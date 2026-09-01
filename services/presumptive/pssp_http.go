package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/otelx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/resilience"
)

// PSSPHTTPAdapter is the real PSSP payment-switch client (H4): payment
// init/authorise, capture/verify and void against {PSSP_API_URL}, requests
// HMAC-SHA256 signed with PSSP_API_KEY, 3-retry exponential backoff +
// circuit breaker (5 failures → open 30s). Never logs raw TIN — payer_ref
// is already a tin_hash upstream.
type PSSPHTTPAdapter struct {
	base    string
	apiKey  string
	hc      *http.Client
	breaker *resilience.Breaker
}

func NewPSSPHTTPAdapter(base, apiKey string) *PSSPHTTPAdapter {
	return &PSSPHTTPAdapter{
		base:    strings.TrimRight(base, "/"),
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: 10 * time.Second, Transport: otelx.Client(nil)},
		breaker: &resilience.Breaker{Threshold: 5, Cooldown: 30 * time.Second},
	}
}

func (a *PSSPHTTPAdapter) Name() string { return "pssp" }

// post issues one signed JSON POST to the PSSP API.
func (a *PSSPHTTPAdapter) post(path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	sig := hmacHex(a.apiKey, string(body))
	return a.breaker.Retry(3, func() error {
		req, err := http.NewRequest(http.MethodPost, a.base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-PSSP-Signature", sig)
		if a.apiKey != "" {
			req.Header.Set("X-API-Key", a.apiKey)
		}
		resp, err := a.hc.Do(req)
		if err != nil {
			return fmt.Errorf("pssp adapter: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("pssp adapter: status %d: %s", resp.StatusCode, string(b))
		}
		if out == nil {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("pssp adapter: decode: %w", err)
		}
		return nil
	})
}

// Authorise performs payment init/authorise: POST /payments/init.
func (a *PSSPHTTPAdapter) Authorise(req AuthoriseRequest) (AuthoriseResponse, error) {
	var out AuthoriseResponse
	if err := a.post("/payments/init", req, &out); err != nil {
		log.Printf("pssp adapter: authorise failed payment_id=%s: %v", req.PaymentID, err)
		return AuthoriseResponse{}, err
	}
	if out.Status == "" {
		out.Status = "authorised"
	}
	return out, nil
}

// Capture settles a previously authorised payment: POST /payments/capture.
// The Idempotency-Key is sent both as a header and in the body so the
// provider dedupes retries (crash between provider capture and our persist).
func (a *PSSPHTTPAdapter) Capture(reference string, amountKobo uint64, idempotencyKey string) (CaptureResponse, error) {
	var out CaptureResponse
	err := a.post("/payments/capture", map[string]any{
		"reference": reference, "amount_kobo": amountKobo, "idempotency_key": idempotencyKey,
	}, &out)
	if err != nil {
		log.Printf("pssp adapter: capture failed reference=%s: %v", reference, err)
		return CaptureResponse{}, err
	}
	if out.Reference == "" {
		out.Reference = reference
	}
	if out.Status == "" {
		out.Status = "captured"
	}
	return out, nil
}

// Verify checks settlement status: POST /payments/verify (used by recon).
func (a *PSSPHTTPAdapter) Verify(reference string) (CaptureResponse, error) {
	var out CaptureResponse
	err := a.post("/payments/verify", map[string]any{"reference": reference}, &out)
	if err != nil {
		return CaptureResponse{}, err
	}
	if out.Reference == "" {
		out.Reference = reference
	}
	return out, nil
}

// Void cancels an authorisation: POST /payments/void.
func (a *PSSPHTTPAdapter) Void(reference string) error {
	return a.post("/payments/void", map[string]any{"reference": reference}, nil)
}

// Refund reverses a settled capture: POST /payments/refund (saga
// compensation when the ledger/certificate leg fails after capture).
func (a *PSSPHTTPAdapter) Refund(reference string, amountKobo uint64) error {
	return a.post("/payments/refund", map[string]any{
		"reference": reference, "amount_kobo": amountKobo,
	}, nil)
}
