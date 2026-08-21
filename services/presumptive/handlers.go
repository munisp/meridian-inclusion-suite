package main

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
)

type server struct {
	pay     *PaymentService
	float   *FloatService
	engine  *BandEngine
	gates   *GateClient
	certs   *CertificateService
	wf      *PSMWorkflows
	limiter *RateLimiter
	devices *DeviceService
	bus     events.Bus
	pssps   *PSSPRegistry
	nip     *NIPService // nil until first NIP route use (lazy, env-fail-closed)
	nipErr  error
	nipOnce sync.Once
	// guard is the PSSP webhook replay guard (audit funds-flow #5):
	// X-PSSP-Timestamp within ±5 min + signature-as-nonce replay cache.
	// Nil-safe: when nil the check is skipped (unit tests with bare servers).
	guard *webhookguard.Guard
}

// checkWebhookReplay applies the webhook replay guard when configured:
// replays dedup to 409; stale/malformed/missing (prod) timestamps 401.
func (s *server) checkWebhookReplay(w http.ResponseWriter, r *http.Request) bool {
	if s.guard == nil {
		return true
	}
	err := s.guard.Check(r)
	if err == nil {
		return true
	}
	if errors.Is(err, webhookguard.ErrReplay) {
		httpx.WriteProblem(w, http.StatusConflict, "replay", "duplicate webhook delivery (signature already seen)")
		return false
	}
	httpx.WriteProblem(w, http.StatusUnauthorized, "unauthorized", err.Error())
	return false
}

// nipRoutes lazily builds the NIP rail service (fail-closed: if the live
// rail is misconfigured/unreachable, NIP endpoints return 503 rather than
// silently degrading to the simulator) and mounts the NIP surface. Minimal
// additive hook — no changes to the payments/disputes core paths.
func (s *server) nipRoutes(mux *http.ServeMux) {
	init := func() (*NIPService, error) {
		s.nipOnce.Do(func() {
			// reuse the payment service's durable store for transfer records
			s.nip, s.nipErr = NewNIPServiceFromEnv(s.pay.st, s.bus)
			if s.nipErr == nil {
				s.nip.StartTSQSweeper(nipSweepInterval(), make(chan struct{}))
			}
		})
		return s.nip, s.nipErr
	}
	guard := func(fn func(nipHTTP, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			svc, err := init()
			if err != nil {
				httpx.WriteProblem(w, http.StatusServiceUnavailable, "nip_unavailable", err.Error())
				return
			}
			fn(nipHTTP{svc: svc}, w, r)
		}
	}
	mux.HandleFunc("POST /v1/nip/name-enquiry", guard(nipHTTP.nameEnquiry))
	mux.HandleFunc("POST /v1/nip/payout", guard(nipHTTP.payout))
	mux.HandleFunc("POST /v1/nip/refund", guard(nipHTTP.refund))
	mux.HandleFunc("POST /v1/nip/reversal", guard(nipHTTP.reversal))
	mux.HandleFunc("GET /v1/nip/transfers/{session}", guard(nipHTTP.getTransfer))
	mux.HandleFunc("POST /v1/nip/sweep", guard(nipHTTP.sweep))
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.Healthz(serviceName, serviceVersion))
	mux.HandleFunc("GET /readyz", httpx.Readyz(nil))

	// band engine
	mux.HandleFunc("POST /v1/bands/evaluate", s.evaluateBand)
	mux.HandleFunc("GET /v1/packs", s.listPacks)

	// payments
	mux.HandleFunc("POST /v1/payments/intent", s.createIntent)
	mux.HandleFunc("GET /v1/payments", s.listPayments)
	mux.HandleFunc("GET /v1/payments/{id}", s.getPayment)
	mux.HandleFunc("POST /v1/payments/{id}/authorise", s.authorisePayment)
	mux.HandleFunc("POST /v1/payments/{id}/capture", s.capturePayment)
	mux.HandleFunc("POST /v1/payments/{id}/void", s.voidPayment)

	// PSSP webhooks (public but per-PSSP HMAC-signed)
	mux.HandleFunc("POST /v1/pssp/webhook/{provider}", s.psspWebhook)

	// PSSP registry (O6 onboarding)
	mux.HandleFunc("POST /v1/pssps", s.onboardPSSP)
	mux.HandleFunc("GET /v1/pssps", s.listPSSPs)
	mux.HandleFunc("GET /v1/pssps/{id}", s.getPSSP)
	mux.HandleFunc("POST /v1/pssps/{id}/rotate-secret", s.rotatePSSPSecret)
	mux.HandleFunc("POST /v1/pssps/{id}/status", s.setPSSPStatus)

	// certificates (public verify, rate-limited)
	mux.HandleFunc("GET /v1/certificates/verify/{serial}", s.verifyCertificate)

	// device key enrolment + offline receipt verification (audit fix #6)
	mux.HandleFunc("POST /v1/devices/enroll", s.enrollDevice)
	mux.HandleFunc("POST /v1/receipts/verify", s.verifyReceipt)

	// agent float
	mux.HandleFunc("POST /v1/float/accounts", s.openFloat)
	mux.HandleFunc("POST /v1/float/topup", s.topupFloat)
	mux.HandleFunc("POST /v1/float/debit", s.debitFloat)
	mux.HandleFunc("GET /v1/float/{agent}/balance", s.floatBalance)
	mux.HandleFunc("GET /v1/float/{agent}/movements", s.floatMovements)
	mux.HandleFunc("GET /v1/float/{agent}/risk", s.floatRisk)

	// gates
	mux.HandleFunc("GET /v1/gates", s.listGates)
	mux.HandleFunc("POST /v1/gates/{id}/flip", s.flipGate)

	// workflows
	mux.HandleFunc("GET /v1/workflows", s.listWorkflows)
	mux.HandleFunc("POST /v1/workflows/{name}/trigger", s.triggerWorkflow)
	mux.HandleFunc("GET /v1/workflows/runs", s.listRuns)
	mux.HandleFunc("GET /v1/simulations", s.listSimulations)

	// NIP rail (N1): name enquiry, payout/refund, reversal, TSQ sweep
	s.nipRoutes(mux)
	return mux
}

