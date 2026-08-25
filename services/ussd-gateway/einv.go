package main

// I3: USSD -> e-invoice issuance. The taxpayer can issue an e-invoice
// (amount in kobo + buyer TIN) or query invoice status by IRN through the
// compliance-suite einvoicing service (POST /v1/invoices/nrs contract).
//
// Contract notes (from compliance services/einvoicing/nrs_handlers.go):
//   - POST /v1/invoices/nrs ingests an NRS-schema invoice; the
//     Idempotency-Key header makes resubmission a replay, never a duplicate.
//   - A payload carrying only an existing IRN returns that invoice's current
//     record (idempotent status lookup); an unknown IRN falls through to
//     schema validation and fails 422 -> surfaced as "not found".
//   - Auth is the shared service-token pattern: X-Service-Token (env-injected
//     MERIDIAN_SERVICE_TOKEN / EINVOICING_SERVICE_TOKEN), validated
//     fail-closed server-side; X-Dev-Role is sent only as the dev fallback.
//
// Fail-closed: PROFILE=prod without EINVOICING_BASE_URL refuses boot (see
// checkEInvoicingConfig, wired in main.go). Upstream errors surface honest
// messages — never a fabricated success.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxEInvoiceAmountKobo bounds USSD-issued invoices (N10,000,000). Anything
// larger is an absurd entry for this channel and is rejected locally.
const maxEInvoiceAmountKobo = 1_000_000_000

// einvConfigFromEnv resolves the einvoicing upstream. Prod profile requires
// EINVOICING_BASE_URL (boot refusal otherwise); dev may run without it (the
// actions then report the service as unavailable instead of faking success).
func einvConfigFromEnv() (baseURL string, err error) {
	baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("EINVOICING_BASE_URL")), "/")
	if baseURL == "" && isProdProfile() {
		return "", fmt.Errorf("EINVOICING_BASE_URL is required in prod profile (fail closed)")
	}
	return baseURL, nil
}

// einvServiceToken returns the env-injected service token ("" in dev).
func einvServiceToken() string {
	if tok := os.Getenv("EINVOICING_SERVICE_TOKEN"); tok != "" {
		return tok
	}
	return os.Getenv("MERIDIAN_SERVICE_TOKEN")
}

