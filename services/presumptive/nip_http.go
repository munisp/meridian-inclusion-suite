package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/resilience"
)

// nip_http.go — [REAL] NIP rail adapter skeleton.
//
// Signed HTTP client for the NIBSS Instant Payments rail. Transport
// security and request signing follow NIBSS conventions:
//
//   - mTLS: client certificate from NIP_TLS_CERT_FILE / NIP_TLS_KEY_FILE,
//     optional CA bundle NIP_TLS_CA_FILE (NIBSS issues participant certs at
//     onboarding). When cert envs are unset the adapter falls back to plain
//     TLS (dev against a mock endpoint only).
//   - Every request carries:
//     Authorization: Bearer <NIP_API_KEY>        (participant credential)
//     X-Signature:   hex HMAC-SHA256(NIP_API_KEY, raw-body)
//     X-NIP-Session-Id: <session id>             (end-to-end trace key)
//     [PLACEHOLDER] NIBSS production signing uses the participant's private
//     key (asymmetric) over a canonical request string; swap signRequest()
//     when the official credential + signing spec are issued. The header
//     names and placement are stable.
//
// Endpoint map (NIBSS NIP REST conventions; paths confirmed at onboarding):
//
//	POST /name-enquiry          -> NameEnquiry
//	POST /funds-transfer        -> FundsTransfer
//	POST /transaction-status    -> TransactionStatusQuery (TSQ)
//	POST /reversal              -> Reversal
//	GET  /healthz               -> Probe
//
// Retries: idempotent queries (name-enquiry, TSQ, health) get 3-attempt
// exponential backoff + circuit breaker (5 failures -> open 30s).
// funds-transfer gets EXACTLY ONE attempt (B3 #18): TSQ — never blind
// retry — is the recovery mechanism for ambiguous transfer outcomes (see
// nip_recon.go).
type NIPHTTPAdapter struct {
	base   string
	apiKey string
	hc     *http.Client
	brk    *resilience.Breaker
}

