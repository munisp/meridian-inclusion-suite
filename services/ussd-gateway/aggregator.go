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
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/otelx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/resilience"
)

// Aggregator adapter (H4): inbound webhook HMAC verification via
// USSD_AGGREGATOR_KEY and an outbound notify client via USSD_AGGREGATOR_URL,
// with the shared resilience policy. Raw MSISDNs are never logged — only a
// hash prefix.

// aggregatorKey returns the HMAC key for aggregator signatures; empty when
// unset (dev: signature checks are skipped and logged once).
func aggregatorKey() string { return os.Getenv("USSD_AGGREGATOR_KEY") }

// signAggregator computes hex HMAC-SHA256(key, body).
func signAggregator(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyAggregatorSignature validates the inbound webhook
// X-Aggregator-Signature header: hex HMAC-SHA256(USSD_AGGREGATOR_KEY, raw
// body). In dev (key unset) every webhook is accepted. In profile=prod a
// missing key fails closed (reject-all) rather than silently disabling
// signature checks on a misdeployed gateway.
func VerifyAggregatorSignature(signature string, body []byte) bool {
	key := aggregatorKey()
	if key == "" {
		if keyx.Prod() {
			log.Printf("profile=prod component=ussd-gateway aggregator_key=missing action=reject-all (set USSD_AGGREGATOR_KEY)")
			return false // fail closed: no key in prod => nothing verifies
		}
		return true // dev fallback
	}
	if signature == "" {
		return false
	}
	want := signAggregator(key, body)
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}

// msisdnHash pseudonymises an MSISDN for logs/events. B4-9: keyed
// HMAC-SHA256 — an unsalted SHA-256 of a phone number is dictionary-
// reversible in seconds (small MSISDN space). Key from
// USSD_MSISDN_HMAC_KEY; keyx.MustKey fails closed in prod when unset.
func msisdnHash(phone string) string {
	mac := hmac.New(sha256.New, []byte(keyx.MustKey("USSD_MSISDN_HMAC_KEY", "dev-msisdn-hmac-do-not-deploy")))
	mac.Write([]byte("msisdn:" + phone))
	return hex.EncodeToString(mac.Sum(nil))[:12]
}

// AggregatorNotifier is the outbound notify client (delivery receipts,
// session-end push notifications) against {USSD_AGGREGATOR_URL}/notify.
// Payloads are HMAC-signed with USSD_AGGREGATOR_KEY.
type AggregatorNotifier struct {
	base    string
	key     string
	hc      *http.Client
	breaker *resilience.Breaker
}

func NewAggregatorNotifier(base, key string) *AggregatorNotifier {
	return &AggregatorNotifier{
		base:    strings.TrimRight(base, "/"),
		key:     key,
		hc:      &http.Client{Timeout: 10 * time.Second, Transport: otelx.Client(nil)},
		breaker: &resilience.Breaker{Threshold: 5, Cooldown: 30 * time.Second},
	}
}

// Notify posts a notification {session_id, phone, message}. Never logs the
// raw MSISDN.
func (n *AggregatorNotifier) Notify(sessionID, phone, message string) error {
	body, _ := json.Marshal(map[string]string{
		"session_id": sessionID, "phone": phone, "message": message,
	})
	err := n.breaker.Retry(3, func() error {
		req, err := http.NewRequest(http.MethodPost, n.base+"/notify", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if n.key != "" {
			req.Header.Set("X-Aggregator-Signature", signAggregator(n.key, body))
			req.Header.Set("X-API-Key", n.key)
		}
		resp, err := n.hc.Do(req)
		if err != nil {
			return fmt.Errorf("aggregator notify: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("aggregator notify: status %d: %s", resp.StatusCode, string(b))
		}
		io.Copy(io.Discard, resp.Body)
		return nil
	})
	if err != nil {
		log.Printf("aggregator notify failed session=%s msisdn_hash=%s: %v", sessionID, msisdnHash(phone), err)
	}
	return err
}

// NewAggregatorNotifierFromEnv wires USSD_AGGREGATOR_URL → real notify
// client (profile=prod); unset → nil (dev: notifications are dropped, the
// built-in simulator remains the way to exercise sessions).
func NewAggregatorNotifierFromEnv() *AggregatorNotifier {
	if u := os.Getenv("USSD_AGGREGATOR_URL"); u != "" {
		log.Printf("profile=prod component=ussd-aggregator url=%s", u)
		return NewAggregatorNotifier(u, aggregatorKey())
	}
	log.Printf("profile=dev component=ussd-aggregator (simulator)")
	return nil
}
