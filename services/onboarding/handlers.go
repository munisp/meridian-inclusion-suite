package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/otelx"
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
	agents       *AgentRegistry
	hierarchy    *Hierarchy
	commissions  *CommissionEngine
	docs         *DocService
	fsBackend    *fsDocBackend // non-nil only in dev profile
}

func (s *server) routes() *http.ServeMux {
	mux := otelx.NewMux()
	mux.HandleFunc("GET /healthz", httpx.Healthz(serviceName, serviceVersion))
	mux.HandleFunc("GET /readyz", httpx.Readyz(nil))

	mux.HandleFunc("POST /v1/operators", s.createOperator)
	mux.HandleFunc("GET /v1/operators", s.listOperators)
	mux.HandleFunc("GET /v1/operators/{id}", s.getOperator)
	mux.HandleFunc("PATCH /v1/operators/{id}", s.patchOperator)
	mux.HandleFunc("POST /v1/operators/{id}/status", s.transitionStatus)
	mux.HandleFunc("GET /v1/operators/{id}/audit", s.operatorAudit)
	mux.HandleFunc("POST /v1/operators/lookup", s.lookupByNIN)

	mux.HandleFunc("POST /v1/agents", s.registerAgent)
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	mux.HandleFunc("POST /v1/agents/{id}/vetting", s.setAgentVetting)
	mux.HandleFunc("POST /v1/agents/{id}/parent", s.attachSubAgent)
	mux.HandleFunc("GET /v1/agents/{id}/downline", s.agentDownline)
	mux.HandleFunc("POST /v1/commissions/accrue", s.accrueCommission)
	mux.HandleFunc("GET /v1/agents/{id}/commissions", s.agentCommissionRecords)

	mux.HandleFunc("POST /v1/operators/{id}/documents/presign", s.presignDoc)
	mux.HandleFunc("POST /v1/operators/{id}/documents/complete", s.completeDoc)
	mux.HandleFunc("GET /v1/operators/{id}/documents", s.listDocs)
	mux.HandleFunc("GET /v1/onboarding/{id}", s.onboardingStatus)

	mux.HandleFunc("GET /v1/review/queue", s.reviewQueue)
	mux.HandleFunc("POST /v1/review/{id}/approve", s.reviewApprove)
	mux.HandleFunc("POST /v1/review/{id}/reject", s.reviewReject)

	mux.HandleFunc("POST /v1/workflows/runs/{id}/redrive", s.redriveRun)
	if s.fsBackend != nil {
		mux.HandleFunc("PUT /v1/docs/upload/{doc}", s.fsBackend.serveUpload)
	}

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
	return mux.ServeMux
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
	if warn, err := s.agents.ValidateForCapture(in.AgentID); err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_agent", err.Error())
		return
	} else if warn != "" {
		log.Printf("agent validation warning (dev): %s", warn)
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
	// Object-level authz (audit H-5): non-admin callers see only their own
	// records in the listing — never the full PII roster.
	if !httpx.HasRole(r, "admin") {
		own := []Operator{}
		for _, op := range ops {
			if canAccessOperator(r, op) {
				own = append(own, op)
			}
		}
		ops = own
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"operators": ops, "count": len(ops)})
}

// canAccessOperator enforces object-level authz on operator PII records
// (audit H-5): admins may access any record; any other caller may access
// only their own record — identified by the operator ID, phone (MSISDN),
// or the capturing agent ID matching the authenticated identity.
func canAccessOperator(r *http.Request, op Operator) bool {
	if httpx.HasRole(r, "admin") {
		return true
	}
	id := httpx.CallerIdentity(r)
	return id != "" && (op.ID == id || op.Phone == id || op.AgentID == id)
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
	if !canAccessOperator(r, op) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden", "operators may only read their own records")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, op)
}

func (s *server) patchOperator(w http.ResponseWriter, r *http.Request) {
	opVal, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	if !canAccessOperator(r, opVal) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden", "operators may only modify their own records")
		return
	}
	op := &opVal
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
			// O2: lifecycle transitions go through the state machine;
			// illegal transitions are rejected with 409 + no mutation.
			if !ValidOperatorStatus(v) {
				httpx.WriteProblem(w, http.StatusBadRequest, "validation", "invalid status "+v)
				return
			}
			if !CanTransition(op.Status, v) {
				httpx.WriteProblem(w, http.StatusConflict, "illegal_transition",
					fmt.Sprintf("transition %s -> %s is not allowed; see GET /v1/operators/{id}/audit", op.Status, v))
				return
			}
			if err := s.registry.Transition(op, v, "api:"+httpx.RequestIdentity(r)); err != nil {
				httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
				return
			}
			s.publishStatusEvent(op, v)
		}
	}
	if err := s.registry.Update(*op); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, op)
}

