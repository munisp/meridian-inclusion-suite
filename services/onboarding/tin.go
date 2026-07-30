package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// TINProvisionResult is the outcome of TIN provisioning.
type TINProvisionResult struct {
	TIN     string `json:"tin"`
	TINHash string `json:"tin_hash"`
	Source  string `json:"source"` // tin_graph|local_fallback
	Detail  string `json:"detail,omitempty"`
}

// TINProvisioner provisions TINs via the core tin-graph service with a
// deterministic local fallback (SPEC §4 T5: "tin-provision via core tin-graph
// API w/ fallback").
type TINProvisioner interface {
	ProvisionForNIN(ninHash string) (TINProvisionResult, error)
	VerifyTIN(tin string) (bool, error)
}

// TinGraphClient calls core tin-graph (§2: POST /v1/tin/provision).
type TinGraphClient struct {
	base string
	hc   *http.Client
}

func NewTinGraphClient(base string) *TinGraphClient {
	return &TinGraphClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 10 * time.Second}}
}

func (c *TinGraphClient) ProvisionForNIN(ninHash string) (TINProvisionResult, error) {
	body, _ := json.Marshal(map[string]any{"nin_hash": ninHash, "mode": "nin_eq_tin"})
	req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/tin/provision", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dev-Role", "operator")
	resp, err := c.hc.Do(req)
	if err != nil {
		return TINProvisionResult{}, fmt.Errorf("tin-graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return TINProvisionResult{}, fmt.Errorf("tin-graph: status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		TIN string `json:"tin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return TINProvisionResult{}, err
	}
	if out.TIN == "" {
		return TINProvisionResult{}, fmt.Errorf("tin-graph: empty tin in response")
	}
	return TINProvisionResult{TIN: out.TIN, TINHash: TINHash(out.TIN), Source: "tin_graph"}, nil
}

func (c *TinGraphClient) VerifyTIN(tin string) (bool, error) {
	body, _ := json.Marshal(map[string]string{"tin": tin})
	req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/verify/tin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dev-Role", "operator")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Verified bool `json:"verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Verified, nil
}

// LocalTINProvisioner is the offline fallback: NIN=TIN fusion approximation
// deriving a deterministic 10-digit TIN from the (already pseudonymised) NIN
// hash. Deterministic so re-provisioning the same person is idempotent.
type LocalTINProvisioner struct{}

func (LocalTINProvisioner) ProvisionForNIN(ninHash string) (TINProvisionResult, error) {
	if ninHash == "" {
		return TINProvisionResult{}, fmt.Errorf("nin_hash required")
	}
	sum := sha256.Sum256([]byte("tin-fusion:" + ninHash))
	n := binary.BigEndian.Uint64(sum[:8]) % 900000000 // 9 digits
	tin := fmt.Sprintf("2%09d", n)                    // 10-digit dev TIN
	return TINProvisionResult{TIN: tin, TINHash: TINHash(tin), Source: "local_fallback", Detail: "NIN=TIN fusion approximated locally; reconcile with tin-graph when available"}, nil
}

func (LocalTINProvisioner) VerifyTIN(tin string) (bool, error) {
	return len(tin) == 10, nil
}

// fallbackProvisioner tries tin-graph first, degrades to local on error.
type fallbackProvisioner struct {
	primary  *TinGraphClient
	fallback TINProvisioner
}

func (f fallbackProvisioner) ProvisionForNIN(ninHash string) (TINProvisionResult, error) {
	res, err := f.primary.ProvisionForNIN(ninHash)
	if err == nil {
		return res, nil
	}
	res, ferr := f.fallback.ProvisionForNIN(ninHash)
	if ferr != nil {
		return TINProvisionResult{}, fmt.Errorf("tin-graph error: %v; fallback error: %w", err, ferr)
	}
	res.Detail = strings.TrimSpace(res.Detail + " (tin-graph unreachable: " + err.Error() + ")")
	return res, nil
}

func (f fallbackProvisioner) VerifyTIN(tin string) (bool, error) {
	ok, err := f.primary.VerifyTIN(tin)
	if err == nil {
		return ok, nil
	}
	return f.fallback.VerifyTIN(tin)
}

// NewTINProvisionerFromEnv wires TIN_GRAPH_URL with local fallback, or local
// only when unset.
func NewTINProvisionerFromEnv() TINProvisioner {
	if u := os.Getenv("TIN_GRAPH_URL"); u != "" {
		return fallbackProvisioner{primary: NewTinGraphClient(u), fallback: LocalTINProvisioner{}}
	}
	return LocalTINProvisioner{}
}