func (s *server) evaluateBand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		State              string `json:"state"`
		TradeCategory      string `json:"trade_category"`
		AnnualTurnoverKobo uint64 `json:"annual_turnover_kobo"`
		IsCompany          bool   `json:"is_company"`
		FixedAssetsKobo    uint64 `json:"fixed_assets_kobo"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	res := s.engine.Evaluate(in.State, in.TradeCategory, in.AnnualTurnoverKobo, in.IsCompany, in.FixedAssetsKobo)
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *server) listPacks(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"packs": s.engine.Packs()})
}

func (s *server) createIntent(w http.ResponseWriter, r *http.Request) {
	var in IntentRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	p, err := s.pay.CreateIntent(in)
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrGateClosed {
			status = http.StatusForbidden
		}
		if errors.Is(err, ErrIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpx.WriteProblem(w, status, "intent_rejected", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// listPayments is paginated (QA-08): the funds-path list endpoint must
// never return an unbounded result set. limit defaults to 50 and is
// clamped to maxListPaymentsLimit (500); offset defaults to 0.
const (
	defaultListPaymentsLimit = 50
	maxListPaymentsLimit     = 500
)

func (s *server) listPayments(w http.ResponseWriter, r *http.Request) {
	limit, offset := listPaymentsPage(r)
	ps, err := s.pay.List()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ps == nil {
		ps = []Payment{}
	}
	total := len(ps)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := ps[offset:end]
	if page == nil {
		page = []Payment{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"payments": page,
		"count":    len(page),
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// listPaymentsPage parses ?limit=&offset= with a default of 50 and a hard
// max of 500; invalid values fall back to the defaults (fail-safe small).
func listPaymentsPage(r *http.Request) (limit, offset int) {
	limit = defaultListPaymentsLimit
	offset = 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxListPaymentsLimit {
		limit = maxListPaymentsLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func (s *server) getPayment(w http.ResponseWriter, r *http.Request) {
	p, ok, err := s.pay.get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "payment not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *server) authorisePayment(w http.ResponseWriter, r *http.Request) {
	p, res, err := s.pay.Authorise(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "authorise_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"payment": p, "authorisation": res})
}

func (s *server) capturePayment(w http.ResponseWriter, r *http.Request) {
	p, cert, err := s.pay.Capture(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "capture_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"payment": p, "certificate": cert})
}

func (s *server) voidPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.pay.Void(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "void_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *server) psspWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// G3: the signature scheme is per-PSSP (hmac-sha256 generic, hmac-sha512
	// Paystack, verif-hash Flutterwave). HMAC schemes use X-PSSP-Signature;
	// the Flutterwave scheme uses the verif-hash header.
	provider := r.PathValue("provider")
	sig := r.Header.Get("X-PSSP-Signature")
	if sig == "" {
		sig = r.Header.Get("verif-hash")
	}
	if !s.pay.hub.VerifyWebhookSignatureFor(provider, sig, body) {
		httpx.WriteProblem(w, http.StatusUnauthorized, "bad_signature", "webhook signature validation failed for provider scheme")
		return
	}
	// Replay guard (audit funds-flow #5): fresh X-PSSP-Timestamp + the
	// (verified) signature as the replay nonce, so a captured valid
	// signature cannot be replayed outside the tolerance window and a
	// redelivery within it dedups to 409. Canonicalise the nonce header
	// for the Flutterwave verif-hash scheme.
	if sig != "" && r.Header.Get("X-PSSP-Signature") == "" {
		r.Header.Set("X-PSSP-Signature", sig)
	}
	if !s.checkWebhookReplay(w, r) {
		return
	}
	var payload WebhookPayload
	if err := jsonUnmarshal(body, &payload); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	p, err := s.pay.HandleWebhook(provider, payload)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, ErrWebhookMismatch) {
			status = http.StatusConflict // amount/currency mismatch: 409, no state change (G1)
		}
		httpx.WriteProblem(w, status, "webhook_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// --- PSSP registry handlers (O6) ---

func (s *server) onboardPSSP(w http.ResponseWriter, r *http.Request) {
	var in OnboardRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	res, err := s.pssps.Onboard(in)
	if err != nil {
		httpx.WriteProblem(w, http.StatusConflict, "onboard_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

func (s *server) listPSSPs(w http.ResponseWriter, r *http.Request) {
	out, err := s.pssps.List()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pssps": out, "count": len(out)})
}

func (s *server) getPSSP(w http.ResponseWriter, r *http.Request) {
	v, ok, err := s.pssps.Get(r.PathValue("id"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "PSSP not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *server) rotatePSSPSecret(w http.ResponseWriter, r *http.Request) {
	res, err := s.pssps.RotateSecret(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *server) setPSSPStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.Status == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "status is required")
		return
	}
	v, err := s.pssps.SetStatus(r.PathValue("id"), in.Status)
	if err != nil {
		httpx.WriteProblem(w, http.StatusConflict, "illegal_transition", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *server) verifyCertificate(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		clientIP = strings.Split(fwd, ",")[0]
	}
	if !s.limiter.Allow(clientIP) {
		httpx.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "certificate verification is rate-limited; retry shortly")
		return
	}
	cert, valid, err := s.certs.Verify(r.PathValue("serial"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if cert.Serial == "" {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"serial": r.PathValue("serial"), "valid": false, "detail": "certificate not found"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"serial": cert.Serial, "valid": valid, "certificate": cert,
	})
}

func (s *server) openFloat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.AgentID == "" {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "agent_id is required")
		return
	}
	fa, err := s.float.Open(in.AgentID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "ledger_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, fa)
}

func (s *server) topupFloat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID    string `json:"agent_id"`
		AmountKobo uint64 `json:"amount_kobo"`
		Reference  string `json:"reference"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.AgentID == "" || in.AmountKobo == 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "agent_id and amount_kobo are required")
		return
	}
	mv, err := s.float.Topup(in.AgentID, in.AmountKobo, in.Reference)
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "topup_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, mv)
}