// publishStatusEvent emits the lifecycle audit event for a transition.
func (s *server) publishStatusEvent(op *Operator, to string) {
	s.workflows.bus.Publish("nrs.onb.operator.status.v1", events.New("nrs.onb.operator.status.v1", serviceName, "", "", map[string]any{
		"operator_id": op.ID, "to": to, "nin_hash": op.NINHash, "tin_hash": op.TINHash,
	}))
}

// transitionStatus is the explicit lifecycle endpoint (preferred over PATCH).
func (s *server) transitionStatus(w http.ResponseWriter, r *http.Request) {
	op, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	var in struct {
		To     string `json:"to"`
		Reason string `json:"reason,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.To == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "to is required")
		return
	}
	if !ValidOperatorStatus(in.To) {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "invalid status "+in.To)
		return
	}
	if !CanTransition(op.Status, in.To) {
		httpx.WriteProblem(w, http.StatusConflict, "illegal_transition",
			fmt.Sprintf("transition %s -> %s is not allowed", op.Status, in.To))
		return
	}
	if err := s.registry.Transition(&op, in.To, "api:"+httpx.RequestIdentity(r)); err != nil {
		httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
		return
	}
	s.publishStatusEvent(&op, in.To)
	httpx.WriteJSON(w, http.StatusOK, op)
}

// operatorAudit returns the transition audit trail for an operator.
func (s *server) operatorAudit(w http.ResponseWriter, r *http.Request) {
	trail, err := s.registry.StatusAuditTrail(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"audit": trail})
}

// lookupByNIN resolves pseudonymised NIN -> operator status (USSD tin_status
// path; never returns the raw NIN).
func (s *server) lookupByNIN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NIN string `json:"nin"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.NIN == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "nin is required")
		return
	}
	op, found, err := s.registry.FindByNINHash(NINHash(in.NIN))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !found {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"found": true, "operator_id": op.ID, "status": op.Status,
		"tin": op.TIN, "tin_hash": op.TINHash, "review_status": op.ReviewStatus,
	})
}

func (s *server) verifyNIN(w http.ResponseWriter, r *http.Request) {
	// B2 #16: NIN verification against NIMC is restricted to back-office
	// roles with a verified identity (no anonymous/arbitrary-caller lookups).
	if !backOfficeRole(r) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"nin verification requires admin/operator role")
		return
	}
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

// backOfficeRole gates statutory identity/provisioning operations to
// verified back-office roles (B2 #16).
func backOfficeRole(r *http.Request) bool {
	for _, role := range httpx.RequestRoles(r) {
		if role == "admin" || role == "operator" {
			return true
		}
	}
	return false
}

func (s *server) provisionTIN(w http.ResponseWriter, r *http.Request) {
	// B2 #16: TIN provisioning is a back-office operation. The caller must
	// hold a verified admin/operator role; the operator record must exist
	// and the supplied NIN must MATCH the registered NIN hash (ownership —
	// no provisioning a TIN against an arbitrary NIN for an arbitrary
	// operator_id).
	if !backOfficeRole(r) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"tin provisioning requires admin/operator role")
		return
	}
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
	op, found, err := s.registry.Get(in.OperatorID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !found {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	if op.NINHash != NINHash(in.NIN) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"nin does not match the operator's registered identity")
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
	// B2 #16: TIN verification is restricted to back-office roles.
	if !backOfficeRole(r) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"tin verification requires admin/operator role")
		return
	}
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

// --- agent registry handlers (O5) ---

func (s *server) registerAgent(w http.ResponseWriter, r *http.Request) {
	var in Agent
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ag, err := s.agents.Register(in)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, ag)
}

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	ags, err := s.agents.List()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ags == nil {
		ags = []Agent{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agents": ags, "count": len(ags)})
}

func (s *server) getAgent(w http.ResponseWriter, r *http.Request) {
	if s.hierarchy != nil {
		// I6: tenant- + subtree-scoped read (no cross-tenant existence oracle).
		ag, ok := s.hierarchy.visibleAgent(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, ag)
		return
	}
	ag, ok, err := s.agents.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ag)
}