// einvDo performs one authenticated JSON call against the einvoicing service.
func einvDo(baseURL, path, idemKey string, payload any, out any) (int, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Name", serviceName)
	if tok := einvServiceToken(); tok != "" {
		req.Header.Set("X-Service-Token", tok)
	} else {
		req.Header.Set("X-Dev-Role", "operator") // dev fallback only
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, fmt.Errorf("einvoicing %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// einvIssueRequest is the minimal NRS-schema invoice the USSD flow submits.
type einvIssueRequest struct {
	BusinessID           string `json:"business_id"`
	IssueDate            string `json:"issue_date"`
	InvoiceTypeCode      string `json:"invoice_type_code"`
	DocumentCurrencyCode string `json:"document_currency_code"`
	InvoiceKind          string `json:"invoice_kind"`
	BuyerReference       string `json:"buyer_reference,omitempty"`
	Supplier             einvParty
	Customer             einvParty
	Lines                []einvLine
	MonetaryTotal        einvMonetaryTotal
}

type einvParty struct {
	TIN  string `json:"tin"`
	Name string `json:"name,omitempty"`
}

type einvLine struct {
	InvoicedQuantity    float64 `json:"invoiced_quantity"`
	LineExtensionAmount float64 `json:"line_extension_amount"`
	Item                struct {
		Name string `json:"name"`
	} `json:"item"`
	Price struct {
		PriceAmount float64 `json:"price_amount"`
	} `json:"price"`
}

type einvMonetaryTotal struct {
	LineExtensionAmount float64 `json:"line_extension_amount"`
	TaxExclusiveAmount  float64 `json:"tax_exclusive_amount"`
	TaxInclusiveAmount  float64 `json:"tax_inclusive_amount"`
	PayableAmount       float64 `json:"payable_amount"`
}

// MarshalJSON shapes the request into the NRS field names.
func (r einvIssueRequest) MarshalJSON() ([]byte, error) {
	naira := func(kobo uint64) float64 { return float64(kobo) / 100.0 }
	amount, _ := strconv.ParseUint(r.BuyerReference, 10, 64) // unused; kept for clarity
	_ = amount
	type line struct {
		InvoicedQuantity    float64 `json:"invoiced_quantity"`
		LineExtensionAmount float64 `json:"line_extension_amount"`
		Item                struct {
			Name string `json:"name"`
		} `json:"item"`
		Price struct {
			PriceAmount float64 `json:"price_amount"`
		} `json:"price"`
	}
	lines := make([]line, len(r.Lines))
	for i, l := range r.Lines {
		lines[i].InvoicedQuantity = l.InvoicedQuantity
		lines[i].LineExtensionAmount = l.LineExtensionAmount
		lines[i].Item.Name = l.Item.Name
		lines[i].Price.PriceAmount = l.Price.PriceAmount
	}
	return json.Marshal(map[string]any{
		"business_id":            r.BusinessID,
		"issue_date":             r.IssueDate,
		"invoice_type_code":      r.InvoiceTypeCode,
		"document_currency_code": r.DocumentCurrencyCode,
		"invoice_kind":           r.InvoiceKind,
		"accounting_supplier_party": map[string]any{
			"tin": r.Supplier.TIN, "name": r.Supplier.Name,
		},
		"accounting_customer_party": map[string]any{
			"tin": r.Customer.TIN,
		},
		"invoice_line": lines,
		"legal_monetary_total": map[string]any{
			"line_extension_amount": naira(uint64(r.MonetaryTotal.LineExtensionAmount)),
			"tax_exclusive_amount":  naira(uint64(r.MonetaryTotal.TaxExclusiveAmount)),
			"tax_inclusive_amount":  naira(uint64(r.MonetaryTotal.TaxInclusiveAmount)),
			"payable_amount":        naira(uint64(r.MonetaryTotal.PayableAmount)),
		},
	})
}

// einvIssueResponse is the subset of the NRS lifecycle response we surface.
type einvIssueResponse struct {
	IRN           string `json:"irn"`
	Status        string `json:"status"`
	PaymentStatus string `json:"payment_status"`
	InvoiceID     string `json:"invoice_id"`
	Error         string `json:"error"`
}

// einvIdempotencyKey returns the durable issuance key for this session's
// issue step. It is minted once into session data so a redial/resume (which
// re-keys the session to a new sessionId) still replays the SAME key
// upstream — the einvoicing store then replays instead of double-issuing.
func einvIdempotencyKey(sess *Session) string {
	if k := sess.Data["einv_idem_key"]; k != "" {
		return k
	}
	k := "ussd:" + sess.ID + ":einv-issue"
	sess.Data["einv_idem_key"] = k
	return k
}

// einvDigitsOnly reports whether s is non-empty ASCII digits.
func einvDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseAmountKobo validates the collected amount: positive whole kobo within
// the channel bound. Negatives/decimals never reach here (menu regex), but
// the action re-validates defensively.
func parseAmountKobo(s string) (uint64, error) {
	if !einvDigitsOnly(s) {
		return 0, fmt.Errorf("amount must be whole kobo (digits only)")
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount is too large")
	}
	if v == 0 {
		return 0, fmt.Errorf("amount must be greater than zero")
	}
	if v > maxEInvoiceAmountKobo {
		return 0, fmt.Errorf("amount exceeds the USSD limit of N%s", koboToNaira(maxEInvoiceAmountKobo))
	}
	return v, nil
}

// registerEInvActions wires the I3 e-invoice action handlers.
func registerEInvActions(actions map[string]ActionHandler, bus eventPublisher) {
	baseURL, _ := einvConfigFromEnv() // prod gate enforced at boot (main.go)

	unavailable := func() error {
		return fmt.Errorf("e-invoice service is temporarily unavailable; please try again later")
	}

	// einv.issue: collect amount + buyer TIN (already in session data) and
	// issue through the einvoicing service with a durable idempotency key.
	actions["einv.issue"] = func(sess *Session) error {
		amountKobo, err := parseAmountKobo(sess.Data["amount_kobo"])
		if err != nil {
			return err
		}
		buyerTIN := sess.Data["buyer_tin"]
		if buyerTIN == "" {
			return fmt.Errorf("missing buyer TIN in session")
		}
		sess.Data["amount_naira"] = koboToNaira(amountKobo)
		// Session-level replay guard: a resumed/redialed session that already
		// issued replays the recorded result without another upstream call.
		if irn := sess.Data["einv_issued_irn"]; irn != "" &&
			sess.Data["einv_issued_amount"] == sess.Data["amount_kobo"] &&
			sess.Data["einv_issued_buyer"] == buyerTIN {
			sess.Data["irn"] = irn
			sess.Data["invoice_status"] = sess.Data["einv_issued_status"]
			return nil
		}
		if baseURL == "" {
			return unavailable()
		}
		supplierTIN := sess.Data["tin"]
		if supplierTIN == "" {
			supplierTIN = "USSD-" + sess.Phone // dev standalone identity
		}
		amountNaira := float64(amountKobo) / 100.0
		req := map[string]any{
			"business_id":            supplierTIN,
			"issue_date":             time.Now().UTC().Format("2006-01-02"),
			"invoice_type_code":      "381",
			"document_currency_code": "NGN",
			"invoice_kind":           "B2B",
			"buyer_reference":        buyerTIN,
			"accounting_supplier_party": map[string]any{
				"tin": supplierTIN, "name": sess.Data["name"],
			},
			"accounting_customer_party": map[string]any{"tin": buyerTIN},
			"invoice_line": []map[string]any{{
				"invoiced_quantity":     1,
				"line_extension_amount": amountNaira,
				"item":                  map[string]any{"name": "USSD sale"},
				"price":                 map[string]any{"price_amount": amountNaira},
			}},
			"legal_monetary_total": map[string]any{
				"line_extension_amount": amountNaira,
				"tax_exclusive_amount":  amountNaira,
				"tax_inclusive_amount":  amountNaira,
				"payable_amount":        amountNaira,
			},
		}
		idem := einvIdempotencyKey(sess)
		var out einvIssueResponse
		status, err := einvDo(baseURL, "/v1/invoices/nrs", idem, req, &out)
		if err != nil {
			bus.Publish("nrs.einv.ussd.v1", map[string]any{
				"flow": "issue", "phone": sess.Phone, "outcome": "upstream_error", "status": status,
			})
			return unavailable()
		}
		if out.IRN == "" {
			bus.Publish("nrs.einv.ussd.v1", map[string]any{
				"flow": "issue", "phone": sess.Phone, "outcome": "rejected", "error": out.Error,
			})
			if out.Error != "" {
				return fmt.Errorf("invoice rejected: %s", out.Error)
			}
			return fmt.Errorf("invoice was not issued (no IRN returned)")
		}
		sess.Data["irn"] = out.IRN
		sess.Data["invoice_status"] = out.Status
		// Record the issuance in-session for the replay guard above.
		sess.Data["einv_issued_irn"] = out.IRN
		sess.Data["einv_issued_status"] = out.Status
		sess.Data["einv_issued_amount"] = sess.Data["amount_kobo"]
		sess.Data["einv_issued_buyer"] = buyerTIN
		bus.Publish("nrs.einv.ussd.v1", map[string]any{
			"flow": "issue", "phone": sess.Phone, "irn": out.IRN, "status": out.Status, "amount_kobo": amountKobo, "outcome": "issued",
		})
		return nil
	}

	// einv.status: query invoice status by IRN. Uses the NRS contract's
	// idempotent resubmission: POSTing a payload that carries only the IRN
	// returns the existing record; an unknown IRN fails validation (422),
	// surfaced honestly as "not found".
	actions["einv.status"] = func(sess *Session) error {
		irn := sess.Data["irn"]
		if irn == "" {
			return fmt.Errorf("missing IRN in session")
		}
		if baseURL == "" {
			return unavailable()
		}
		var out einvIssueResponse
		status, err := einvDo(baseURL, "/v1/invoices/nrs", "", map[string]any{"irn": irn}, &out)
		if err != nil {
			if status == 422 || status == 400 || status == 404 {
				return fmt.Errorf("no invoice found for IRN %s", irn)
			}
			return unavailable()
		}
		if out.Status == "" {
			return fmt.Errorf("no invoice found for IRN %s", irn)
		}
		sess.Data["invoice_status"] = out.Status
		if out.PaymentStatus != "" {
			sess.Data["payment_status"] = out.PaymentStatus
		} else {
			sess.Data["payment_status"] = "-"
		}
		bus.Publish("nrs.einv.ussd.v1", map[string]any{
			"flow": "status", "phone": sess.Phone, "irn": irn, "status": out.Status, "outcome": "found",
		})
		return nil
	}
}
