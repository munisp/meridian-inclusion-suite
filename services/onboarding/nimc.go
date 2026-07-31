package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/resilience"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

// NINVerification is the result of an NIMC identity verification.
type NINVerification struct {
	NINHash        string `json:"nin_hash"`
	Verified       bool   `json:"verified"`
	FirstName      string `json:"first_name,omitempty"`
	LastName       string `json:"last_name,omitempty"`
	Reference      string `json:"reference"`
	Source         string `json:"source"`          // nimc_api|simulator
	CredentialType string `json:"credential_type"` // vnin|nin_legacy
	Detail         string `json:"detail,omitempty"`
}

// NINVerifier is the NIMC adapter interface (external rail behind interface).
// VerifyVNIN is the PRIMARY rail: NIMC discontinued raw-NIN third-party
// verification in favour of 16-char vNIN tokens (72h TTL). VerifyNIN is the
// legacy raw-NIN path, retained for migration only.
type NINVerifier interface {
	VerifyNIN(nin string) (NINVerification, error)
	VerifyVNIN(vnin string, issuedAt time.Time) (NINVerification, error)
}

// VNINTTL is the NIMC vNIN token lifetime (72 hours from issuance).
const VNINTTL = 72 * time.Hour

var vninPattern = regexp.MustCompile(`^[A-Za-z]{2}\d{12}[A-Za-z]{2}$`)

// VNINValid checks the vNIN token shape: 2 letters + 12 digits + 2 letters.
func VNINValid(vnin string) bool { return vninPattern.MatchString(vnin) }

// VNINExpired reports whether the token is past its 72h TTL.
func VNINExpired(issuedAt time.Time) bool { return time.Since(issuedAt) > VNINTTL }

func hmacSHA256Hex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// NINHash pseudonymises a NIN per §1.3 (HMAC-SHA256, NIN_HMAC_KEY via keyx;
// fail-closed in profile=prod).
func NINHash(nin string) string {
	return hmacSHA256Hex(keyx.MustKey("NIN_HMAC_KEY", "meridian-dev-nin-key"), nin)
}

// TINHash pseudonymises a TIN per §1.3 (HMAC-SHA256, TIN_HMAC_KEY via keyx;
// fail-closed in profile=prod).
func TINHash(tin string) string {
	return hmacSHA256Hex(keyx.MustKey("TIN_HMAC_KEY", "meridian-dev-tin-key"), tin)
}

var ninPattern = regexp.MustCompile(`^\d{11}$`)

// NIMCSimulator is the deterministic offline NIMC simulator. Valid 11-digit
// NINs verify; NINs ending in "0000" simulate a mismatch outcome.
type NIMCSimulator struct{}

func (NIMCSimulator) VerifyNIN(nin string) (NINVerification, error) {
	v := NINVerification{NINHash: NINHash(nin), Source: "simulator", CredentialType: "nin_legacy"}
	if !ninPattern.MatchString(nin) {
		v.Verified = false
		v.Detail = "NIN must be exactly 11 digits"
		v.Reference = "NIMCSIM-REJ-" + v.NINHash[:12]
		return v, nil
	}
	// deterministic persona from the NIN (no real PII in dev)
	sum := sha256.Sum256([]byte("nimc-persona:" + nin))
	first := []string{"Adaeze", "Chinedu", "Aisha", "Tunde", "Ngozi", "Ibrahim", "Funke", "Musa"}[sum[0]%8]
	last := []string{"Okafor", "Bello", "Adeyemi", "Eze", "Danladi", "Balogun", "Nwosu", "Garba"}[sum[1]%8]
	if strings.HasSuffix(nin, "0000") {
		v.Verified = false
		v.Detail = "NIN not found in NIMC registry (simulated miss)"
	} else {
		v.Verified = true
		v.FirstName = first
		v.LastName = last
		v.Detail = "identity verified against simulated NIMC registry"
	}
	v.Reference = "NIMCSIM-" + v.NINHash[:16]
	return v, nil
}

