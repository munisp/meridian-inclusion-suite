package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// actions implements the DSL action handlers. Where a backing service URL is
// configured (ONBOARDING_URL / PRESUMPTIVE_URL) the action calls it; otherwise
// a deterministic local fallback runs so the gateway is dev-standalone.

func hmacHex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func keyOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// deriveTIN mirrors services/onboarding LocalTINProvisioner (NIN=TIN fusion
// approximation) so USSD-only registrations stay consistent.
func deriveTIN(nin string) string {
	ninHash := hmacHex(keyOr("NIN_HMAC_KEY", "meridian-dev-nin-key"), nin)
	sum := sha256.Sum256([]byte("tin-fusion:" + ninHash))
	n := binary.BigEndian.Uint64(sum[:8]) % 900000000
	return fmt.Sprintf("2%09d", n)
}

func maskNIN(nin string) string {
	if len(nin) < 7 {
		return "***"
	}
	return nin[:3] + "*****" + nin[len(nin)-3:]
}

func koboToNaira(kobo uint64) string {
	return strconv.FormatFloat(float64(kobo)/100.0, 'f', 2, 64)
}

// levyTable is the embedded fallback band table (mirror of the presumptive
// packs' small-band defaults, kobo/yr) used when PRESUMPTIVE_URL is unset.
var levyTable = map[string]map[string]uint64{
	// state -> turnover_kobo bucket -> levy kobo
	"lagos":   {"50000000": 500000, "90000000": 500000, "300000000": 1500000, "1500000000": 3500000},
	"kano":    {"50000000": 200000, "90000000": 200000, "300000000": 700000, "1500000000": 1800000},
	"federal": {"50000000": 300000, "90000000": 300000, "300000000": 1000000, "1500000000": 2500000},
}

func adminFee(state string) uint64 {
	switch strings.ToLower(state) {
	case "lagos":
		return 10000
	case "kano":
		return 3000
	default:
		return 5000
	}
}

func bandName(turnoverKobo uint64) string {
	switch {
	case turnoverKobo <= 80000000:
		return "exempt"
	case turnoverKobo <= 100000000:
		return "micro"
	case turnoverKobo <= 500000000:
		return "small"
	default:
		return "medium"
	}
}

func httpClient() *http.Client { return &http.Client{Timeout: 8 * time.Second} }

