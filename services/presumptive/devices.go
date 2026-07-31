package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// devices.go — server-side enrolment of agent device signing keys and
// verification of offline cash receipts (audit HIGH #6: offline receipts
// were self-signed by a localStorage key the server never knew, so they
// were unverifiable).
//
// Enrolment flow (documented in services/presumptive/README.md):
//  1. The agent PWA generates a device key once (per device) and calls
//     POST /v1/devices/enroll while authenticated; the server binds the key
//     to (agent_id, device_id). In profile=prod the agent_id must match the
//     authenticated principal (JWT sub); in AUTH_MODE=dev the X-Dev-Agent-Id
//     header is honoured (and only there).
//  2. Offline receipts are signed as SHA-256(device_key | canonical_payload)
//     exactly as the PWA does today.
//  3. POST /v1/receipts/verify recomputes the signature against the enrolled
//     key; unknown devices fail closed.

// DeviceKey is an enrolled agent device signing key.
type DeviceKey struct {
	AgentID  string `json:"agent_id"`
	DeviceID string `json:"device_id"`
	// Key is persisted in the embedded store but never returned by any HTTP
	// handler (handlers project only agent_id/device_id/status).
	Key       string `json:"key"`
	Status    string `json:"status"` // active|revoked
	CreatedAt string `json:"created_at"`
}

// DeviceService manages device key enrolment + receipt verification.
type DeviceService struct {
	st *store.Store
}

func NewDeviceService(st *store.Store) *DeviceService { return &DeviceService{st: st} }

func deviceStoreID(agentID, deviceID string) string { return agentID + "/" + deviceID }

// Enroll registers (or re-registers) a device key for an agent.
func (d *DeviceService) Enroll(agentID, deviceID, key string) (DeviceKey, error) {
	if agentID == "" || deviceID == "" || len(key) < 16 {
		return DeviceKey{}, fmt.Errorf("agent_id, device_id and a key of >= 16 chars are required")
	}
	dk := DeviceKey{AgentID: agentID, DeviceID: deviceID, Key: key, Status: "active", CreatedAt: nowRFC3339()}
	if err := d.st.Put("devices", deviceStoreID(agentID, deviceID), dk); err != nil {
		return DeviceKey{}, err
	}
	return dk, nil
}

// ReceiptVerifyRequest is one offline receipt presented for verification.
type ReceiptVerifyRequest struct {
	Serial     string `json:"serial"`
	AgentID    string `json:"agent_id"`
	DeviceID   string `json:"device_id"`
	PayerName  string `json:"payer_name"`
	AmountKobo uint64 `json:"amount_kobo"`
	Purpose    string `json:"purpose"`
	IssuedAt   string `json:"issued_at"`
	Signature  string `json:"signature"`
}

// CanonicalReceiptPayload must match the PWA signing order (Receipts.tsx):
// serial|payer|amountKobo|purpose|issuedAt.
func CanonicalReceiptPayload(r ReceiptVerifyRequest) string {
	return strings.Join([]string{r.Serial, r.PayerName, fmt.Sprint(r.AmountKobo), r.Purpose, r.IssuedAt}, "|")
}

func signReceipt(key, payload string) string {
	sum := sha256.Sum256([]byte(key + "|" + payload))
	return hex.EncodeToString(sum[:])
}

// VerifyReceipt recomputes the signature against the enrolled device key.
// Fails closed: unknown/revoked device => invalid.
func (d *DeviceService) VerifyReceipt(r ReceiptVerifyRequest) (bool, string, error) {
	var dk DeviceKey
	ok, err := d.st.Get("devices", deviceStoreID(r.AgentID, r.DeviceID), &dk)
	if err != nil {
		return false, "store error", err
	}
	if !ok {
		return false, "device not enrolled", nil
	}
	if dk.Status != "active" {
		return false, "device " + dk.Status, nil
	}
	want := signReceipt(dk.Key, CanonicalReceiptPayload(r))
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(r.Signature)), []byte(want)) != 1 {
		return false, "signature mismatch", nil
	}
	return true, "verified against enrolled device key", nil
}

// --- HTTP handlers ---

type enrollRequest struct {
	AgentID  string `json:"agent_id"`
	DeviceID string `json:"device_id"`
	Key      string `json:"key"`
}

func (s *server) enrollDevice(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Identity binding: in prod the agent_id must be the authenticated
	// principal; in dev the X-Dev-Agent-Id header stands in for it.
	if ident := httpx.RequestIdentity(r); ident != "" {
		if req.AgentID == "" {
			req.AgentID = ident
		}
		if req.AgentID != ident {
			httpx.WriteProblem(w, http.StatusForbidden, "forbidden", "agent_id does not match the authenticated identity")
			return
		}
	}
	dk, err := s.devices.Enroll(req.AgentID, req.DeviceID, req.Key)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"agent_id": dk.AgentID, "device_id": dk.DeviceID, "status": dk.Status,
	})
}

func (s *server) verifyReceipt(w http.ResponseWriter, r *http.Request) {
	var req ReceiptVerifyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	valid, detail, err := s.devices.VerifyReceipt(req)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	status := http.StatusOK
	if !valid {
		status = http.StatusUnprocessableEntity
	}
	httpx.WriteJSON(w, status, map[string]any{"serial": req.Serial, "valid": valid, "detail": detail})
}
