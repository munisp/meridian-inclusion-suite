package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// nip_recon.go — NIP payout service: mandatory name-enquiry gate,
// idempotent transfers, reversal (NOT refund) handling, the TSQ
// reconciliation sweeper, and the HTTP surface.
//
// The payout and refund paths in this service ALWAYS pass through the
// name-enquiry-before-transfer gate (NIP operational requirement). The gate
// is controlled by config flag NIP_NAME_ENQUIRY_REQUIRED (default "true";
// only set "false" in dev). Reconciliation follows the existing recovery
// sweeper pattern: transfers left in_flight are swept and resolved via
// TransactionStatusQuery (TSQ); a TSQ result of failed triggers the CBN
// auto-reversal path (see reversal-vs-refund note in nip.go).

// NIPTransfer is the durable record of one payout/refund instruction.
type NIPTransfer struct {
	ID             string `json:"id"` // nip_<ulid>
	SessionID      string `json:"session_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Purpose        string `json:"purpose"` // payout|refund
	AmountKobo     uint64 `json:"amount_kobo"`
	DestAccount    string `json:"dest_account"`
	DestBankCode   string `json:"dest_bank_code"`
	DestName       string `json:"dest_name"` // as verified by mandatory name enquiry
	NameEnquiryID  string `json:"name_enquiry_session_id"`
	Narration      string `json:"narration"`
	Status         string `json:"status"` // success|failed|in_flight|reversed
	Detail         string `json:"detail,omitempty"`
	// RequestHash binds the idempotency key to the exact request payload;
	// a retry with the same key but a different payload is rejected.
	RequestHash string `json:"request_hash,omitempty"`
	// ExpiresAt bounds the idempotency replay window (default 7 days,
	// assurance R4 item 2). After expiry a reused key is treated as new;
	// the TSQ sweeper purges expired records only in terminal state.
	ExpiresAt      string `json:"expires_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// nipIdempotencyTTL is the default idempotency replay window for NIP
// payout/refund keys (financial safety margin over the 24h PSM intent).
const nipIdempotencyTTL = 7 * 24 * time.Hour

// nipTransferExpired reports whether t's idempotency window has closed.
func nipTransferExpired(t NIPTransfer, now time.Time) bool {
	exp := t.ExpiresAt
	if exp == "" {
		if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil {
			exp = ts.Add(nipIdempotencyTTL).Format(time.RFC3339)
		}
	}
	if exp == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339, exp)
	return err == nil && now.After(deadline)
}

// NIPService runs outbound transfers over the NIP rail with the mandatory
// name-enquiry gate and TSQ reconciliation.
type NIPService struct {
	rail               NIPRail
	st                 *store.Store
	bus                events.Bus
	requireNameEnquiry bool
	mu                 sync.Mutex // serializes payout idempotency check+put
}

// NewNIPService wires the service. requireNameEnquiry defaults to true when
// built via NewNIPServiceFromEnv.
func NewNIPService(rail NIPRail, st *store.Store, bus events.Bus, requireNameEnquiry bool) *NIPService {
	return &NIPService{rail: rail, st: st, bus: bus, requireNameEnquiry: requireNameEnquiry}
}

// NewNIPServiceFromEnv builds the rail (fail-closed) and service from env.
// NIP_NAME_ENQUIRY_REQUIRED=false disables the gate (dev only).
func NewNIPServiceFromEnv(st *store.Store, bus events.Bus) (*NIPService, error) {
	rail, err := NewNIPRailFromEnv()
	if err != nil {
		return nil, err
	}
	require := !strings.EqualFold(os.Getenv("NIP_NAME_ENQUIRY_REQUIRED"), "false")
	if !require {
		log.Printf("component=nip warn: name-enquiry gate DISABLED (dev only)")
	}
	return NewNIPService(rail, st, bus, require), nil
}

func (s *NIPService) publish(t *NIPTransfer) {
	if s.bus == nil {
		return
	}
	_ = s.bus.Publish("nrs.psm.nip.v1", events.New("nrs.psm.nip.v1", serviceName, "", "", map[string]any{
		"transfer_id": t.ID, "session_id": t.SessionID, "status": t.Status,
		"amount_kobo": t.AmountKobo, "purpose": t.Purpose,
	}))
}

