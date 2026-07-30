package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// NINVerification is the result of an NIMC identity verification.
type NINVerification struct {
	NINHash   string `json:"nin_hash"`
	Verified  bool   `json:"verified"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Reference string `json:"reference"`
	Source    string `json:"source"` // nimc_api|simulator
	Detail    string `json:"detail,omitempty"`
}

// NINVerifier is the NIMC adapter interface (external rail behind interface).
type NINVerifier interface {
	VerifyNIN(nin string) (NINVerification, error)
}

func hmacSHA256Hex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// NINHash pseudonymises a NIN per §1.3 (HMAC-SHA256, NIN_HMAC_KEY).
func NINHash(nin string) string {
	key := os.Getenv("NIN_HMAC_KEY")
	if key == "" {
		key = "meridian-dev-nin-key"
	}
	return hmacSHA256Hex(key, nin)
}

// TINHash pseudonymises a TIN per §1.3 (HMAC-SHA256, TIN_HMAC_KEY).
func TINHash(tin string) string {
	key := os.Getenv("TIN_HMAC_KEY")
	if key == "" {
		key = "meridian-dev-tin-key"
	}
	return hmacSHA256Hex(key, tin)
}

var ninPattern = regexp.MustCompile(`^\d{11}$`)

// NIMCSimulator is the deterministic offline NIMC simulator. Valid 11-digit
// NINs verify; NINs ending in "0000" simulate a mismatch outcome.
type NIMCSimulator struct{}

func (NIMCSimulator) VerifyNIN(nin string) (NINVerification, error) {
	v := NINVerification{NINHash: NINHash(nin), Source: "simulator"}
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

// NIMCHTTPAdapter calls a real NIMC-side verification endpoint (NIMC_URL).
type NIMCHTTPAdapter struct {
	base string
	hc   *http.Client
}

func NewNIMCHTTPAdapter(base string) *NIMCHTTPAdapter {
	return &NIMCHTTPAdapter{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 10 * time.Second}}
}

func (a *NIMCHTTPAdapter) VerifyNIN(nin string) (NINVerification, error) {
	body, _ := json.Marshal(map[string]string{"nin": nin})
	resp, err := a.hc.Post(a.base+"/v1/verify/nin", "application/json", bytes.NewReader(body))
	if err != nil {
		return NINVerification{}, fmt.Errorf("nimc adapter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return NINVerification{}, fmt.Errorf("nimc adapter: status %d: %s", resp.StatusCode, string(b))
	}
	var v NINVerification
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return NINVerification{}, err
	}
	v.Source = "nimc_api"
	if v.NINHash == "" {
		v.NINHash = NINHash(nin)
	}
	return v, nil
}

// NewNINVerifierFromEnv wires NIMC_URL → HTTP adapter, else the simulator.
func NewNINVerifierFromEnv() NINVerifier {
	if u := os.Getenv("NIMC_URL"); u != "" {
		return NewNIMCHTTPAdapter(u)
	}
	return NIMCSimulator{}
}
