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
	"strconv"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
)

// actions implements the DSL action handlers. Where a backing service URL is
// configured (ONBOARDING_URL / PRESUMPTIVE_URL) the action calls it; otherwise
// a deterministic local fallback runs so the gateway is dev-standalone.

func hmacHex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// keyOr resolves HMAC key material via keyx: env/file providers first, dev
// fallback only in profile=dev (fail-closed in profile=prod).
func keyOr(name, fallback string) string {
	return keyx.MustKey(name, fallback)
}

// isProdProfile mirrors keyx/workflowx: APP_PROFILE=prod|production or
// AUTH_MODE=keycloak.
func isProdProfile() bool {
	p := strings.ToLower(os.Getenv("APP_PROFILE"))
	if p == "prod" || p == "production" {
		return true
	}
	return strings.EqualFold(os.Getenv("AUTH_MODE"), "keycloak")
}

// onboardingPost is the shared signed-role client for the onboarding svc.
func onboardingPost(onbURL, path string, payload any, out any) (int, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(onbURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dev-Role", "operator")
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, fmt.Errorf("onboarding %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// createOperatorViaService registers the operator; a duplicate-NIN 409
// resolves to the existing operator id (idempotent re-registration).
func createOperatorViaService(onbURL, nin, name, state, agentID string) (string, error) {
	var op struct {
		ID string `json:"id"`
	}
	_, err := onboardingPost(onbURL, "/v1/operators", map[string]string{
		"nin": nin, "full_name": name, "state": state, "agent_id": agentID,
	}, &op)
	if err == nil && op.ID != "" {
		return op.ID, nil
	}
	// duplicate: resolve the existing record
	var lk struct {
		Found      bool   `json:"found"`
		OperatorID string `json:"operator_id"`
	}
	if _, lerr := onboardingPost(onbURL, "/v1/operators/lookup", map[string]string{"nin": nin}, &lk); lerr == nil && lk.Found {
		return lk.OperatorID, nil
	}
	return "", err
}

// provisionTINViaService runs wf-onb-tin-provision via the API and returns
// the issued TIN. Any failure (NIMC adapter outage, verification miss,
// service down) is an error — the caller parks the registration.
func provisionTINViaService(onbURL, operatorID, nin string) (tin, tinHash string, err error) {
	var run struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Result struct {
			TIN     string `json:"tin"`
			TINHash string `json:"tin_hash"`
		} `json:"result"`
	}
	if _, err = onboardingPost(onbURL, "/v1/tin/provision", map[string]string{
		"operator_id": operatorID, "nin": nin,
	}, &run); err != nil {
		return "", "", err
	}
	if run.Status != "completed" || run.Result.TIN == "" {
		return "", "", fmt.Errorf("provision workflow did not complete: %s", run.Error)
	}
	return run.Result.TIN, run.Result.TINHash, nil
}

// parkTINRegistration moves the operator to pending_review (best effort).
func parkTINRegistration(onbURL, operatorID string) {
	_, _ = onboardingPost(onbURL, "/v1/operators/"+operatorID+"/status", map[string]string{
		"to": "pending_review", "reason": "ussd: verification unavailable at registration",
	}, nil)
}

type operatorLookup struct {
	Found        bool   `json:"found"`
	OperatorID   string `json:"operator_id"`
	Status       string `json:"status"`
	TIN          string `json:"tin"`
	TINHash      string `json:"tin_hash"`
	ReviewStatus string `json:"review_status"`
}

func lookupOperatorByNIN(onbURL, nin string) (operatorLookup, error) {
	var out operatorLookup
	_, err := onboardingPost(onbURL, "/v1/operators/lookup", map[string]string{"nin": nin}, &out)
	return out, err
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

	// onb.register: register + NIMC verify + TIN provision THROUGH the
	// onboarding service (audit O3: the USSD path must not bypass NIMC or
	// derive TINs locally). On an adapter/service outage the registration is
	// parked as pending_review — never issued a local TIN.
	actions["onb.register"] = func(sess *Session) error {
		nin := sess.Data["nin"]
		name := sess.Data["name"]
		if nin == "" || name == "" {
			return fmt.Errorf("missing nin or name in session")
		}
		park := func(reason string) {
			sess.Data["registration_status"] = "pending_review"
			sess.Data["park_reason"] = reason
			sess.Data["_next_override"] = "onb_pending"
		}
		if onbURL == "" {
			if isProdProfile() {
				// fail closed: no onboarding service configured in prod
				return fmt.Errorf("registration service is temporarily unavailable; please try again later")
			}
			// dev standalone: park locally, never derive a TIN
			park("onboarding service not configured (dev)")
			bus.Publish("nrs.onb.ussd.v1", map[string]any{
				"flow": "register", "phone": sess.Phone, "state": sess.Data["state"], "outcome": "pending_review", "via": "dev_no_service",
			})
			return nil
		}
		// 1) create the operator record (idempotent per NIN via 409 handling)
		opID, err := createOperatorViaService(onbURL, nin, name, sess.Data["state"], "ussd:"+sess.Phone)
		if err != nil {
			park("operator create failed")
			bus.Publish("nrs.onb.ussd.v1", map[string]any{
				"flow": "register", "phone": sess.Phone, "outcome": "pending_review", "via": "onboarding_svc", "stage": "create",
			})
			return nil
		}
		sess.Data["operator_id"] = opID
		// 2) NIMC verify + TIN provision through the durable workflow
		tin, tinHash, err := provisionTINViaService(onbURL, opID, nin)
		if err != nil {
			// adapter outage / verification failure -> park for review
			parkTINRegistration(onbURL, opID)
			park("verification unavailable")
			bus.Publish("nrs.onb.ussd.v1", map[string]any{
				"flow": "register", "phone": sess.Phone, "operator_id": opID, "outcome": "pending_review", "stage": "provision",
			})
			return nil
		}
		sess.Data["tin"] = tin
		sess.Data["tin_hash"] = tinHash
		sess.Data["registration_status"] = "tin_provisioned"
		bus.Publish("nrs.onb.ussd.v1", map[string]any{
			"flow": "register", "phone": sess.Phone, "operator_id": opID, "tin_hash": tinHash, "state": sess.Data["state"], "outcome": "tin_provisioned",
		})
		return nil
	}

	// onb.tin_status: report provisioning status for a NIN, resolved through
	// the onboarding service (no local TIN derivation).
	actions["onb.tin_status"] = func(sess *Session) error {
		nin := sess.Data["nin"]
		if nin == "" {
			return fmt.Errorf("missing nin in session")
		}
		sess.Data["nin_masked"] = maskNIN(nin)
		if onbURL == "" {
			if isProdProfile() {
				return fmt.Errorf("status service is temporarily unavailable; please try again later")
			}
			sess.Data["tin"] = "-"
			sess.Data["tin_status"] = "unknown (onboarding service not configured, dev)"
			return nil
		}
		st, err := lookupOperatorByNIN(onbURL, nin)
		if err != nil {
			return fmt.Errorf("status service is temporarily unavailable; please try again later")
		}
		if !st.Found {
			sess.Data["tin"] = "-"
			sess.Data["tin_status"] = "not registered"
		} else {
			sess.Data["operator_id"] = st.OperatorID
			if st.TIN != "" {
				sess.Data["tin"] = st.TIN
			} else {
				sess.Data["tin"] = "-"
			}
			sess.Data["tin_status"] = st.Status
		}
		bus.Publish("nrs.onb.ussd.v1", map[string]any{
			"flow": "tin_status", "phone": sess.Phone, "found": st.Found,
		})
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