// PayoutRequest starts an outbound transfer (payout or refund leg).
type PayoutRequest struct {
	Purpose        string `json:"purpose"` // payout|refund
	AmountKobo     uint64 `json:"amount_kobo"`
	DestAccount    string `json:"dest_account"`
	DestBankCode   string `json:"dest_bank_code"`
	Narration      string `json:"narration"`
	IdempotencyKey string `json:"idempotency_key"` // required: caller-stable retry key
}

// Payout executes a NIP funds transfer with the MANDATORY
// name-enquiry-before-transfer hook. Idempotent on IdempotencyKey: a retry
// returns the original transfer record with its original session id.
func (s *NIPService) Payout(req PayoutRequest) (NIPTransfer, error) {
	if req.AmountKobo == 0 || req.DestAccount == "" || req.DestBankCode == "" {
		return NIPTransfer{}, fmt.Errorf("amount_kobo, dest_account and dest_bank_code are required")
	}
	if req.IdempotencyKey == "" {
		return NIPTransfer{}, fmt.Errorf("idempotency_key is required (NIP transfers must be safely retryable)")
	}
	if req.Purpose == "" {
		req.Purpose = "payout"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reqHash := payoutRequestHash(req)
	// idempotent retry: same key -> same record (and same session id on the
	// rail). An in_flight record is returned as-is (the TSQ sweeper resolves
	// it); the client must NOT trigger a second rail dispatch.
	var existing NIPTransfer
	if ok, _ := s.st.Get("nip_transfers", "idem:"+req.IdempotencyKey, &existing); ok {
		if nipTransferExpired(existing, time.Now()) {
			// expired key: treated as new — fall through to a fresh transfer
			// (the old record is overwritten on Put below).
		} else {
			if existing.RequestHash != "" && existing.RequestHash != reqHash {
				return NIPTransfer{}, fmt.Errorf("idempotency_key %q already used with a different request", tail4(req.IdempotencyKey))
			}
			return existing, nil
		}
	}

	t := NIPTransfer{
		ID:             ids.WithPrefix("nip"),
		IdempotencyKey: req.IdempotencyKey,
		Purpose:        req.Purpose,
		AmountKobo:     req.AmountKobo,
		DestAccount:    req.DestAccount,
		DestBankCode:   req.DestBankCode,
		Narration:      req.Narration,
		ExpiresAt:      time.Now().Add(nipIdempotencyTTL).UTC().Format(time.RFC3339),
		CreatedAt:      nowRFC3339(),
		UpdatedAt:      nowRFC3339(),
	}

	// MANDATORY name-enquiry-before-transfer gate.
	if s.requireNameEnquiry {
		ne, err := s.rail.NameEnquiry(req.DestAccount, req.DestBankCode)
		if err != nil {
			return NIPTransfer{}, fmt.Errorf("name enquiry failed (transfer blocked): %w", err)
		}
		if !ne.Verified {
			return NIPTransfer{}, fmt.Errorf("name enquiry could not verify account ...%s at bank %s: transfer blocked", tail4(req.DestAccount), req.DestBankCode)
		}
		t.DestName = ne.AccountName
		t.NameEnquiryID = ne.SessionID
	}

	t.SessionID = NewNIPSessionID()
	t.RequestHash = reqHash
	// Durable in_flight record BEFORE the rail dispatch, in the same
	// critical section as the idempotency check (audit funds-flow #1): a
	// crash after dispatch leaves a record the TSQ sweeper adopts and
	// reconciles, and a client retry with the same idempotency key returns
	// this record (same session id) instead of sending a second transfer.
	t.Status = NIPStatusInFlight
	t.Detail = "dispatched: awaiting rail result"
	if err := s.st.Put("nip_transfers", "idem:"+t.IdempotencyKey, t); err != nil {
		return NIPTransfer{}, fmt.Errorf("persist pre-dispatch record: %w", err)
	}
	if err := s.st.Put("nip_transfers", t.SessionID, t); err != nil {
		return NIPTransfer{}, fmt.Errorf("persist pre-dispatch record: %w", err)
	}
	res, err := s.rail.FundsTransfer(NIPTransferRequest{
		SessionID:      t.SessionID,
		AmountKobo:     t.AmountKobo,
		DestAccount:    t.DestAccount,
		DestBankCode:   t.DestBankCode,
		DestName:       t.DestName,
		Narration:      t.Narration,
		IdempotencyKey: t.IdempotencyKey,
	})
	if err != nil {
		// Transport-level failure: the rail may or may not have received the
		// transfer. Leave the durable record in_flight — the TSQ sweeper
		// resolves it (success or failed -> auto-reversal). Never blind-fail.
		t.Detail = "dispatch uncertain: " + err.Error()
		t.UpdatedAt = nowRFC3339()
		s.put(t)
		return t, nil
	}
	t.Status = res.Status
	t.Detail = res.Detail
	t.UpdatedAt = nowRFC3339()
	if err := s.st.Put("nip_transfers", "idem:"+t.IdempotencyKey, t); err != nil {
		return t, fmt.Errorf("update transfer record: %w", err)
	}
	if err := s.st.Put("nip_transfers", t.SessionID, t); err != nil {
		return t, fmt.Errorf("update transfer record: %w", err)
	}
	s.publish(&t)
	return t, nil
}

// payoutRequestHash binds an idempotency key to the payload it first
// carried, so a key reused with different parameters is rejected instead of
// silently returning an unrelated transfer.
func payoutRequestHash(req PayoutRequest) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		req.Purpose, req.IdempotencyKey, fmt.Sprint(req.AmountKobo), req.DestAccount, req.DestBankCode, req.Narration,
	}, "|")))
	return fmt.Sprintf("%x", sum[:16])
}

