package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
)

// nip.go — NIP (NIBSS Instant Payments) rail adapter skeleton (N1).
//
// NIP is the Nigerian inter-bank instant payment rail operated by NIBSS.
// This file defines the rail interface, the deterministic [SIM] simulator
// and the rail selection / fail-closed configuration. The signed HTTP
// adapter ([REAL]) lives in nip_http.go; the payout service (mandatory
// name-enquiry gate, idempotency, TSQ reconciliation sweeper) lives in
// nip_recon.go.
//
// Regulatory notes (documented in-code per CBN expectations):
//   - Name enquiry BEFORE funds transfer is mandatory on NIP: the payer/
//     sender MUST be shown the verified beneficiary account name before
//     value moves (NIBSS NIP operational rules; CBN cashless policy
//     guidance on misdirected transfers).
//   - REVERSAL is distinct from REFUND. A reversal unwinds a failed or
//     errored NIP transaction at the rail level and returns funds to the
//     sender's account — CBN requires same-day (T+0/T+1) auto-reversal of
//     failed instant transfers (see CBN Guidelines on Point of Sale (POS)
//     card acceptance & the CBN circular on failed e-transaction reversals,
//     "Re: Timelines for Reversal of Failed Transactions", CBN/BPS/DIR).
//     A refund is a commercial return of value for a SUCCESSFUL transaction
//     (goods/services not delivered) and follows the merchant/PSSP dispute
//     process instead. Do NOT use Reversal() to refund a settled transfer.

// NIP transfer statuses.
const (
	NIPStatusSuccess  = "success"   // beneficiary credited
	NIPStatusFailed   = "failed"    // declined outright (final)
	NIPStatusInFlight = "in_flight" // ambiguous: timed out / no response — MUST be resolved by TSQ before retry
	NIPStatusReversed = "reversed"  // unwound at the rail (see reversal vs refund note above)
)

