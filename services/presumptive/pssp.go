package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
)

// PSSP adapter interface (SPEC §4 T12: Remita/eTranzact/Flutterwave-class
// authorise -> capture/void with webhook callbacks).

type AuthoriseRequest struct {
	PaymentID  string `json:"payment_id"`
	AmountKobo uint64 `json:"amount_kobo"`
	PayerRef   string `json:"payer_ref"` // tin_hash or phone
	Narration  string `json:"narration"`
}

type AuthoriseResponse struct {
	Reference string `json:"reference"` // provider ref (RRR / transaction id / tx_ref)
	Status    string `json:"status"`    // authorised|pending_mandate|failed
	Detail    string `json:"detail,omitempty"`
}

type CaptureResponse struct {
	Reference   string `json:"reference"`
	Status      string `json:"status"` // captured|failed
	SettledKobo uint64 `json:"settled_kobo"`
	Detail      string `json:"detail,omitempty"`
}

// PSSPAdapter is the payment-switch provider interface.
type PSSPAdapter interface {
	Name() string
	Authorise(req AuthoriseRequest) (AuthoriseResponse, error)
	Capture(reference string, amountKobo uint64) (CaptureResponse, error)
	Void(reference string) error
}

// psspSim is the shared deterministic simulator core; each provider differs in
// reference format and fee shape only. References ending in "X" simulate a
// declined authorisation to exercise failure paths.
type psspSim struct {
	name    string
	refFmt  string
	feeBps  uint64
	mu      sync.Mutex
	authed  map[string]uint64 // reference -> amount
	settled map[string]bool
}

func newPSSPSim(name, refFmt string, feeBps uint64) *psspSim {
	return &psspSim{name: name, refFmt: refFmt, feeBps: feeBps, authed: map[string]uint64{}, settled: map[string]bool{}}
}

func (s *psspSim) Name() string { return s.name }

func (s *psspSim) Authorise(req AuthoriseRequest) (AuthoriseResponse, error) {
	if req.AmountKobo == 0 {
		return AuthoriseResponse{Status: "failed", Detail: "amount must be > 0"}, nil
	}
	ref := fmt.Sprintf(s.refFmt, strings.ToUpper(ids.New()[:16]))
	// deterministic failure path for testing: payer_ref "DECLINE"
	if strings.Contains(strings.ToUpper(req.PayerRef), "DECLINE") {
		return AuthoriseResponse{Reference: ref, Status: "failed", Detail: "simulated issuer decline"}, nil
	}
	s.mu.Lock()
	s.authed[ref] = req.AmountKobo
	s.mu.Unlock()
	return AuthoriseResponse{Reference: ref, Status: "authorised", Detail: s.name + " simulator authorisation"}, nil
}

func (s *psspSim) Capture(reference string, amountKobo uint64) (CaptureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	amt, ok := s.authed[reference]
	if !ok {
		return CaptureResponse{Reference: reference, Status: "failed", Detail: "unknown or expired authorisation"}, nil
	}
	if amountKobo == 0 || amountKobo > amt {
		amountKobo = amt
	}
	if s.settled[reference] {
		return CaptureResponse{Reference: reference, Status: "failed", Detail: "already captured"}, nil
	}
	fee := amountKobo * s.feeBps / 10000
	s.settled[reference] = true
	delete(s.authed, reference)
	return CaptureResponse{Reference: reference, Status: "captured", SettledKobo: amountKobo - fee, Detail: fmt.Sprintf("%s fee %d kobo (%d bps)", s.name, fee, s.feeBps)}, nil
}

func (s *psspSim) Void(reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.authed[reference]; !ok {
		return fmt.Errorf("%s: authorisation %s not found or already settled", s.name, reference)
	}
	delete(s.authed, reference)
	return nil
}

// PSSPHub routes to provider adapters.
type PSSPHub struct{ adapters map[string]PSSPAdapter }

// NewPSSPHub wires provider adapters per H1: PSSP_API_URL set → the real
// signed HTTP adapter is registered as provider "pssp" (profile=prod);
// the deterministic provider simulators always remain available for dev
// and for side-by-side testing.
func NewPSSPHub() *PSSPHub {
	adapters := map[string]PSSPAdapter{
		"remita":      newPSSPSim("remita", "RRR-%s", 100),      // 1.00% fee
		"etranzact":   newPSSPSim("etranzact", "ETZ-%s", 75),    // 0.75%
		"flutterwave": newPSSPSim("flutterwave", "FLW-%s", 140), // 1.40%
	}
	if u := os.Getenv("PSSP_API_URL"); u != "" {
		log.Printf("profile=prod component=pssp-adapter url=%s", u)
		adapters["pssp"] = NewPSSPHTTPAdapter(u, os.Getenv("PSSP_API_KEY"))
	} else {
		log.Printf("profile=dev component=pssp-adapter (simulators)")
	}
	return &PSSPHub{adapters: adapters}
}

func (h *PSSPHub) Adapter(provider string) (PSSPAdapter, error) {
	a, ok := h.adapters[strings.ToLower(provider)]
	if !ok {
		return nil, fmt.Errorf("unknown PSSP provider %q (have remita, etranzact, flutterwave)", provider)
	}
	return a, nil
}

// webhookSecret is the shared secret for PSSP webhook HMAC signatures:
// PSSP_API_KEY (prod) → PSSP_WEBHOOK_SECRET → dev default.
func webhookSecret() string {
	if s := os.Getenv("PSSP_API_KEY"); s != "" {
		return s
	}
	if s := os.Getenv("PSSP_WEBHOOK_SECRET"); s != "" {
		return s
	}
	return "meridian-dev-pssp-webhook-secret"
}

// hmacHex computes hex HMAC-SHA256(key, value).
func hmacHex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature validates X-PSSP-Signature: hex HMAC-SHA256(secret, body).
func VerifyWebhookSignature(signature string, body []byte) bool {
	if signature == "" {
		return false
	}
	want := hmacHex(webhookSecret(), string(body))
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}