// Refund sends value back for a SUCCESSFUL prior transfer. Per CBN rules a
// refund is a NEW transfer (commercial return of value); Reversal() below is
// only for failed/errored rail transactions. The name-enquiry gate applies.
func (s *NIPService) Refund(req PayoutRequest) (NIPTransfer, error) {
	req.Purpose = "refund"
	return s.Payout(req)
}

// Reversal unwinds a failed or in-flight-resolved-failed transfer at the
// rail. Distinct from Refund: see the CBN regulatory note in nip.go.
func (s *NIPService) Reversal(sessionID, reason string) (NIPTransfer, error) {
	t, ok, err := s.getBySession(sessionID)
	if err != nil || !ok {
		return NIPTransfer{}, fmt.Errorf("nip transfer %s not found", sessionID)
	}
	if t.Status == NIPStatusReversed {
		return t, nil // idempotent
	}
	if t.Status == NIPStatusSuccess {
		return NIPTransfer{}, fmt.Errorf("transfer %s settled successfully: use the refund flow, reversal is for failed/errored transactions", sessionID)
	}
	res, err := s.rail.Reversal(sessionID, reason)
	if err != nil {
		return NIPTransfer{}, fmt.Errorf("reversal: %w", err)
	}
	t.Status = res.Status
	t.Detail = res.Detail
	t.UpdatedAt = nowRFC3339()
	s.put(t)
	s.publish(&t)
	return t, nil
}

func (s *NIPService) getBySession(sessionID string) (NIPTransfer, bool, error) {
	var t NIPTransfer
	ok, err := s.st.Get("nip_transfers", sessionID, &t)
	return t, ok, err
}

// put updates both the session-id and idempotency-key records.
func (s *NIPService) put(t NIPTransfer) {
	_ = s.st.Put("nip_transfers", t.SessionID, t)
	_ = s.st.Put("nip_transfers", "idem:"+t.IdempotencyKey, t)
}