// VerifyVNIN [SIM]: deterministic vNIN personas — "EX.." prefix or an
// out-of-TTL issuedAt simulates an expired token, "NF.." a registry miss.
func (NIMCSimulator) VerifyVNIN(vnin string, issuedAt time.Time) (NINVerification, error) {
	v := NINVerification{NINHash: NINHash(vnin), Source: "simulator", CredentialType: "vnin"}
	switch {
	case !VNINValid(vnin):
		v.Detail = "vNIN must be 16 chars: 2 letters + 12 digits + 2 letters"
	case strings.HasPrefix(strings.ToUpper(vnin), "EX") || VNINExpired(issuedAt):
		v.Detail = "vNIN token expired (72h TTL)"
	case strings.HasPrefix(strings.ToUpper(vnin), "NF"):
		v.Detail = "vNIN not found in NIMC registry (simulated miss)"
	default:
		v.Verified = true
		sum := sha256.Sum256([]byte("nimc-persona:vnin:" + vnin))
		v.FirstName = []string{"Adaeze", "Chinedu", "Aisha", "Tunde", "Ngozi", "Ibrahim", "Funke", "Musa"}[sum[0]%8]
		v.LastName = []string{"Okafor", "Bello", "Adeyemi", "Eze", "Danladi", "Balogun", "Nwosu", "Garba"}[sum[1]%8]
		v.Detail = "identity verified via simulated vNIN token"
	}
	v.Reference = "NIMCSIM-V-" + v.NINHash[:16]
	return v, nil
}

// NIMCHTTPAdapter is the real NIMC verification client (H4): POST
// {NIMC_API_URL}/verify {nin} with HMAC-SHA256 request signing
// (NIMC_API_KEY), 3-retry exponential backoff and a circuit breaker
// (5 failures → open 30s) via the shared resilience policy. It NEVER logs
// the raw NIN — only the pseudonymised nin_hash.
type NIMCHTTPAdapter struct {
	base    string
	apiKey  string
	hc      *http.Client
	breaker *resilience.Breaker
}

func NewNIMCHTTPAdapter(base, apiKey string) *NIMCHTTPAdapter {
	return &NIMCHTTPAdapter{
		base:    strings.TrimRight(base, "/"),
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: 10 * time.Second},
		breaker: &resilience.Breaker{Threshold: 5, Cooldown: 30 * time.Second},
	}
}