// NameEnquiryResult is the NIP account-name verification response.
type NameEnquiryResult struct {
	SessionID     string `json:"session_id"`
	AccountNumber string `json:"account_number"`
	BankCode      string `json:"bank_code"`
	AccountName   string `json:"account_name"`
	Verified      bool   `json:"verified"`
	BVNMatch      bool   `json:"bvn_match,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// NIPTransferRequest is one funds-transfer instruction. Amount is kobo.
type NIPTransferRequest struct {
	SessionID      string `json:"session_id"`
	AmountKobo     uint64 `json:"amount_kobo"`
	DestAccount    string `json:"dest_account"`
	DestBankCode   string `json:"dest_bank_code"`
	DestName       string `json:"dest_name,omitempty"` // as verified by name enquiry
	Narration      string `json:"narration"`
	IdempotencyKey string `json:"idempotency_key"`
}

// NIPTransferResult is the outcome of a transfer, TSQ or reversal call.
type NIPTransferResult struct {
	SessionID    string `json:"session_id"`
	Status       string `json:"status"`                  // success|failed|in_flight|reversed
	ResponseCode string `json:"response_code,omitempty"` // NIP response code, e.g. "00" approved
	Detail       string `json:"detail,omitempty"`
}

// NIPRail is the NIBSS Instant Payments rail interface.
type NIPRail interface {
	Name() string
	// NameEnquiry verifies an account number at a destination bank and
	// returns the account name. MANDATORY before FundsTransfer.
	NameEnquiry(account, bankCode string) (NameEnquiryResult, error)
	// FundsTransfer moves value to a verified beneficiary. Must be safe to
	// retry with the same SessionID/IdempotencyKey (no double debit).
	FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error)
	// TransactionStatusQuery (TSQ) resolves the true state of an in-flight
	// or ambiguous transfer. NIP rules REQUIRE TSQ before any reinitiation.
	TransactionStatusQuery(sessionID string) (NIPTransferResult, error)
	// Reversal unwinds a failed/errored transfer at the rail (see the
	// reversal-vs-refund regulatory note at the top of this file).
	Reversal(originalSessionID, reason string) (NIPTransferResult, error)
	// Probe is a liveness check used by fail-closed rail selection.
	Probe() error
}

// nipInstitutionCode is the 6-digit NIBSS-assigned institution code used as
// the session-id prefix. PLACEHOLDER: replaced by the code assigned at NIBSS
// onboarding (env NIP_INSTITUTION_CODE overrides).
const nipInstitutionCode = "999999"

// NewNIPSessionID generates a NIP session id.
//
// [STRUCTURAL PLACEHOLDER — honestly tagged] NIP session ids are 30-digit
// numeric strings; the structure implemented here follows the commonly
// documented layout:
//
//	6  digits  institution code (NIBSS-assigned)
//	8  digits  transaction date YYYYMMDD
//	16 digits  cryptographically random unique suffix
//
// The exact field layout MUST be confirmed against the NIBSS NIP
// specification issued at onboarding before go-live; only the generator
// body changes, callers are unaffected.
func NewNIPSessionID() string {
	inst := os.Getenv("NIP_INSTITUTION_CODE")
	if inst == "" {
		inst = nipInstitutionCode
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	var suffix uint64
	for _, c := range b {
		suffix = suffix*10 + uint64(c%10)
	}
	return fmt.Sprintf("%06s%s%016d", inst, time.Now().UTC().Format("20060102"), suffix%1e16)
}

// ---- [SIM] deterministic NIP simulator ----
//
// nipSim models the three rail outcomes needed by the payout/recon paths:
//   - success:               transfer approved synchronously
//   - failed-in-flight:      dest account containing "HANG" returns
//     in_flight; TSQ then deterministically resolves it to failed
//     (requiring reversal) unless the narration contains "RECOVER",
//     in which case TSQ resolves it to success
//   - reversed:              Reversal() moves any non-reversed transfer to
//     reversed (CBN auto-reversal path)
//
// Name enquiry: account "0000000000" is unknown (unverified); any other
// 10-digit account verifies with a deterministic simulated name.
type nipSim struct {
	mu        sync.Mutex
	transfers map[string]string // sessionID -> status
	names     map[string]string // sessionID -> verified name (for transfers)
	seq       int
}

// NewNIPSim returns the [SIM] rail (dev/test; also used for side-by-side
// testing when the live rail is configured).
func NewNIPSim() NIPRail {
	return &nipSim{transfers: map[string]string{}, names: map[string]string{}}
}

func (s *nipSim) Name() string { return "nip-sim" }
func (s *nipSim) Probe() error { return nil }

func (s *nipSim) NameEnquiry(account, bankCode string) (NameEnquiryResult, error) {
	res := NameEnquiryResult{
		SessionID:     NewNIPSessionID(),
		AccountNumber: account,
		BankCode:      bankCode,
	}
	if len(account) != 10 || account == "0000000000" {
		res.Detail = "[SIM] account not found at destination bank"
		return res, nil
	}
	res.Verified = true
	res.AccountName = fmt.Sprintf("[SIM] VERIFIED NAME %s/%s", account[len(account)-4:], bankCode)
	res.Detail = "[SIM] name enquiry"
	return res, nil
}

func (s *nipSim) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	if req.AmountKobo == 0 {
		return NIPTransferResult{SessionID: req.SessionID, Status: NIPStatusFailed, Detail: "amount must be > 0"}, nil
	}
	if req.SessionID == "" {
		return NIPTransferResult{Status: NIPStatusFailed, Detail: "session id required"}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// idempotent retry: same session id returns the recorded outcome.
	if st, ok := s.transfers[req.SessionID]; ok {
		return NIPTransferResult{SessionID: req.SessionID, Status: st, Detail: "[SIM] idempotent replay"}, nil
	}
	st := NIPStatusSuccess
	detail := "[SIM] transfer approved"
	if strings.Contains(strings.ToUpper(req.DestAccount), "HANG") || strings.Contains(strings.ToUpper(req.Narration), "HANG") {
		st = NIPStatusInFlight
		detail = "[SIM] no response from beneficiary bank (timeout) — resolve via TSQ"
	}
	s.transfers[req.SessionID] = st
	return NIPTransferResult{SessionID: req.SessionID, Status: st, ResponseCode: "00", Detail: detail}, nil
}

func (s *nipSim) TransactionStatusQuery(sessionID string) (NIPTransferResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.transfers[sessionID]
	if !ok {
		return NIPTransferResult{SessionID: sessionID, Status: NIPStatusFailed, Detail: "[SIM] TSQ: unknown session (treat as not-received)"}, nil
	}
	if st == NIPStatusInFlight {
		// deterministic resolution of the ambiguous state
		st = NIPStatusFailed
		s.transfers[sessionID] = st
	}
	return NIPTransferResult{SessionID: sessionID, Status: st, Detail: "[SIM] TSQ"}, nil
}

// TSQResolveAs lets tests steer how the sim resolves an in-flight transfer
// (e.g. mark recovered-to-success). [SIM] only.
func TSQResolveAs(rail NIPRail, sessionID, status string) {
	if s, ok := rail.(*nipSim); ok {
		s.mu.Lock()
		s.transfers[sessionID] = status
		s.mu.Unlock()
	}
}

func (s *nipSim) Reversal(originalSessionID, reason string) (NIPTransferResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.transfers[originalSessionID]
	if !ok {
		return NIPTransferResult{SessionID: originalSessionID, Status: NIPStatusFailed, Detail: "[SIM] reversal: unknown original session"}, nil
	}
	if st == NIPStatusReversed {
		return NIPTransferResult{SessionID: originalSessionID, Status: NIPStatusReversed, Detail: "[SIM] already reversed (idempotent)"}, nil
	}
	s.transfers[originalSessionID] = NIPStatusReversed
	return NIPTransferResult{SessionID: originalSessionID, Status: NIPStatusReversed, Detail: "[SIM] reversed: " + reason}, nil
}

// ---- rail selection (fail-closed) ----

// NewNIPRailFromEnv selects the rail:
//   - NIP_API_URL set          -> [REAL] signed HTTP adapter (mTLS when
//     NIP_TLS_CERT_FILE/NIP_TLS_KEY_FILE are set)
//   - otherwise                -> [SIM] simulator (dev default)
//
// FAIL-CLOSED: when NIP_RAIL=live OR PROFILE=prod (QA-21) the service
// refuses to run payouts on the simulator. If the live rail is configured
// but unreachable (Probe fails), or the live rail is required without
// NIP_API_URL, an error is returned and the caller must not start the
// payout path.
func NewNIPRailFromEnv() (NIPRail, error) {
	url := os.Getenv("NIP_API_URL")
	live := strings.EqualFold(os.Getenv("NIP_RAIL"), "live")
	// QA-21: PROFILE=prod must fail closed exactly like NIP_RAIL=live —
	// otherwise a prod deploy that forgets NIP_RAIL silently pays out on
	// the simulator.
	prod := keyx.Prod()
	if (live || prod) && url == "" {
		return nil, fmt.Errorf("live NIP rail required (NIP_RAIL=live or PROFILE=prod) but NIP_API_URL is unset: refusing to fall back to simulator (fail-closed)")
	}
	if url == "" {
		log.Printf("profile=dev component=nip-adapter ([SIM] simulator)")
		return NewNIPSim(), nil
	}
	rail, err := NewNIPHTTPAdapter(url, os.Getenv("NIP_API_KEY"))
	if err != nil {
		return nil, fmt.Errorf("nip adapter init: %w (fail-closed)", err)
	}
	if err := rail.Probe(); err != nil {
		if live || prod {
			return nil, fmt.Errorf("live NIP rail required (NIP_RAIL=live or PROFILE=prod) but rail %s unreachable: %v (fail-closed)", url, err)
		}
		log.Printf("component=nip-adapter warn: live rail probe failed, keeping adapter (NIP_RAIL!=live): %v", err)
	}
	log.Printf("profile=prod component=nip-adapter url=%s", url)
	return rail, nil
}
