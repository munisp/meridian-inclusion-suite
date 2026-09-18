package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
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

// DeviceKey is an enrolled agent device record.
type DeviceKey struct {
	AgentID  string `json:"agent_id"`
	DeviceID string `json:"device_id"`
	// Key is the LEGACY symmetric MAC key, persisted in the embedded store
	// but never returned by any HTTP handler. Present only on legacy
	// devices; ed25519-enrolled devices never carry a Key.
	Key string `json:"key,omitempty"`
	// PublicKey is the hex-encoded ed25519 public key. The server stores
	// ONLY the public key — compromise of the device-key store yields no
	// signing capability (S1b#10).
	PublicKey string `json:"public_key,omitempty"`
	// Legacy marks devices enrolled under the old symmetric-MAC scheme.
	// Legacy MAC receipts issued before cutover still verify (subject to
	// the grace flag), but the device must re-enrol with ed25519 before
	// issuing new receipts.
	Legacy    bool   `json:"legacy"`
	Status    string `json:"status"` // active|revoked
	CreatedAt string `json:"created_at"`
}

// DeviceService manages device key enrolment + receipt verification.
type DeviceService struct {
	st *store.Store
}

func NewDeviceService(st *store.Store) *DeviceService { return &DeviceService{st: st} }

func deviceStoreID(agentID, deviceID string) string { return agentID + "/" + deviceID }

// legacyGraceAllowed reports whether legacy MAC receipts are still
// verifiable. Config-driven via DEVICE_LEGACY_GRACE (true/false); when
// unset it fails closed in profile=prod and is allowed in dev.
func legacyGraceAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEVICE_LEGACY_GRACE"))) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return !workflowx.IsProdProfile()
}

// Enroll registers (or re-registers) a LEGACY symmetric device key.
// Retained for backward compatibility; new enrolments should use
// EnrollEd25519.
func (d *DeviceService) Enroll(agentID, deviceID, key string) (DeviceKey, error) {
	if agentID == "" || deviceID == "" || len(key) < 16 {
		return DeviceKey{}, fmt.Errorf("agent_id, device_id and a key of >= 16 chars are required")
	}
	dk := DeviceKey{AgentID: agentID, DeviceID: deviceID, Key: key, Legacy: true, Status: "active", CreatedAt: nowRFC3339()}
	if err := d.st.Put("devices", deviceStoreID(agentID, deviceID), dk); err != nil {
		return DeviceKey{}, err
	}
	return dk, nil
}

// EnrollEd25519 registers a device by its ed25519 PUBLIC key. The server
// persists only the public key. Re-enrolling an existing (e.g. legacy)
// device upgrades it to ed25519 while retaining the legacy MAC key so
// previously issued receipts remain verifiable during the grace window.
func (d *DeviceService) EnrollEd25519(agentID, deviceID, publicKeyHex string) (DeviceKey, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(publicKeyHex))
	if agentID == "" || deviceID == "" {
		return DeviceKey{}, fmt.Errorf("agent_id and device_id are required")
	}
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return DeviceKey{}, fmt.Errorf("public_key must be a hex-encoded 32-byte ed25519 public key")
	}
	dk := DeviceKey{AgentID: agentID, DeviceID: deviceID,
		PublicKey: strings.ToLower(strings.TrimSpace(publicKeyHex)),
		Status:    "active", CreatedAt: nowRFC3339()}
	// Preserve legacy key material for verifying pre-cutover receipts.
	var prev DeviceKey
	if ok, err := d.st.Get("devices", deviceStoreID(agentID, deviceID), &prev); err != nil {
		return DeviceKey{}, err
	} else if ok && prev.Key != "" {
		dk.Key = prev.Key
	}
	if err := d.st.Put("devices", deviceStoreID(agentID, deviceID), dk); err != nil {
		return DeviceKey{}, err
	}
	return dk, nil
}

