package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

// PSSP registry (audit O6): PSSPs are first-class onboarded records with
// PER-PSSP webhook secrets (replacing the single shared secret), callback
// URLs, key references, sandbox->active promotion and rotation. Simulator
// adapters remain, but each registered PSSP gets its own keyed sim instance.
type psspRecord struct {
	ID              string `json:"id"`
	Name            string `json:"name"` // provider key, e.g. "flutterwave"
	CallbackURL     string `json:"callback_url"`
	WebhookSecret   string `json:"webhook_secret"` // stored, never exposed via API
	APIKeyRef       string `json:"api_key_ref"`    // reference into the secret manager, not the key itself
	FeeBps          uint64 `json:"fee_bps"`
	Status          string `json:"status"` // sandbox|active|suspended
	Sim             bool   `json:"sim"`    // simulator-backed adapter
	CreatedAt       string `json:"created_at"`
	RotatedAt       string `json:"rotated_at,omitempty"`
	SecretVersion   int    `json:"secret_version"`
	OnboardingNotes string `json:"onboarding_notes,omitempty"`
}

// PSSPView is the API representation (webhook secret redacted).
type PSSPView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CallbackURL   string `json:"callback_url"`
	SecretPreview string `json:"webhook_secret_preview"`
	APIKeyRef     string `json:"api_key_ref"`
	FeeBps        uint64 `json:"fee_bps"`
	Status        string `json:"status"`
	Sim           bool   `json:"sim"`
	CreatedAt     string `json:"created_at"`
	RotatedAt     string `json:"rotated_at,omitempty"`
	SecretVersion int    `json:"secret_version"`
}

func (p psspRecord) view() PSSPView {
	preview := ""
	if len(p.WebhookSecret) >= 8 {
		preview = p.WebhookSecret[:4] + "…" + p.WebhookSecret[len(p.WebhookSecret)-4:]
	}
	return PSSPView{
		ID: p.ID, Name: p.Name, CallbackURL: p.CallbackURL, SecretPreview: preview,
		APIKeyRef: p.APIKeyRef, FeeBps: p.FeeBps, Status: p.Status, Sim: p.Sim,
		CreatedAt: p.CreatedAt, RotatedAt: p.RotatedAt, SecretVersion: p.SecretVersion,
	}
}

// PSSP status transitions (sandbox -> active promotion; suspension).
var psspStatusTransitions = map[string][]string{
	"sandbox":   {"active", "suspended"},
	"active":    {"suspended", "sandbox"},
	"suspended": {"sandbox"},
}

// PSSPRegistry is the PSSP store + adapter wiring.
type PSSPRegistry struct {
	st  *store.Store
	hub *PSSPHub
}

func newWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "whsec_" + hex.EncodeToString(b)
}

// NewPSSPRegistry loads the registry and seeds the three simulator providers
// (remita/etranzact/flutterwave) as sandbox PSSPs with their OWN webhook
// secrets when the store is empty.
func NewPSSPRegistry(st *store.Store, hub *PSSPHub) *PSSPRegistry {
	r := &PSSPRegistry{st: st, hub: hub}
	seed := []struct {
		name   string
		refFmt string
		feeBps uint64
	}{
		{"remita", "RRR-%s", 100},
		{"etranzact", "ETZ-%s", 75},
		{"flutterwave", "FLW-%s", 140},
	}
	for _, s := range seed {
		if _, ok, _ := r.byName(s.name); ok {
			continue
		}
		rec := psspRecord{
			ID: ids.WithPrefix("pssp"), Name: s.name, FeeBps: s.feeBps,
			Status: "sandbox", Sim: true, WebhookSecret: newWebhookSecret(),
			APIKeyRef: "sim://" + s.name, CreatedAt: time.Now().UTC().Format(time.RFC3339),
			SecretVersion: 1, OnboardingNotes: "seeded simulator PSSP",
		}
		if err := r.st.Put("pssps", rec.ID, rec); err != nil {
			log.Printf("pssp registry seed %s: %v", s.name, err)
		}
	}
	return r
}

// OnboardRequest registers a new PSSP.
type OnboardRequest struct {
	Name        string `json:"name"`
	CallbackURL string `json:"callback_url"`
	APIKeyRef   string `json:"api_key_ref"`
	FeeBps      uint64 `json:"fee_bps"`
	Notes       string `json:"notes,omitempty"`
}

// OnboardResult is returned once at onboarding (the ONLY time the full
// webhook secret is disclosed to the caller).
type OnboardResult struct {
	PSSPView
	WebhookSecret string `json:"webhook_secret"`
}