// verifyResponse is the NIMC-side response mapped onto NINVerification.
type verifyResponse struct {
	Verified  bool   `json:"verified"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Reference string `json:"reference"`
	Detail    string `json:"detail"`
}

func (a *NIMCHTTPAdapter) VerifyNIN(nin string) (NINVerification, error) {
	ninHash := NINHash(nin)
	body, _ := json.Marshal(map[string]string{"nin": nin})
	sig := hmacSHA256Hex(a.apiKey, string(body))
	var out NINVerification
	err := a.breaker.Retry(3, func() error {
		req, err := http.NewRequest(http.MethodPost, a.base+"/verify", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NIMC-Signature", sig)
		if a.apiKey != "" {
			req.Header.Set("X-API-Key", a.apiKey)
		}
		resp, err := a.hc.Do(req)
		if err != nil {
			return fmt.Errorf("nimc adapter: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			// "NIN not found" is a verification outcome, not a rail
			// failure — do not burn retries / trip the breaker.
			out = NINVerification{NINHash: ninHash, Verified: false,
				Source: "nimc_api", CredentialType: "nin_legacy",
				Detail: "NIN not found in NIMC registry"}
			return nil
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("nimc adapter: status %d: %s", resp.StatusCode, string(b))
		}
		var vr verifyResponse
		if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
			return fmt.Errorf("nimc adapter: decode: %w", err)
		}
		out = NINVerification{
			NINHash:   ninHash,
			Verified:  vr.Verified,
			FirstName: vr.FirstName,
			LastName:  vr.LastName,
			Reference: vr.Reference,
			Source:    "nimc_api",
			Detail:    vr.Detail,
		}
		if out.Reference == "" {
			out.Reference = "NIMC-" + ninHash[:16]
		}
		return nil
	})
	if err != nil {
		// log pseudonym only — never the raw NIN (§1.3/H4)
		log.Printf("nimc adapter: verify failed nin_hash=%s: %v", ninHash[:12], err)
		return NINVerification{}, err
	}
	return out, nil
}

// VerifyVNIN [REAL]: POST {base}/verify/vnin. Distinct rail outcomes map to
// Verified:false results (400 invalid-format, 404 not-found, 409/410
// token-expired); only 5xx/transport errors are retryable. The raw token is
// NEVER logged — only its HMAC pseudonym.
func (a *NIMCHTTPAdapter) VerifyVNIN(vnin string, issuedAt time.Time) (NINVerification, error) {
	vninHash := NINHash(vnin)
	if !VNINValid(vnin) {
		return NINVerification{NINHash: vninHash, Verified: false, Source: "nimc_api",
			CredentialType: "vnin", Detail: "invalid vNIN format"}, nil
	}
	if VNINExpired(issuedAt) {
		return NINVerification{NINHash: vninHash, Verified: false, Source: "nimc_api",
			CredentialType: "vnin", Detail: "vNIN token expired (72h TTL)"}, nil
	}
	body, _ := json.Marshal(map[string]string{"vnin": vnin})
	sig := hmacSHA256Hex(a.apiKey, string(body))
	var out NINVerification
	err := a.breaker.Retry(3, func() error {
		req, err := http.NewRequest(http.MethodPost, a.base+"/verify/vnin", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NIMC-Signature", sig)
		if a.apiKey != "" {
			req.Header.Set("X-API-Key", a.apiKey)
		}
		resp, err := a.hc.Do(req)
		if err != nil {
			return fmt.Errorf("nimc adapter: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusGone {
			reason := map[int]string{
				http.StatusBadRequest: "invalid vNIN format",
				http.StatusNotFound:   "vNIN not found in NIMC registry",
				http.StatusConflict:   "vNIN token expired or already consumed",
				http.StatusGone:       "vNIN token expired (72h TTL)",
			}[resp.StatusCode]
			out = NINVerification{NINHash: vninHash, Verified: false, Source: "nimc_api",
				CredentialType: "vnin", Detail: reason}
			return nil
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("nimc adapter: status %d: %s", resp.StatusCode, string(b))
		}
		var vr verifyResponse
		if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
			return fmt.Errorf("nimc adapter: decode: %w", err)
		}
		out = NINVerification{
			NINHash:        vninHash,
			Verified:       vr.Verified,
			FirstName:      vr.FirstName,
			LastName:       vr.LastName,
			Reference:      vr.Reference,
			Source:         "nimc_api",
			CredentialType: "vnin",
			Detail:         vr.Detail,
		}
		if out.Reference == "" {
			out.Reference = "NIMC-V-" + vninHash[:16]
		}
		return nil
	})
	if err != nil {
		log.Printf("nimc adapter: vnin verify failed vnin_hash=%s: %v", vninHash[:12], err)
		return NINVerification{}, err
	}
	return out, nil
}

// NewNINVerifierFromEnv wires NIMC_API_URL (+NIMC_API_KEY) → the real HTTP
// adapter; legacy NIMC_URL is honoured; unset → simulator (H1 selection
// rule) in profile=dev ONLY. In profile=prod a missing NIMC_API_URL is a
// fatal misconfiguration (audit O4/O8: no silent dev fallback in prod).
func NewNINVerifierFromEnv() NINVerifier {
	if u := os.Getenv("NIMC_API_URL"); u != "" {
		log.Printf("profile=prod component=nimc-adapter url=%s", u)
		return NewNIMCHTTPAdapter(u, os.Getenv("NIMC_API_KEY"))
	}
	if u := os.Getenv("NIMC_URL"); u != "" { // legacy alias
		log.Printf("profile=prod component=nimc-adapter url=%s (legacy NIMC_URL)", u)
		return NewNIMCHTTPAdapter(u, os.Getenv("NIMC_API_KEY"))
	}
	if workflowx.IsProdProfile() {
		log.Fatal("profile=prod FATAL: NIMC_API_URL is required (refusing to start with the NIMC simulator)")
	}
	log.Printf("profile=dev component=nimc-adapter (simulator)")
	return NIMCSimulator{}
}
