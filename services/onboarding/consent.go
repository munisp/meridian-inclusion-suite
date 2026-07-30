package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// ConsentService captures NDPA consent via the core consent svc with a local
// fallback store (SPEC §4 T5: "consent capture (core consent svc API,
// fallback local)").
type ConsentService struct {
	base  string // CONSENT_URL, "" => local only
	st    *store.Store
	hc    *http.Client
	local bool
}

func NewConsentService(st *store.Store) *ConsentService {
	return &ConsentService{
		base:  strings.TrimRight(os.Getenv("CONSENT_URL"), "/"),
		st:    st,
		hc:    &http.Client{Timeout: 10 * time.Second},
		local: os.Getenv("CONSENT_URL") == "",
	}
}

// Capture records consent; returns the consent record with an NDPA receipt.
func (c *ConsentService) Capture(subject, purpose, channel string, granted bool) (ConsentRecord, error) {
	if subject == "" || purpose == "" {
		return ConsentRecord{}, fmt.Errorf("subject and purpose are required")
	}
	if !c.local {
		body, _ := json.Marshal(map[string]any{
			"subject": subject, "purpose": purpose, "channel": channel, "granted": granted,
		})
		req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/consents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dev-Role", "operator")
		resp, err := c.hc.Do(req)
		if err == nil && resp.StatusCode < 300 {
			defer resp.Body.Close()
			var rec ConsentRecord
			if derr := json.NewDecoder(resp.Body).Decode(&rec); derr == nil {
				rec.Source = "consent_svc"
				// mirror locally for offline reads
				if rec.ID == "" {
					rec.ID = ids.WithPrefix("cns")
				}
				_ = c.st.Put("consents", rec.ID, rec)
				return rec, nil
			}
		} else if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		// fall through to local fallback on any transport/5xx error
	}
	rec := ConsentRecord{
		ID:        ids.WithPrefix("cns"),
		Subject:   subject,
		Purpose:   purpose,
		Channel:   channel,
		Granted:   granted,
		Receipt:   "NDPA-RCPT-" + ids.New(),
		CreatedAt: nowRFC3339(),
		Source:    "local_fallback",
	}
	if err := c.st.Put("consents", rec.ID, rec); err != nil {
		return ConsentRecord{}, err
	}
	return rec, nil
}

func (c *ConsentService) GetForSubject(subject string) ([]ConsentRecord, error) {
	var all []ConsentRecord
	if err := c.st.List("consents", &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if r.Subject == subject {
			out = append(out, r)
		}
	}
	return out, nil
}

// Revoke marks a consent revoked (NDPA withdrawal right).
func (c *ConsentService) Revoke(id string) (ConsentRecord, error) {
	var rec ConsentRecord
	ok, err := c.st.Get("consents", id, &rec)
	if err != nil {
		return ConsentRecord{}, err
	}
	if !ok {
		return ConsentRecord{}, fmt.Errorf("consent %s not found", id)
	}
	rec.Revoked = true
	rec.Granted = false
	if !c.local {
		req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/consents/"+id+"/revoke", nil)
		req.Header.Set("X-Dev-Role", "operator")
		resp, err := c.hc.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	if err := c.st.Put("consents", id, rec); err != nil {
		return ConsentRecord{}, err
	}
	return rec, nil
}
