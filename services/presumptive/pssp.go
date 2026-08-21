package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
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
	Status      string `json:"status"`                // authorised|captured|failed
	AmountKobo  uint64 `json:"amount_kobo,omitempty"` // gross transaction amount (verify)
	SettledKobo uint64 `json:"settled_kobo"`
	Currency    string `json:"currency,omitempty"` // ISO 4217; NGN for all in-scope PSSPs
	Detail      string `json:"detail,omitempty"`
}

// PSSPAdapter is the payment-switch provider interface.
type PSSPAdapter interface {
	Name() string
	Authorise(req AuthoriseRequest) (AuthoriseResponse, error)
	// Capture settles an authorisation. idempotencyKey dedupes retries at
	// the provider: a replayed key returns the original capture result
	// instead of a second charge.
	Capture(reference string, amountKobo uint64, idempotencyKey string) (CaptureResponse, error)
	// Verify re-checks a transaction server-side (Paystack/Flutterwave both
	// mandate verify-before-fulfil on webhooks). Returns the provider's own
	// view of status/amount/currency for the reference.
	Verify(reference string) (CaptureResponse, error)
	Void(reference string) error
	// Refund reverses a settled capture (saga compensation).
	Refund(reference string, amountKobo uint64) error
}

// FeeSchedule is a percent-with-naira-cap fee config (G12). Real Nigerian
// fee schedules are never uncapped-linear: CBN's Guide to Charges fixes the
// Merchant Service Charge at 0.5% capped (₦2,000 documented norm for local
// cards; the 2026 exposure draft raises the cap), and commercial PSSP pricing
// is ~1.4-1.5% capped ₦2,000. Uncapped linear fees silently diverge from the
// provider's actual settlement on large levies and corrupt FeeKobo + recon.
type FeeSchedule struct {
	RateBps uint64 `json:"rate_bps"` // basis points of the gross amount
	CapKobo uint64 `json:"cap_kobo"` // maximum fee per transaction (0 = uncapped)
}

// DefaultMSCSchedule is the documented CBN MSC norm for local cards
// (0.5%, capped ₦2,000) — kept configurable per provider.
var DefaultMSCSchedule = FeeSchedule{RateBps: 50, CapKobo: 200000}

// feeScheduleFor resolves a provider's fee schedule: registered default,
// overridable via PSSP_FEE_RATE_BPS_<PROVIDER> / PSSP_FEE_CAP_KOBO_<PROVIDER>
// so the agreed schedule of a real provider can be configured (and validated
// against) without a code change.
func feeScheduleFor(provider string, def FeeSchedule) FeeSchedule {
	p := strings.ToUpper(provider)
	if v := os.Getenv("PSSP_FEE_RATE_BPS_" + p); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			def.RateBps = n
		}
	}
	if v := os.Getenv("PSSP_FEE_CAP_KOBO_" + p); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			def.CapKobo = n
		}
	}
	return def
}

// Fee computes the capped fee for a gross amount (integer kobo).
func (f FeeSchedule) Fee(amountKobo uint64) uint64 {
	fee := amountKobo * f.RateBps / 10000
	if f.CapKobo > 0 && fee > f.CapKobo {
		return f.CapKobo
	}
	return fee
}

// psspSim is the shared deterministic simulator core; each provider differs in
// reference format and fee shape only. References ending in "X" simulate a
// declined authorisation to exercise failure paths.
type psspSim struct {
	name    string
	refFmt  string
	fee     FeeSchedule
	mu      sync.Mutex
	authed  map[string]uint64 // reference -> amount
	settled map[string]bool
	setAmt  map[string]uint64 // reference -> captured gross amount
	capKeys map[string]string // reference -> idempotency key used at capture
}

// simCurrency is the only currency the simulated switches settle in.
const simCurrency = "NGN"