// Onboard registers a PSSP (status sandbox) with a server-generated per-PSSP
// webhook secret and a per-PSSP keyed simulator adapter.
func (r *PSSPRegistry) Onboard(in OnboardRequest) (OnboardResult, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if name == "" || in.CallbackURL == "" {
		return OnboardResult{}, fmt.Errorf("name and callback_url are required")
	}
	if existing, ok, _ := r.byName(name); ok {
		return OnboardResult{}, fmt.Errorf("PSSP %q already registered as %s", name, existing.ID)
	}
	if in.FeeBps == 0 {
		in.FeeBps = 150
	}
	if in.APIKeyRef == "" {
		in.APIKeyRef = "kms://pssp/" + name
	}
	rec := psspRecord{
		ID: ids.WithPrefix("pssp"), Name: name, CallbackURL: in.CallbackURL,
		APIKeyRef: in.APIKeyRef, FeeBps: in.FeeBps, Status: "sandbox", Sim: true,
		WebhookSecret: newWebhookSecret(), SecretVersion: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), OnboardingNotes: in.Notes,
	}
	if err := r.st.Put("pssps", rec.ID, rec); err != nil {
		return OnboardResult{}, err
	}
	// per-PSSP keyed sim adapter (provider key = name)
	refFmt := strings.ToUpper(name)[:3] + "-%s"
	r.hub.adapters[name] = newPSSPSim(name, refFmt, in.FeeBps)
	return OnboardResult{PSSPView: rec.view(), WebhookSecret: rec.WebhookSecret}, nil
}

func (r *PSSPRegistry) byName(name string) (psspRecord, bool, error) {
	var all []psspRecord
	if err := r.st.List("pssps", &all); err != nil {
		return psspRecord{}, false, err
	}
	for _, p := range all {
		if p.Name == strings.ToLower(name) {
			return p, true, nil
		}
	}
	return psspRecord{}, false, nil
}

// List returns all PSSPs (secrets redacted).
func (r *PSSPRegistry) List() ([]PSSPView, error) {
	var all []psspRecord
	if err := r.st.List("pssps", &all); err != nil {
		return nil, err
	}
	out := []PSSPView{}
	for _, p := range all {
		out = append(out, p.view())
	}
	return out, nil
}

// Get returns one PSSP by id (redacted).
func (r *PSSPRegistry) Get(id string) (PSSPView, bool, error) {
	var rec psspRecord
	ok, err := r.st.Get("pssps", id, &rec)
	return rec.view(), ok, err
}

// RotateSecret issues a new webhook secret (old secret invalid immediately);
// the full new secret is returned once.
func (r *PSSPRegistry) RotateSecret(id string) (OnboardResult, error) {
	var rec psspRecord
	ok, err := r.st.Get("pssps", id, &rec)
	if err != nil || !ok {
		return OnboardResult{}, fmt.Errorf("PSSP %s not found", id)
	}
	rec.WebhookSecret = newWebhookSecret()
	rec.SecretVersion++
	rec.RotatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := r.st.Put("pssps", rec.ID, rec); err != nil {
		return OnboardResult{}, err
	}
	return OnboardResult{PSSPView: rec.view(), WebhookSecret: rec.WebhookSecret}, nil
}

// SetStatus moves a PSSP through sandbox->active->suspended.
func (r *PSSPRegistry) SetStatus(id, to string) (PSSPView, error) {
	var rec psspRecord
	ok, err := r.st.Get("pssps", id, &rec)
	if err != nil || !ok {
		return PSSPView{}, fmt.Errorf("PSSP %s not found", id)
	}
	allowed := false
	for _, t := range psspStatusTransitions[rec.Status] {
		if t == to {
			allowed = true
		}
	}
	if !allowed {
		return PSSPView{}, fmt.Errorf("illegal PSSP status transition %s -> %s", rec.Status, to)
	}
	rec.Status = to
	return rec.view(), r.st.Put("pssps", rec.ID, rec)
}

// VerifyWebhook validates X-PSSP-Signature with the PER-PSSP secret (O6).
// Unknown providers fall back to the legacy shared secret in dev only;
// in profile=prod an unregistered provider is rejected.
func (r *PSSPRegistry) VerifyWebhook(provider, signature string, body []byte) bool {
	if signature == "" {
		return false
	}
	if rec, ok, _ := r.byName(provider); ok && rec.WebhookSecret != "" {
		want := hmacHex(rec.WebhookSecret, string(body))
		return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
	}
	if workflowx.IsProdProfile() {
		return false
	}
	return VerifyWebhookSignature(signature, body) // legacy shared secret, dev only
}