func (s *server) debitFloat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID    string `json:"agent_id"`
		AmountKobo uint64 `json:"amount_kobo"`
		Reference  string `json:"reference"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil || in.AgentID == "" || in.AmountKobo == 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "agent_id and amount_kobo are required")
		return
	}
	mv, err := s.float.Debit(in.AgentID, in.AmountKobo, in.Reference)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err == ledger.ErrExceedsCredits {
			status = http.StatusConflict
		}
		httpx.WriteProblem(w, status, "debit_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, mv)
}

func (s *server) floatBalance(w http.ResponseWriter, r *http.Request) {
	bal, err := s.float.Balance(r.PathValue("agent"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "ledger_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bal)
}

func (s *server) floatMovements(w http.ResponseWriter, r *http.Request) {
	mv, err := s.float.Movements(r.PathValue("agent"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if mv == nil {
		mv = []FloatMovement{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"movements": mv})
}

func (s *server) listGates(w http.ResponseWriter, r *http.Request) {
	gates, err := s.gates.Gates()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "gate_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"gates": gates})
}

func (s *server) flipGate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Open bool `json:"open"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	gs, err := s.gates.Flip(r.PathValue("id"), in.Open)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "gate_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, gs)
}

func (s *server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"workflows": s.wf.Names()})
}

func (s *server) triggerWorkflow(w http.ResponseWriter, r *http.Request) {
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
	run, err := s.wf.Run(r.PathValue("name"), input)
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
	var runs []PSMWorkflowRun
	if err := s.wf.st.List("workflow_runs", &runs); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if runs == nil {
		runs = []PSMWorkflowRun{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *server) listSimulations(w http.ResponseWriter, r *http.Request) {
	var sims []Simulation
	if err := s.wf.st.List("simulations", &sims); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if sims == nil {
		sims = []Simulation{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"simulations": sims})
}

// publicPath: health endpoints + PSSP webhooks + public certificate verify.
func publicPath(p string) bool {
	return p == "/healthz" || p == "/readyz" ||
		strings.HasPrefix(p, "/v1/pssp/webhook/") ||
		strings.HasPrefix(p, "/v1/certificates/verify/")
}
