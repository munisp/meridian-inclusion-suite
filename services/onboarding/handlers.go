package main

import (
	"net/http"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
)

// server bundles the onboarding service dependencies.
type server struct {
	registry     *Registry
	verifier     NINVerifier
	provisioner  TINProvisioner
	consent      *ConsentService
	capture      *CaptureService
	workflows    *Workflows
	associations *AssociationService
	crdt         *CRDTMergeService
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.Healthz(serviceName, serviceVersion))
	mux.HandleFunc("GET /readyz", httpx.Readyz(nil))

	mux.HandleFunc("POST /v1/operators", s.createOperator)
	mux.HandleFunc("GET /v1/operators", s.listOperators)
	mux.HandleFunc("GET /v1/operators/{id}", s.getOperator)
	mux.HandleFunc("PATCH /v1/operators/{id}", s.patchOperator)

	mux.HandleFunc("POST /v1/verify/nin", s.verifyNIN)
	mux.HandleFunc("POST /v1/tin/provision", s.provisionTIN)
	mux.HandleFunc("POST /v1/verify/tin", s.verifyTIN)

	mux.HandleFunc("POST /v1/consents", s.captureConsent)
	mux.HandleFunc("GET /v1/consents/{subject}", s.getConsents)
	mux.HandleFunc("POST /v1/consents/{id}/revoke", s.revokeConsent)

	mux.HandleFunc("POST /v1/capture/batch", s.captureBatch)
	mux.HandleFunc("GET /v1/commissions/summary", s.commissionSummary)
	mux.HandleFunc("POST /v1/associations", s.createAssociation)
	mux.HandleFunc("POST /v1/associations/{id}/bulk", s.bulkEnrollAssociation)
	mux.HandleFunc("POST /v1/capture/merge", s.crdtMerge)
	mux.HandleFunc("GET /v1/capture/batch/{key}", s.getBatch)

	mux.HandleFunc("GET /v1/workflows", s.listWorkflows)
	mux.HandleFunc("POST /v1/workflows/{name}/trigger", s.triggerWorkflow)
	mux.HandleFunc("GET /v1/workflows/runs", s.listRuns)
	mux.HandleFunc("POST /v1/graduation/candidates", s.stageGraduation)
	return mux
}

func (s *server) createOperator(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NIN           string `json:"nin"`
		FullName      string `json:"full_name"`
		Phone         string `json:"phone"`
		State         string `json:"state"`
		LGA           string `json:"lga"`
		TradeCategory string `json:"trade_category"`
		AgentID       string `json:"agent_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if in.NIN == "" || in.FullName == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "nin and full_name are required")
		return
	}
	ninHash := NINHash(in.NIN)
	if existing, found, _ := s.registry.FindByNINHash(ninHash); found {
		httpx.WriteProblem(w, http.StatusConflict, "duplicate_nin", "operator already registered as "+existing.ID)
		return
	}
	op := Operator{
		NINHash:       ninHash,
		FullName:      in.FullName,
		Phone:         in.Phone,
		State:         in.State,
		LGA:           in.LGA,
		TradeCategory: in.TradeCategory,
		AgentID:       in.AgentID,
		CapturedAt:    nowRFC3339(),
		SyncedAt:      nowRFC3339(),
	}
	if err := s.registry.Create(&op); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, op)
}

func (s *server) listOperators(w http.ResponseWriter, r *http.Request) {
	ops, err := s.registry.List()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ops == nil {
		ops = []Operator{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"operators": ops, "count": len(ops)})
}

func (s *server) getOperator(w http.ResponseWriter, r *http.Request) {
	op, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, op)
}

func (s *server) patchOperator(w http.ResponseWriter, r *http.Request) {
	op, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	var in map[string]string
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	for k, v := range in {
		switch k {
		case "full_name":
			op.FullName = v
		case "phone":
			op.Phone = v
		case "state":
			op.State = v
		case "lga":
			op.LGA = v
		case "trade_category":
			op.TradeCategory = v
		case "status":
			switch v {
			case "registered", "nin_verified", "tin_provisioned", "graduated":
				op.Status = v
			default:
				httpx.WriteProblem(w, http.StatusBadRequest, "validation", "invalid status "+v)
				return
			}
		}
	}
	if err := s.registry.Update(op); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, op)
}

func (s *server) verifyNIN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NIN string `json:"nin"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.NIN == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "nin is required")
		return
	}
	v, err := s.verifier.VerifyNIN(in.NIN)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadGateway, "nimc_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *server) provisionTIN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OperatorID string `json:"operator_id"`
		NIN        string `json:"nin"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if in.OperatorID == "" || in.NIN == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "operator_id and nin are required")
		return
	}
	run := s.workflows.TINProvision(in.OperatorID, in.NIN)
	if run.Status == "failed" {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "workflow_failed", run.Error)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

func (s *server) verifyTIN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TIN string `json:"tin"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.TIN == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "tin is required")
		return
	}
	ok, err := s.provisioner.VerifyTIN(in.TIN)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadGateway, "tin_graph_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tin_hash": TINHash(in.TIN), "verified": ok})
}