func newPSSPSim(name, refFmt string, fee FeeSchedule) *psspSim {
	return &psspSim{name: name, refFmt: refFmt, fee: fee, authed: map[string]uint64{}, settled: map[string]bool{}, setAmt: map[string]uint64{}, capKeys: map[string]string{}}
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

func (s *psspSim) Capture(reference string, amountKobo uint64, idempotencyKey string) (CaptureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled[reference] {
		// Idempotent replay: same key => return the original capture result
		// (real providers do this via their Idempotency-Key support).
		if idempotencyKey != "" && s.capKeys[reference] == idempotencyKey {
			amt := amountKobo
			if amt == 0 {
				amt = s.setAmt[reference] // original captured gross
			}
			fee := s.fee.Fee(amt)
			return CaptureResponse{Reference: reference, Status: "captured", AmountKobo: amt, SettledKobo: amt - fee, Currency: simCurrency, Detail: s.name + " idempotent replay"}, nil
		}
		return CaptureResponse{Reference: reference, Status: "failed", Detail: "already captured"}, nil
	}
	amt, ok := s.authed[reference]
	if !ok {
		return CaptureResponse{Reference: reference, Status: "failed", Detail: "unknown or expired authorisation"}, nil
	}
	if amountKobo == 0 || amountKobo > amt {
		amountKobo = amt
	}
	fee := s.fee.Fee(amountKobo)
	s.settled[reference] = true
	s.setAmt[reference] = amountKobo
	s.capKeys[reference] = idempotencyKey
	delete(s.authed, reference)
	return CaptureResponse{Reference: reference, Status: "captured", AmountKobo: amountKobo, SettledKobo: amountKobo - fee, Currency: simCurrency, Detail: fmt.Sprintf("%s fee %d kobo (%d bps, cap %d)", s.name, fee, s.fee.RateBps, s.fee.CapKobo)}, nil
}

// Verify [SIM] re-checks a transaction server-side, mirroring the Paystack /
// Flutterwave verify-transaction endpoint the webhook handler calls before
// giving value (G1). Returns the simulator's own view of the transaction.
func (s *psspSim) Verify(reference string) (CaptureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if amt, ok := s.authed[reference]; ok {
		return CaptureResponse{Reference: reference, Status: "authorised", AmountKobo: amt, Currency: simCurrency, Detail: s.name + " verify: authorised"}, nil
	}
	if s.settled[reference] {
		amt := s.setAmt[reference]
		fee := s.fee.Fee(amt)
		return CaptureResponse{Reference: reference, Status: "captured", AmountKobo: amt, SettledKobo: amt - fee, Currency: simCurrency, Detail: s.name + " verify: captured"}, nil
	}
	return CaptureResponse{Reference: reference, Status: "failed", Currency: simCurrency, Detail: s.name + ": unknown reference"}, nil
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

// Refund reverses a settled capture (saga compensation path).
func (s *psspSim) Refund(reference string, amountKobo uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settled[reference] {
		return fmt.Errorf("%s: capture %s not settled; nothing to refund", s.name, reference)
	}
	delete(s.settled, reference)
	return nil
}

// Webhook signature schemes (G3): real Nigerian PSSPs do not share one
// signing scheme — Paystack signs with HMAC-SHA512 keyed by the API secret,
// Flutterwave sends a shared-secret "verif-hash" equality header, Monnify
// uses HMAC-SHA512. The scheme is selectable per registered PSSP.
const (
	SchemeHMACSHA256 = "hmac-sha256" // generic (Meridian switch, Remita/e-BillsPay-class)
	SchemeHMACSHA512 = "hmac-sha512" // Paystack-compatible
	SchemeVerifHash  = "verif-hash"  // Flutterwave-compatible shared-secret header
)

// PSSPHub routes to provider adapters.
type PSSPHub struct {
	adapters       map[string]PSSPAdapter
	webhookSchemes map[string]string // provider -> signature scheme (G3)
}

// NewPSSPHub wires provider adapters per H1: PSSP_API_URL set → the real
// signed HTTP adapter is registered as provider "pssp" (profile=prod).
// The deterministic provider simulators are registered for PROFILE=dev
// only (QA-20): under profile=prod a simulator provider must never be
// selectable on the funds path, so prod refuses to boot without
// PSSP_API_URL (hard-fatal, same contract as the NIMC adapter).
func NewPSSPHub() *PSSPHub {
	adapters := map[string]PSSPAdapter{}
	if u := os.Getenv("PSSP_API_URL"); u != "" {
		log.Printf("profile=prod component=pssp-adapter url=%s", u)
		adapters["pssp"] = NewPSSPHTTPAdapter(u, os.Getenv("PSSP_API_KEY"))
	}
	if keyx.Prod() {
		if _, ok := adapters["pssp"]; !ok {
			log.Fatal("profile=prod FATAL: PSSP_API_URL is required (refusing to start with PSSP provider simulators selectable)")
		}
		log.Printf("profile=prod component=pssp-adapter (simulators disabled)")
	} else {
		// [SIM] schedules: commercial PSSP rates with the documented ₦2,000
		// cap (G12); env PSSP_FEE_RATE_BPS_<P> / PSSP_FEE_CAP_KOBO_<P> override.
		adapters["remita"] = newPSSPSim("remita", "RRR-%s", feeScheduleFor("remita", FeeSchedule{RateBps: 100, CapKobo: 200000}))           // 1.00% capped N2,000
		adapters["etranzact"] = newPSSPSim("etranzact", "ETZ-%s", feeScheduleFor("etranzact", FeeSchedule{RateBps: 75, CapKobo: 200000}))      // 0.75% capped N2,000
		adapters["flutterwave"] = newPSSPSim("flutterwave", "FLW-%s", feeScheduleFor("flutterwave", FeeSchedule{RateBps: 140, CapKobo: 200000})) // 1.40% capped N2,000
		log.Printf("profile=dev component=pssp-adapter (simulators)")
	}
	return &PSSPHub{
		adapters: adapters,
		webhookSchemes: map[string]string{
			// provider-compatible defaults (G3); anything not listed falls
			// back to hmac-sha256. Env PSSP_WEBHOOK_SCHEME_<PROVIDER> wins.
			"paystack":    SchemeHMACSHA512,
			"flutterwave": SchemeVerifHash,
		},
	}
}

// WebhookScheme resolves the signature scheme for a registered PSSP:
// PSSP_WEBHOOK_SCHEME_<PROVIDER> env override -> registered default ->
// hmac-sha256 (generic).
func (h *PSSPHub) WebhookScheme(provider string) string {
	p := strings.ToLower(provider)
	if s := os.Getenv("PSSP_WEBHOOK_SCHEME_" + strings.ToUpper(p)); s != "" {
		return strings.ToLower(s)
	}
	if s, ok := h.webhookSchemes[p]; ok {
		return s
	}
	return SchemeHMACSHA256
}

// VerifyWebhookSignatureFor validates the webhook signature under the
// provider's configured scheme. Empty signatures and unknown schemes fail
// closed. signature is the raw header value (X-PSSP-Signature for HMAC
// schemes, verif-hash for the Flutterwave scheme).
func (h *PSSPHub) VerifyWebhookSignatureFor(provider, signature string, body []byte) bool {
	if signature == "" {
		return false
	}
	secret := webhookSecretFor(provider)
	switch h.WebhookScheme(provider) {
	case SchemeHMACSHA256:
		want := hmacHex(secret, string(body))
		return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
	case SchemeHMACSHA512:
		mac := hmac.New(sha512.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
	case SchemeVerifHash:
		// Flutterwave verif-hash: shared-secret equality, constant time.
		return hmac.Equal([]byte(signature), []byte(secret))
	default:
		return false // unknown scheme: fail closed
	}
}

func (h *PSSPHub) Adapter(provider string) (PSSPAdapter, error) {
	a, ok := h.adapters[strings.ToLower(provider)]
	if !ok {
		avail := make([]string, 0, len(h.adapters))
		for name := range h.adapters {
			avail = append(avail, name)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("unknown PSSP provider %q (have %s)", provider, strings.Join(avail, ", "))
	}
	return a, nil
}

// webhookSecret is the shared secret for PSSP webhook HMAC signatures:
// PSSP_API_KEY (prod) → PSSP_WEBHOOK_SECRET → dev default (dev profile only,
// fail-closed in profile=prod via keyx).
func webhookSecret() string {
	if s := os.Getenv("PSSP_API_KEY"); s != "" {
		return s
	}
	return keyx.MustKey("PSSP_WEBHOOK_SECRET", "meridian-dev-pssp-webhook-secret")
}

// webhookSecretFor resolves the webhook secret per PSSP (G3):
// PSSP_WEBHOOK_SECRET_<PROVIDER> -> the shared webhookSecret() fallback
// (which itself fails closed in profile=prod via keyx).
func webhookSecretFor(provider string) string {
	if s := os.Getenv("PSSP_WEBHOOK_SECRET_" + strings.ToUpper(provider)); s != "" {
		return s
	}
	return webhookSecret()
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