// SweepTSQ resolves all in-flight transfers via TransactionStatusQuery —
// the reconciliation sweeper, following the existing recovery sweeper
// pattern (crash-safe: in-flight state is durable before the rail call).
// A TSQ result of failed triggers the CBN auto-reversal path. Returns the
// number of transfers resolved.
func (s *NIPService) SweepTSQ() (int, error) {
	var all []NIPTransfer
	if err := s.st.List("nip_transfers", &all); err != nil {
		return 0, err
	}
	resolved := 0
	seen := map[string]bool{}
	for _, t := range all {
		if t.Status != NIPStatusInFlight || seen[t.SessionID] {
			continue
		}
		seen[t.SessionID] = true
		res, err := s.rail.TransactionStatusQuery(t.SessionID)
		if err != nil {
			log.Printf("nip tsq sweeper: %s: %v (leaving in_flight)", t.SessionID, err)
			continue
		}
		t.Detail = "tsq: " + res.Detail
		switch res.Status {
		case NIPStatusSuccess:
			t.Status = NIPStatusSuccess
			t.UpdatedAt = nowRFC3339()
			s.put(t)
			s.publish(&t)
			resolved++
		case NIPStatusFailed:
			// CBN failed-transaction auto-reversal path
			if _, err := s.Reversal(t.SessionID, "tsq-resolved-failed (auto-reversal)"); err != nil {
				log.Printf("nip tsq sweeper: auto-reversal %s: %v", t.SessionID, err)
				continue
			}
			resolved++
		default:
			// still in_flight on the rail: leave for the next sweep
		}
	}
	return resolved, nil
}

// PurgeExpiredIdempotency removes expired idempotency records
// ("idem:"+key entries) whose transfer reached a terminal state
// (success|failed|reversed). Expired in_flight records are retained so the
// TSQ sweeper can still resolve them and a late retry still dedupes.
// Returns the number of records purged.
func (s *NIPService) PurgeExpiredIdempotency() (int, error) {
	var all []NIPTransfer
	if err := s.st.List("nip_transfers", &all); err != nil {
		return 0, err
	}
	now := time.Now()
	purged := 0
	for _, t := range all {
		if t.IdempotencyKey == "" || !nipTransferExpired(t, now) {
			continue
		}
		switch t.Status {
		case NIPStatusSuccess, NIPStatusFailed, NIPStatusReversed:
		default:
			continue // in_flight: retained for TSQ resolution
		}
		if _, err := s.st.Delete("nip_transfers", "idem:"+t.IdempotencyKey); err != nil {
			return purged, err
		}
		purged++
	}
	return purged, nil
}

// StartTSQSweeper runs SweepTSQ on an interval until stop is closed (wired
// from main alongside the other recovery loops).
func (s *NIPService) StartTSQSweeper(interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if n, err := s.SweepTSQ(); err != nil {
					log.Printf("nip tsq sweeper: %v", err)
				} else if n > 0 {
					log.Printf("nip tsq sweeper: resolved %d in-flight transfer(s)", n)
				}
				if n, err := s.PurgeExpiredIdempotency(); err != nil {
					log.Printf("nip idempotency purge: %v", err)
				} else if n > 0 {
					log.Printf("nip idempotency purge: removed %d expired terminal record(s)", n)
				}
			}
		}
	}()
}

// ---- HTTP surface (minimal hook; mounted from handlers.go routes()) ----

// nipSweepInterval reads NIP_TSQ_SWEEP_SECONDS (default 60s).
func nipSweepInterval() time.Duration {
	if v := os.Getenv("NIP_TSQ_SWEEP_SECONDS"); v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Second
}

// nipHTTP bundles the NIP handlers.
type nipHTTP struct{ svc *NIPService }

func (h nipHTTP) nameEnquiry(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account  string `json:"account"`
		BankCode string `json:"bank_code"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	res, err := h.svc.rail.NameEnquiry(in.Account, in.BankCode)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadGateway, "rail_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h nipHTTP) payout(w http.ResponseWriter, r *http.Request) {
	var in PayoutRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	t, err := h.svc.Payout(in)
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "payout_blocked", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h nipHTTP) refund(w http.ResponseWriter, r *http.Request) {
	var in PayoutRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	t, err := h.svc.Refund(in)
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "refund_blocked", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h nipHTTP) reversal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"session_id"`
		Reason    string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	t, err := h.svc.Reversal(in.SessionID, in.Reason)
	if err != nil {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "reversal_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h nipHTTP) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok, err := h.svc.getBySession(r.PathValue("session"))
	if err != nil || !ok {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "nip transfer not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h nipHTTP) sweep(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.SweepTSQ()
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "sweep_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"resolved": n})
}