func (s *server) captureConsent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject string `json:"subject"`
		Purpose string `json:"purpose"`
		Channel string `json:"channel"`
		Granted *bool  `json:"granted"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	granted := true
	if in.Granted != nil {
		granted = *in.Granted
	}
	if in.Channel == "" {
		in.Channel = "agent_pwa"
	}
	rec, err := s.consent.Capture(in.Subject, in.Purpose, in.Channel, granted)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec)
}

func (s *server) getConsents(w http.ResponseWriter, r *http.Request) {
	recs, err := s.consent.GetForSubject(r.PathValue("subject"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if recs == nil {
		recs = []ConsentRecord{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"consents": recs})
}

func (s *server) revokeConsent(w http.ResponseWriter, r *http.Request) {
	rec, err := s.consent.Revoke(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

func (s *server) captureBatch(w http.ResponseWriter, r *http.Request) {
	idemKey := r.Header.Get("Idempotency-Key")
	var in struct {
		AgentID string        `json:"agent_id"`
		Items   []CaptureItem `json:"items"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if in.AgentID == "" {
		in.AgentID = "unknown-agent"
	}
	batch, err := s.capture.Ingest(in.AgentID, idemKey, in.Items)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "ingest_error", err.Error())
		return
	}
	status := http.StatusOK
	if batch.Status == "processed" {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, batch)
}

func (s *server) getBatch(w http.ResponseWriter, r *http.Request) {
	var batch CaptureBatch
	ok, err := s.workflows.st.Get("capture_batches", r.PathValue("key"), &batch)
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "batch not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, batch)
}

func (s *server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"workflows": s.workflows.Names()})
}

func (s *server) triggerWorkflow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var input map[string]any
	if r.Body != nil && r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &input); err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	if input == nil {
		input = map[string]any{}
	}
	if _, ok := input["operator_id"]; !ok {
		if v := r.URL.Query().Get("operator_id"); v != "" {
			input["operator_id"] = v
		}
	}
	run, err := s.workflows.Run(name, input)
	if err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "unknown_workflow", err.Error())
		return
	}
	status := http.StatusOK
	if run.Status == "failed" {
		status = http.StatusUnprocessableEntity
	}
	httpx.WriteJSON(w, status, run)
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	var runs []WorkflowRun
	if err := s.workflows.st.List("workflow_runs", &runs); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if runs == nil {
		runs = []WorkflowRun{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *server) stageGraduation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OperatorID            string `json:"operator_id"`
		EstimatedTurnoverKobo uint64 `json:"estimated_turnover_kobo"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.OperatorID == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "operator_id is required")
		return
	}
	id := ids.WithPrefix("grad")
	if err := s.workflows.st.Put("graduation_candidates", id, in); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "staged": in})
}

// publicPath marks endpoints that bypass dev auth.
func publicPath(p string) bool {
	return p == "/healthz" || p == "/readyz" || strings.HasPrefix(p, "/v1/certificates/")
}