// RegisterActions builds the action handler registry.
func RegisterActions(bus eventPublisher) map[string]ActionHandler {
	onbURL := strings.TrimRight(os.Getenv("ONBOARDING_URL"), "/")
	psmURL := strings.TrimRight(os.Getenv("PRESUMPTIVE_URL"), "/")

	actions := map[string]ActionHandler{}

	// onb.register: NIN verify + TIN provision (service or local fallback).
	actions["onb.register"] = func(sess *Session) error {
		nin := sess.Data["nin"]
		name := sess.Data["name"]
		if nin == "" || name == "" {
			return fmt.Errorf("missing nin or name in session")
		}
		if onbURL != "" {
			// try the onboarding service
			body, _ := json.Marshal(map[string]string{
				"nin": nin, "full_name": name, "state": sess.Data["state"], "agent_id": "ussd:" + sess.Phone,
			})
			req, _ := http.NewRequest(http.MethodPost, onbURL+"/v1/operators", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Dev-Role", "operator")
			resp, err := httpClient().Do(req)
			if err == nil && resp.StatusCode < 300 {
				var op struct {
					ID string `json:"id"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&op)
				resp.Body.Close()
				sess.Data["operator_id"] = op.ID
			} else if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
		tin := deriveTIN(nin)
		sess.Data["tin"] = tin
		sess.Data["tin_hash"] = hmacHex(keyOr("TIN_HMAC_KEY", "meridian-dev-tin-key"), tin)
		bus.Publish("nrs.onb.ussd.v1", map[string]any{
			"flow": "register", "phone": sess.Phone, "tin_hash": sess.Data["tin_hash"], "state": sess.Data["state"],
		})
		return nil
	}

	// onb.tin_status: report provisioning status for a NIN.
	actions["onb.tin_status"] = func(sess *Session) error {
		nin := sess.Data["nin"]
		if nin == "" {
			return fmt.Errorf("missing nin in session")
		}
		sess.Data["nin_masked"] = maskNIN(nin)
		tin := deriveTIN(nin)
		sess.Data["tin"] = tin
		sess.Data["tin_status"] = "provisioned (NIN=TIN fusion)"
		bus.Publish("nrs.onb.ussd.v1", map[string]any{"flow": "tin_status", "phone": sess.Phone})
		return nil
	}

	// psm.band_lookup: evaluate the presumptive band (service or fallback).
	actions["psm.band_lookup"] = func(sess *Session) error {
		state := sess.Data["state"]
		trade := sess.Data["trade"]
		turnover, _ := strconv.ParseUint(sess.Data["turnover_kobo"], 10, 64)
		if state == "" || trade == "" {
			return fmt.Errorf("missing state or trade in session")
		}
		if turnover <= 80000000 {
			return fmt.Errorf("Your annual turnover is below the N800,000 tax-free threshold (NTA 2025 First Schedule). You are EXEMPT from the presumptive levy.")
		}
		var levy, fee uint64
		var band string
		if psmURL != "" {
			body, _ := json.Marshal(map[string]any{
				"state": state, "trade_category": trade, "annual_turnover_kobo": turnover,
			})
			req, _ := http.NewRequest(http.MethodPost, psmURL+"/v1/bands/evaluate", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Dev-Role", "operator")
			resp, err := httpClient().Do(req)
			if err == nil && resp.StatusCode < 300 {
				var eval struct {
					Band           string `json:"band"`
					AnnualLevyKobo uint64 `json:"annual_levy_kobo"`
					AdminFeeKobo   uint64 `json:"admin_fee_kobo"`
					Exempt         bool   `json:"exempt"`
					ExemptReason   string `json:"exempt_reason"`
				}
				if derr := json.NewDecoder(resp.Body).Decode(&eval); derr == nil {
					resp.Body.Close()
					if eval.Exempt {
						return fmt.Errorf("%s", eval.ExemptReason)
					}
					band, levy, fee = eval.Band, eval.AnnualLevyKobo, eval.AdminFeeKobo
				}
			}
			if resp != nil && resp.Body != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
		if band == "" { // fallback table
			st := strings.ToLower(state)
			if _, ok := levyTable[st]; !ok {
				st = "federal"
			}
			levy = levyTable[st][sess.Data["turnover_kobo"]]
			fee = adminFee(state)
			band = bandName(turnover)
		}
		sess.Data["band"] = band
		sess.Data["levy_kobo"] = strconv.FormatUint(levy+fee, 10)
		sess.Data["levy_naira"] = koboToNaira(levy + fee)
		sess.Data["fee_naira"] = koboToNaira(fee)
		bus.Publish("nrs.psm.ussd.v1", map[string]any{
			"flow": "band_lookup", "phone": sess.Phone, "state": state, "band": band, "levy_kobo": levy + fee,
		})
		return nil
	}

	// psm.pay: collect via presumptive svc when available; else simulate and
	// issue a locally-signed certificate serial (SMS simulated by transcript).
	actions["psm.pay"] = func(sess *Session) error {
		levy, _ := strconv.ParseUint(sess.Data["levy_kobo"], 10, 64)
		if levy == 0 {
			return fmt.Errorf("no levy computed; restart the flow")
		}
		if psmURL != "" {
			body, _ := json.Marshal(map[string]any{
				"tin_hash": sess.Data["tin_hash"], "state": sess.Data["state"],
				"trade_category": sess.Data["trade"], "annual_turnover_kobo": mustU64(sess.Data["turnover_kobo"]),
				"provider": "flutterwave",
			})
			req, _ := http.NewRequest(http.MethodPost, psmURL+"/v1/workflows/wf-psm-payment/trigger", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Dev-Role", "operator")
			resp, err := httpClient().Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode < 300 {
					var run struct {
						Status string `json:"status"`
						Result struct {
							Certificate struct {
								Serial string `json:"serial"`
							} `json:"certificate"`
							Payment struct {
								PSSPRef string `json:"pssp_ref"`
							} `json:"payment"`
						} `json:"result"`
					}
					if derr := json.NewDecoder(resp.Body).Decode(&run); derr == nil && run.Status == "completed" && run.Result.Certificate.Serial != "" {
						sess.Data["cert_serial"] = run.Result.Certificate.Serial
						sess.Data["pssp_ref"] = run.Result.Payment.PSSPRef
						bus.Publish("nrs.psm.ussd.v1", map[string]any{
							"flow": "pay", "phone": sess.Phone, "cert_serial": run.Result.Certificate.Serial, "via": "presumptive_svc",
						})
						return nil
					}
				}
			}
		}
		// local fallback: simulated authorise->capture + serial
		seed := sha256.Sum256([]byte(sess.ID + "|" + sess.Data["levy_kobo"]))
		sess.Data["pssp_ref"] = "FLW-" + strings.ToUpper(hex.EncodeToString(seed[:8]))
		sess.Data["cert_serial"] = fmt.Sprintf("PSM-%d-%s", time.Now().UTC().Year(), strings.ToUpper(hex.EncodeToString(seed[8:13])))
		bus.Publish("nrs.psm.ussd.v1", map[string]any{
			"flow": "pay", "phone": sess.Phone, "cert_serial": sess.Data["cert_serial"], "via": "local_simulator",
		})
		return nil
	}

	// psm.cert_verify: verify a certificate serial.
	actions["psm.cert_verify"] = func(sess *Session) error {
		serial := sess.Data["serial"]
		if serial == "" {
			return fmt.Errorf("no serial entered")
		}
		if psmURL != "" {
			req, _ := http.NewRequest(http.MethodGet, psmURL+"/v1/certificates/verify/"+serial, nil)
			resp, err := httpClient().Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == 200 {
					var out struct {
						Valid       bool `json:"valid"`
						Certificate struct {
							Band       string `json:"band"`
							AmountKobo uint64 `json:"amount_kobo"`
							Period     string `json:"period"`
						} `json:"certificate"`
					}
					if derr := json.NewDecoder(resp.Body).Decode(&out); derr == nil {
						if !out.Valid {
							return fmt.Errorf("certificate invalid or tampered")
						}
						sess.Data["cert_band"] = out.Certificate.Band
						sess.Data["cert_amount_naira"] = koboToNaira(out.Certificate.AmountKobo)
						sess.Data["cert_period"] = out.Certificate.Period
						return nil
					}
				}
				if resp.StatusCode == 404 {
					return fmt.Errorf("certificate not found")
				}
			}
		}
		// fallback: format-plausible serials are reported as unverifiable offline
		return fmt.Errorf("verification service unreachable; serial format valid but cannot confirm offline")
	}

	return actions
}

func mustU64(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// eventPublisher abstracts the inproc bus for testability.
type eventPublisher interface {
	Publish(topic string, data map[string]any)
}
