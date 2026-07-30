// Package main implements services/onboarding (SPEC §4, T5):
// informal-sector operator onboarding — NIMC verification (adapter+simulator),
// TIN provisioning via core tin-graph (with local fallback), NDPA consent
// capture, offline-first capture ingest with idempotency + conflict
// resolution, operator registry CRUD and the wf-onb-* workflow functions.
package main

import "time"

const serviceName = "onboarding"
const serviceVersion = "1.0.0"

// Operator is an informal-sector operator in the registry. The origin plane
// holds the TIN; every event/analytics payload carries only nin_hash/tin_hash
// per §1.3 pseudonymisation.
type Operator struct {
	ID            string `json:"id"`
	NINHash       string `json:"nin_hash"`
	TINHash       string `json:"tin_hash,omitempty"`
	TIN           string `json:"tin,omitempty"`
	FullName      string `json:"full_name"`
	Phone         string `json:"phone"`
	State         string `json:"state"`
	LGA           string `json:"lga"`
	TradeCategory string `json:"trade_category"`
	Status        string `json:"status"` // registered|nin_verified|tin_provisioned|graduated
	AgentID       string `json:"agent_id"`
	ConsentID     string `json:"consent_id,omitempty"`
	Serial        uint64 `json:"serial"` // §1.5 low-64 entity serial
	CapturedAt    string `json:"captured_at"`
	SyncedAt      string `json:"synced_at"`
	ClientRef     string `json:"client_ref,omitempty"` // agent-side idempotency ref
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ConsentRecord is the local NDPA consent fallback record.
type ConsentRecord struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"` // operator id
	Purpose   string `json:"purpose"` // e.g. "tax_onboarding"
	Channel   string `json:"channel"` // agent_pwa|ussd|paper_digitised
	Granted   bool   `json:"granted"`
	Revoked   bool   `json:"revoked"`
	Receipt   string `json:"receipt"` // NDPA receipt id
	CreatedAt string `json:"created_at"`
	Source    string `json:"source"` // consent_svc|local_fallback
}

// CaptureItem is one offline-captured operator record inside a batch.
type CaptureItem struct {
	ClientRef     string `json:"client_ref"` // agent-side UUID (idempotency element)
	NIN           string `json:"nin"`
	FullName      string `json:"full_name"`
	Phone         string `json:"phone"`
	State         string `json:"state"`
	LGA           string `json:"lga"`
	TradeCategory string `json:"trade_category"`
	ConsentID     string `json:"consent_id,omitempty"`
	CapturedAt    string `json:"captured_at"` // RFC3339 when captured on device
}

// CaptureBatch is the offline batch ingest unit (≥72h offline tolerance).
type CaptureBatch struct {
	ID             string              `json:"id"`
	IdempotencyKey string              `json:"idempotency_key"`
	AgentID        string              `json:"agent_id"`
	Items          []CaptureItem       `json:"items"`
	Results        []CaptureItemResult `json:"results"`
	Status         string              `json:"status"` // processed|duplicate
	CreatedAt      string              `json:"created_at"`
}

// CaptureItemResult reports per-item disposition incl. conflict resolution.
type CaptureItemResult struct {
	ClientRef       string `json:"client_ref"`
	OperatorID      string `json:"operator_id,omitempty"`
	Outcome         string `json:"outcome"` // created|duplicate_client_ref|conflict_resolved|rejected
	Detail          string `json:"detail,omitempty"`
	OfflineAgeHours int    `json:"offline_age_hours"`
}

// WorkflowRun records one wf-onb-* execution (dev in-process runner).
type WorkflowRun struct {
	ID        string   `json:"id"`
	Workflow  string   `json:"workflow"`
	Input     any      `json:"input,omitempty"`
	Steps     []string `json:"steps"`
	Status    string   `json:"status"` // completed|failed
	Error     string   `json:"error,omitempty"`
	Result    any      `json:"result,omitempty"`
	StartedAt string   `json:"started_at"`
	EndedAt   string   `json:"ended_at"`
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
