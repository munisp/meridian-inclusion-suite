// Package main implements services/presumptive (SPEC §4, T12):
// presumptive-tax payments for the informal sector — payment intent ->
// pending transfer (ledger 200) -> PSSP authorise (adapter + simulators) ->
// capture/void; certificates with HMAC-signed payloads and a public
// rate-limited verification endpoint; agent float management (ledger 100,
// DEBITS_MUST_NOT_EXCEED_CREDITS); a band engine over the embedded
// rp-presumptive-* / rp-turnover-bands / rp-exemption-nta packs; wf-psm-*
// workflows; and reg-watch gate enforcement with a local gate-file fallback.
package main

import "time"

const serviceName = "presumptive"
const serviceVersion = "1.0.0"

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// Payment is a presumptive levy payment lifecycle record.
type Payment struct {
	ID                string `json:"id"`
	TINHash           string `json:"tin_hash"`
	State             string `json:"state"`
	TradeCategory     string `json:"trade_category"`
	TurnoverBand      string `json:"turnover_band"`
	AmountKobo        uint64 `json:"amount_kobo"`
	Currency          string `json:"currency"` // ISO 4217; locked to NGN at intent (G1)
	Period            string `json:"period"`   // e.g. "2026" (annual) or "2026-03"
	Provider          string `json:"provider"` // remita|etranzact|flutterwave|cash_agent
	Status            string `json:"status"`   // intent|pending_authorisation|authorised|captured_awaiting_post|captured|settled|disputed|charged_back|voided|failed|compensated
	PendingTransferID string `json:"pending_transfer_id,omitempty"`
	PostTransferID    string `json:"post_transfer_id,omitempty"`
	FeeKobo           uint64 `json:"fee_kobo,omitempty"` // PSSP fee leg (gross - settled)
	PSSPRef           string `json:"pssp_ref,omitempty"`
	CertificateSerial string `json:"certificate_serial,omitempty"`
	DisputeID         string `json:"dispute_id,omitempty"` // open/resolved dispute record (G11)
	RulePackVersion   string `json:"rule_pack_version"`
	FailReason        string `json:"fail_reason,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// Certificate is a presumptive payment certificate with an HMAC-signed payload.
type Certificate struct {
	Serial          string `json:"serial"`
	TINHash         string `json:"tin_hash"`
	State           string `json:"state"`
	Band            string `json:"band"`
	AmountKobo      uint64 `json:"amount_kobo"`
	Period          string `json:"period"`
	PaymentID       string `json:"payment_id"`
	IssuedAt        string `json:"issued_at"`
	RulePackVersion string `json:"rule_pack_version"`
	Signature       string `json:"signature"` // HMAC-SHA256 over canonical payload
}

// FloatAccount is the metadata mapping for a §1.5 ledger-100 agent float
// account (no PII on the ledger itself).
type FloatAccount struct {
	AgentID   string `json:"agent_id"`
	AccountID string `json:"account_id"`
	Serial    uint64 `json:"serial"`
	Currency  string `json:"currency"` // NGN
	CreatedAt string `json:"created_at"`
}

// FloatMovement records a topup/debit for the audit trail. ID is
// deterministic per (kind, reference) — the reference dedup key. Status
// tracks the pending->post saga so the recovery sweeper can finish or void
// movements interrupted by a crash.
type FloatMovement struct {
	ID                string `json:"id"`
	AgentID           string `json:"agent_id"`
	Kind              string `json:"kind"` // topup|debit
	AmountKobo        uint64 `json:"amount_kobo"`
	Reference         string `json:"reference"`
	PendingTransferID string `json:"pending_transfer_id,omitempty"`
	TransferID        string `json:"transfer_id"`
	Status            string `json:"status"` // pending|posted|voided
	FailReason        string `json:"fail_reason,omitempty"`
	CreatedAt         string `json:"created_at"`
}

// GateState mirrors the reg-watch gate model (fallback local gate file).
type GateState struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Open        bool   `json:"open"`
	UpdatedAt   string `json:"updated_at"`
	Source      string `json:"source"` // reg_watch|local_file
}

// Simulation is the persisted output of wf-psm-simulation.
type Simulation struct {
	ID        string            `json:"id"`
	Scenarios int               `json:"scenarios"`
	Results   []SimulationRow   `json:"results"`
	Totals    map[string]uint64 `json:"totals_kobo"`
	CreatedAt string            `json:"created_at"`
}

// SimulationRow is one band-engine evaluation inside a simulation.
type SimulationRow struct {
	OperatorRef    string `json:"operator_ref"`
	State          string `json:"state"`
	TradeCategory  string `json:"trade_category"`
	TurnoverKobo   uint64 `json:"turnover_kobo"`
	Band           string `json:"band"`
	AnnualLevyKobo uint64 `json:"annual_levy_kobo"`
	Exempt         bool   `json:"exempt"`
	PackID         string `json:"pack_id"`
}