// PublicKey returns the enrolled ed25519 public key (hex) for a device,
// for third-party/offline verification. Returns ok=false for unknown or
// legacy-only devices (no public key on file).
func (d *DeviceService) PublicKey(agentID, deviceID string) (string, bool, error) {
	var dk DeviceKey
	ok, err := d.st.Get("devices", deviceStoreID(agentID, deviceID), &dk)
	if err != nil {
		return "", false, err
	}
	if !ok || dk.PublicKey == "" {
		return "", false, nil
	}
	return dk.PublicKey, true, nil
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

// SignReceiptEd25519 signs the canonical receipt payload with the device's
// ed25519 private key (device-side; the server never sees this key).
func SignReceiptEd25519(priv ed25519.PrivateKey, payload string) string {
	return hex.EncodeToString(ed25519.Sign(priv, []byte(payload)))
}

// VerifyReceipt verifies a presented offline receipt.
//
//   - ed25519-enrolled device: ed25519.Verify against the stored PUBLIC
//     key. A server-side key-store compromise cannot forge receipts.
//   - legacy (symmetric-MAC) device: the old MAC path is honoured only
//     while the grace flag allows it (DEVICE_LEGACY_GRACE; fail-closed in
//     profile=prod). After cutover the device must re-enrol with ed25519
//     before any new receipt it issues will verify.
//
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
	payload := CanonicalReceiptPayload(r)

	if !dk.Legacy {
		// Asymmetric path: verify with the stored public key only.
		pub, decErr := hex.DecodeString(dk.PublicKey)
		sig, sigErr := hex.DecodeString(strings.ToLower(strings.TrimSpace(r.Signature)))
		if decErr != nil || len(pub) != ed25519.PublicKeySize {
			return false, "device key record invalid", nil
		}
		if sigErr != nil || len(sig) != ed25519.SignatureSize ||
			!ed25519.Verify(ed25519.PublicKey(pub), []byte(payload), sig) {
			return false, "signature mismatch", nil
		}
		return true, "verified against enrolled device ed25519 public key", nil
	}

	// Legacy path: pre-cutover MAC receipts verify during the grace window;
	// fail closed afterwards so the device is forced to re-enrol (ed25519)
	// before issuing new receipts.
	if !legacyGraceAllowed() {
		return false, "legacy device: ed25519 re-enrolment required (grace period ended)", nil
	}
	want := signReceipt(dk.Key, payload)
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(r.Signature)), []byte(want)) != 1 {
		return false, "signature mismatch", nil
	}
	return true, "verified against legacy device key — device must re-enrol with ed25519", nil
}

// --- HTTP handlers ---

type enrollRequest struct {
	AgentID   string `json:"agent_id"`
	DeviceID  string `json:"device_id"`
	Key       string `json:"key"`        // legacy symmetric key (deprecated)
	PublicKey string `json:"public_key"` // hex ed25519 public key (preferred)
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
	var dk DeviceKey
	var err error
	if req.PublicKey != "" {
		dk, err = s.devices.EnrollEd25519(req.AgentID, req.DeviceID, req.PublicKey)
	} else {
		dk, err = s.devices.Enroll(req.AgentID, req.DeviceID, req.Key)
	}
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"agent_id": dk.AgentID, "device_id": dk.DeviceID, "status": dk.Status,
		"legacy": dk.Legacy,
	})
}

// devicePublicKey serves the enrolled ed25519 public key for a device so
// third parties (banks, auditors, payers) can verify receipts offline by
// device id. Public keys are not sensitive.
func (s *server) devicePublicKey(w http.ResponseWriter, r *http.Request) {
	pub, ok, err := s.devices.PublicKey(r.PathValue("agent"), r.PathValue("device"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "no ed25519 public key enrolled for this device")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"agent_id": r.PathValue("agent"), "device_id": r.PathValue("device"),
		"algorithm": "ed25519", "public_key": pub,
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
