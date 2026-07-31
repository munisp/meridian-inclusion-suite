package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// associations.go — market-association bulk onboarding (I16): informal-sector
// associations (market unions, artisan guilds) onboard their whole roster via
// CSV upload or USSD-driven member lists. Every member becomes a CaptureItem
// with a deterministic per-association client_ref, so re-uploads dedup
// cleanly; NIN-hash dedup against the registry (pseudonymised, consistent
// with the tin-graph HMAC scheme) blocks double enrolment across agents.

// Association is a verified market association / guild.
type Association struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	LGA        string `json:"lga"`
	AdminName  string `json:"admin_name"`
	AdminPhone string `json:"admin_phone"`
	Status     string `json:"status"` // active|suspended
	CreatedAt  string `json:"created_at"`
}

// AssociationService manages the registry + bulk enrolment.
type AssociationService struct {
	st      *store.Store
	reg     *Registry
	capture *CaptureService
}

func NewAssociationService(st *store.Store, reg *Registry, cap *CaptureService) *AssociationService {
	return &AssociationService{st: st, reg: reg, capture: cap}
}

func (a *AssociationService) Create(in Association) (Association, error) {
	if in.Name == "" || in.AdminName == "" {
		return Association{}, fmt.Errorf("name and admin_name are required")
	}
	in.ID = ids.WithPrefix("assoc")
	in.Status = "active"
	in.CreatedAt = nowRFC3339()
	if err := a.st.Put("associations", in.ID, in); err != nil {
		return Association{}, err
	}
	return in, nil
}

func (a *AssociationService) Get(id string) (Association, bool, error) {
	var as Association
	ok, err := a.st.Get("associations", id, &as)
	return as, ok, err
}

// BulkResult summarises a bulk enrolment run.
type BulkResult struct {
	AssociationID string              `json:"association_id"`
	Rows          int                 `json:"rows"`
	BatchID       string              `json:"batch_id"`
	Results       []CaptureItemResult `json:"results"`
	Duplicates    int                 `json:"duplicates"`
	Created       int                 `json:"created"`
}

// EnrollCSV ingests a roster CSV (header: nin,full_name,phone,state,lga,
// trade_category) as one capture batch. client_ref is deterministic per
// (association, nin) so re-uploading the same roster dedups instead of
// double-enrolling; NIN-hash conflicts resolve via the capture service.
func (a *AssociationService) EnrollCSV(assocID, agentID string, r io.Reader) (BulkResult, error) {
	as, ok, err := a.Get(assocID)
	if err != nil || !ok {
		return BulkResult{}, fmt.Errorf("association %s not found", assocID)
	}
	if as.Status != "active" {
		return BulkResult{}, fmt.Errorf("association %s is %s", assocID, as.Status)
	}
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return BulkResult{}, fmt.Errorf("csv header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, req := range []string{"nin", "full_name"} {
		if _, ok := col[req]; !ok {
			return BulkResult{}, fmt.Errorf("csv must have columns nin,full_name[,phone,state,lga,trade_category]")
		}
	}
	get := func(row []string, name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	var items []CaptureItem
	now := nowRFC3339()
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return BulkResult{}, fmt.Errorf("csv row: %w", err)
		}
		nin := get(row, "nin")
		if nin == "" {
			continue
		}
		items = append(items, CaptureItem{
			ClientRef:     fmt.Sprintf("assoc:%s:%s", assocID, NINHash(nin)[:16]),
			NIN:           nin,
			FullName:      get(row, "full_name"),
			Phone:         get(row, "phone"),
			State:         firstNonEmpty(get(row, "state"), as.State),
			LGA:           firstNonEmpty(get(row, "lga"), as.LGA),
			TradeCategory: get(row, "trade_category"),
			CapturedAt:    now,
		})
	}
	if len(items) == 0 {
		return BulkResult{}, fmt.Errorf("no member rows found")
	}
	idemKey := fmt.Sprintf("assoc-bulk-%s-%d", assocID, time.Now().UTC().UnixNano())
	batch, err := a.capture.Ingest(agentID, idemKey, items)
	if err != nil {
		return BulkResult{}, err
	}
	res := BulkResult{AssociationID: assocID, Rows: len(items), BatchID: batch.ID, Results: batch.Results}
	for _, r := range batch.Results {
		switch r.Outcome {
		case "created":
			res.Created++
		case "duplicate_client_ref", "conflict_resolved":
			res.Duplicates++
		}
	}
	return res, nil
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<22)).Decode(v)
}

// --- HTTP handlers ---

func (s *server) createAssociation(w http.ResponseWriter, r *http.Request) {
	var in Association
	if err := decodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	as, err := s.associations.Create(in)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, as)
}

func (s *server) bulkEnrollAssociation(w http.ResponseWriter, r *http.Request) {
	agentID := httpx.RequestIdentity(r)
	if agentID == "" {
		agentID = "agent-unknown"
	}
	res, err := s.associations.EnrollCSV(r.PathValue("id"), agentID, r.Body)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "bulk_enroll", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