func (s *server) setAgentVetting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"` // approved|suspended|rejected|pending
		Notes  string `json:"notes,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.Status == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "status is required")
		return
	}
	ag, err := s.agents.SetVetting(r.PathValue("id"), in.Status, in.Notes)
	if err != nil {
		httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ag)
}

// --- document handlers (O4) ---

func (s *server) presignDoc(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind     string `json:"kind"`
		Filename string `json:"filename"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	res, err := s.docs.Presign(r.PathValue("id"), in.Kind, in.Filename)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

func (s *server) completeDoc(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DocID     string `json:"doc_id"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.DocID == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "doc_id is required")
		return
	}
	doc, err := s.docs.Complete(r.PathValue("id"), in.DocID, in.SHA256, in.SizeBytes)
	if err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, doc)
}

func (s *server) listDocs(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"documents": s.docs.List(r.PathValue("id"))})
}

// onboardingStatus is the resumption endpoint (O4).
func (s *server) onboardingStatus(w http.ResponseWriter, r *http.Request) {
	op, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.docs.Status(op))
}

// --- review queue / approval workflow (O4) ---

// reviewerRoleAllowed gates approve/reject to back-office roles. B2 #6:
// roles come from the verified JWT (authx-stamped X-Meridian-Roles in
// keycloak mode; dev X-Dev-Role / HS256 roles claim in dev mode via
// httpx.RequestRoles) — a raw client-supplied X-Dev-Role is never read
// here, and read-only auditor cannot approve.
func reviewerRoleAllowed(r *http.Request) bool {
	for _, role := range httpx.RequestRoles(r) {
		if role == "admin" || role == "operator" {
			return true
		}
	}
	return false
}

func (s *server) reviewQueue(w http.ResponseWriter, r *http.Request) {
	ops, err := s.registry.List()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	queue := []Operator{}
	for _, op := range ops {
		if op.Status == "pending_review" || op.ReviewStatus == "pending" {
			queue = append(queue, op)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"queue": queue, "count": len(queue)})
}

func (s *server) reviewDecision(w http.ResponseWriter, r *http.Request, approve bool) {
	if !reviewerRoleAllowed(r) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden", "approve/reject requires admin/operator role")
		return
	}
	op, ok, err := s.registry.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "operator not found")
		return
	}
	reviewer := httpx.CallerIdentity(r)
	// B2 #18 (SoD): the reviewer must not be the agent who captured/created
	// the operator record — no self-approval of one's own onboarding.
	if reviewer != "" && op.AgentID != "" && reviewer == op.AgentID {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"reviewer must differ from the capturing agent (segregation of duties)")
		return
	}
	if approve {
		op.ReviewStatus = "approved"
		if op.Status == "pending_review" {
			// approved -> back to registered so the provision workflow can proceed
			if err := s.registry.Transition(&op, "registered", "review:"+reviewer); err != nil {
				httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
				return
			}
		}
	} else {
		op.ReviewStatus = "rejected"
		if op.Status != "rejected" {
			if err := s.registry.Transition(&op, "rejected", "review:"+reviewer); err != nil {
				httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
				return
			}
		}
	}
	if err := s.registry.Update(op); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	event := "nrs.onb.review.approved.v1"
	if !approve {
		event = "nrs.onb.review.rejected.v1"
	}
	s.workflows.bus.Publish(event, events.New(event, serviceName, "", "", map[string]any{
		"operator_id": op.ID, "reviewer": reviewer,
	}))
	httpx.WriteJSON(w, http.StatusOK, op)
}

func (s *server) reviewApprove(w http.ResponseWriter, r *http.Request) { s.reviewDecision(w, r, true) }
func (s *server) reviewReject(w http.ResponseWriter, r *http.Request)  { s.reviewDecision(w, r, false) }

// redriveRun re-drives a crashed/failed workflow run (O1 resumption).
func (s *server) redriveRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Redrive(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusConflict, "redrive_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

// publicPath marks endpoints that bypass dev auth.
func publicPath(p string) bool {
	return p == "/healthz" || p == "/readyz" || strings.HasPrefix(p, "/v1/certificates/") ||
		strings.HasPrefix(p, "/v1/docs/upload/") // dev FS upload URLs carry their own HMAC signature
}