// NewNIPHTTPAdapter builds the [REAL] adapter, configuring mTLS when the
// NIP_TLS_* envs are present.
func NewNIPHTTPAdapter(base, apiKey string) (*NIPHTTPAdapter, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	certFile, keyFile := os.Getenv("NIP_TLS_CERT_FILE"), os.Getenv("NIP_TLS_KEY_FILE")
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("nip mTLS keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if caFile := os.Getenv("NIP_TLS_CA_FILE"); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("nip CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("nip CA bundle %s: no certificates parsed", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &NIPHTTPAdapter{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		hc: &http.Client{
			Timeout:   30 * time.Second, // NIP transfers can legitimately take seconds; TSQ resolves timeouts
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		brk: &resilience.Breaker{Threshold: 5, Cooldown: 30 * time.Second},
	}, nil
}

func (a *NIPHTTPAdapter) Name() string { return "nip-live" }

// Probe liveness-checks the rail (used by fail-closed selection).
func (a *NIPHTTPAdapter) Probe() error {
	req, err := http.NewRequest(http.MethodGet, a.base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("rail health: status %d", resp.StatusCode)
	}
	return nil
}

// post issues one signed JSON POST to the NIP endpoint with bounded
// retries — safe ONLY for idempotent queries (name-enquiry, TSQ, health).
func (a *NIPHTTPAdapter) post(path, sessionID string, payload any, out any) error {
	return a.postAttempt(path, sessionID, payload, out, true)
}

// postOnce issues exactly ONE signed POST (breaker-guarded, no retry).
// B3 #18: funds-transfer MUST use this path — a retried transfer POST is a
// blind retry that can double-debit; ambiguous outcomes are resolved by
// the TSQ sweeper (never-blind-retry doctrine, see nip_recon.go).
func (a *NIPHTTPAdapter) postOnce(path, sessionID string, payload any, out any) error {
	return a.postAttempt(path, sessionID, payload, out, false)
}

func (a *NIPHTTPAdapter) postAttempt(path, sessionID string, payload any, out any, retryable bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	attempt := func() error {
		req, err := http.NewRequest(http.MethodPost, a.base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if sessionID != "" {
			req.Header.Set("X-NIP-Session-Id", sessionID)
		}
		a.signRequest(req, body)
		resp, err := a.hc.Do(req)
		if err != nil {
			return fmt.Errorf("nip adapter: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("nip adapter: status %d: %s", resp.StatusCode, string(b))
		}
		if out == nil {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("nip adapter: decode: %w", err)
		}
		return nil
	}
	if retryable {
		return a.brk.Retry(3, attempt)
	}
	return a.brk.Do(attempt)
}

// signRequest applies the NIBSS-convention credential + signature headers.
// [PLACEHOLDER] swap the HMAC for the asymmetric participant signature when
// official NIBSS credentials are issued (see file header).
func (a *NIPHTTPAdapter) signRequest(req *http.Request, body []byte) {
	if a.apiKey == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("X-Signature", hmacHex(a.apiKey, string(body)))
}

// NameEnquiry verifies the beneficiary account: POST /name-enquiry.
func (a *NIPHTTPAdapter) NameEnquiry(account, bankCode string) (NameEnquiryResult, error) {
	var out NameEnquiryResult
	err := a.post("/name-enquiry", "", map[string]any{
		"account_number": account, "bank_code": bankCode,
	}, &out)
	if err != nil {
		log.Printf("nip adapter: name enquiry failed account=...%s: %v", tail4(account), err)
		return NameEnquiryResult{}, err
	}
	if out.AccountNumber == "" {
		out.AccountNumber = account
	}
	if out.BankCode == "" {
		out.BankCode = bankCode
	}
	out.Verified = out.Verified || out.AccountName != ""
	return out, nil
}

// FundsTransfer moves value to a verified beneficiary: POST /funds-transfer.
// B3 #18: exactly one dispatch attempt — no blind retry (double-debit risk).
func (a *NIPHTTPAdapter) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	var out NIPTransferResult
	err := a.postOnce("/funds-transfer", req.SessionID, req, &out)
	if err != nil {
		// transport failure after dispatch is the classic ambiguous case:
		// report in_flight so the TSQ sweeper resolves it — NEVER retry
		// blindly (risk of double debit).
		log.Printf("nip adapter: funds transfer ambiguous session=%s: %v", req.SessionID, err)
		return NIPTransferResult{SessionID: req.SessionID, Status: NIPStatusInFlight, Detail: "transport error: " + err.Error()}, nil
	}
	if out.SessionID == "" {
		out.SessionID = req.SessionID
	}
	if out.Status == "" {
		out.Status = NIPStatusSuccess
	}
	return out, nil
}

// TransactionStatusQuery resolves an ambiguous transfer: POST /transaction-status.
func (a *NIPHTTPAdapter) TransactionStatusQuery(sessionID string) (NIPTransferResult, error) {
	var out NIPTransferResult
	err := a.post("/transaction-status", sessionID, map[string]any{"session_id": sessionID}, &out)
	if err != nil {
		return NIPTransferResult{}, err
	}
	if out.SessionID == "" {
		out.SessionID = sessionID
	}
	return out, nil
}

// Reversal unwinds a failed/errored transfer: POST /reversal. Reversal is
// NOT a refund (see the CBN regulatory note in nip.go).
func (a *NIPHTTPAdapter) Reversal(originalSessionID, reason string) (NIPTransferResult, error) {
	var out NIPTransferResult
	err := a.post("/reversal", originalSessionID, map[string]any{
		"original_session_id": originalSessionID, "reason": reason,
	}, &out)
	if err != nil {
		return NIPTransferResult{}, err
	}
	if out.SessionID == "" {
		out.SessionID = originalSessionID
	}
	if out.Status == "" {
		out.Status = NIPStatusReversed
	}
	return out, nil
}

// tail4 returns the last 4 chars of an account number for logs (never log
// full account numbers).
func tail4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
