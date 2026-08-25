package main

// commission_engine.go — I6 rule-pack-driven commission engine.
//
// The commission table (bps per upline level) comes from a rule pack in the
// SPEC 1.4 format: an honestly-tagged embedded fallback copy of
// rp-commissions-ng (canonical YAML in the meridian-rule-packs repo), with
// the version selected by COMMISSION_PACK_VERSION (fail-closed when the env
// asks for a version the service does not carry).
//
// Money math is kobo-integer only (amount_kobo * rate_bps / 10000) — no
// floats anywhere. Each computed commission is posted to the core ledger as a
// hold -> post saga (ledger 700, code 6 hold) from the NRS commissions pool
// into the per-agent commission payable account, via the existing ledger
// client, in post -> mark order per platform convention: the durable
// commission record is written ONLY after a successful post, and a replay is
// honoured only against a posted record (replay-only-if-posted). Durable
// idempotency: the record is keyed by (reference, level) with a payload hash
// binding and a 7-day TTL — the same reference replayed with a different
// payload surfaces a conflict, never a silent replay.

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

//go:embed packs/rp-commissions-ng.json
var commissionPackJSON []byte

// commissionPack is the JSON mirror of the SPEC 1.4 rp-commissions-ng pack.
type commissionPack struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Rules       struct {
		Currency string `json:"currency"`
		Unit     string `json:"unit"`
		Levels   []struct {
			Level   int    `json:"level"`
			RateBPS uint64 `json:"rate_bps"`
			Narrate string `json:"narrate"`
		} `json:"levels"`
	} `json:"rules"`
}

// commissionRecordTTL bounds how long a durable commission record stays
// authoritative for idempotent replay (mirrors the payout-marker TTL).
const commissionRecordTTL = 7 * 24 * time.Hour

// ErrCommissionConflict is returned when an idempotency key is replayed with
// a different payload (payload-hash binding violation).
var ErrCommissionConflict = errors.New("commission reference replayed with a different payload: manual reconciliation required")

// CommissionRecord is the durable per-(reference, level) commission record.
type CommissionRecord struct {
	ID              string `json:"id"` // cm_ deterministic from reference+level
	Reference       string `json:"reference"`
	Level           int    `json:"level"`
	AgentID         string `json:"agent_id"`
	TenantID        string `json:"tenant_id"`
	RateBPS         uint64 `json:"rate_bps"`
	BaseKobo        uint64 `json:"base_kobo"`   // settled transaction amount the bps applies to
	AmountKobo      uint64 `json:"amount_kobo"` // computed commission (kobo integer)
	AccountID       string `json:"account_id"`  // agent commission payable account
	HoldTransferID  string `json:"hold_transfer_id"`
	PostTransferID  string `json:"post_transfer_id,omitempty"`
	Status          string `json:"status"` // posted
	PayloadHash     string `json:"payload_hash"`
	RulePackVersion string `json:"rule_pack_version"`
	CreatedAt       string `json:"created_at"`
	ExpiresAt       string `json:"expires_at"`
}

// CommissionEngine computes and posts hierarchy commissions.
type CommissionEngine struct {
	st        *store.Store
	hierarchy *Hierarchy
	ledger    ledger.Client
	pack      commissionPack
	rates     map[int]uint64 // level -> bps
}

// LoadCommissionEngine builds the engine from the embedded fallback pack,
// honouring COMMISSION_PACK_VERSION (fail-closed on an unknown version).
func LoadCommissionEngine(st *store.Store, h *Hierarchy, lc ledger.Client) (*CommissionEngine, error) {
	var p commissionPack
	if err := json.Unmarshal(commissionPackJSON, &p); err != nil {
		return nil, fmt.Errorf("embedded commission pack: %w", err)
	}
	if want := os.Getenv("COMMISSION_PACK_VERSION"); want != "" && want != p.Version {
		return nil, fmt.Errorf("COMMISSION_PACK_VERSION=%s not carried by this build (embedded fallback %s@%s); refusing to compute commissions against an unverified table",
			want, p.ID, p.Version)
	}
	e := &CommissionEngine{st: st, hierarchy: h, ledger: lc, pack: p, rates: map[int]uint64{}}
	for _, l := range p.Rules.Levels {
		if l.Level < 1 || l.Level > maxAgentDepth {
			return nil, fmt.Errorf("commission pack %s@%s: level %d outside hierarchy depth cap", p.ID, p.Version, l.Level)
		}
		e.rates[l.Level] = l.RateBPS
	}
	if len(e.rates) == 0 {
		return nil, fmt.Errorf("commission pack %s@%s carries no level rates", p.ID, p.Version)
	}
	return e, nil
}

// PackVersion identifies the table in effect ("rp-commissions-ng@1.0.0").
func (e *CommissionEngine) PackVersion() string { return e.pack.ID + "@" + e.pack.Version }

// computeKobo applies a bps rate to a kobo amount with integer math only.
func computeKobo(baseKobo, rateBPS uint64) uint64 { return baseKobo * rateBPS / 10000 }

// recordKey is the durable idempotency key: (reference, level).
func recordKey(reference string, level int) string { return fmt.Sprintf("%s:%d", reference, level) }

// payloadHash binds the idempotency key to the full computation payload.
func payloadHash(agentID, reference string, level int, rateBPS, baseKobo, amountKobo uint64, packVersion string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%d|%d|%s",
		agentID, reference, level, rateBPS, baseKobo, amountKobo, packVersion)))
	return hex.EncodeToString(sum[:])
}

// commissionRecordExpired reports whether the record's replay window lapsed.
func commissionRecordExpired(rec CommissionRecord, now time.Time) bool {
	if exp, err := time.Parse(time.RFC3339, rec.ExpiresAt); err == nil {
		return now.After(exp)
	}
	if ts, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil {
		return now.Sub(ts) > commissionRecordTTL
	}
	return false
}

// liveRecord returns the unexpired record for (reference, level), if any.
func (e *CommissionEngine) liveRecord(reference string, level int) (CommissionRecord, bool, error) {
	var rec CommissionRecord
	ok, err := e.st.Get("commission_records", recordKey(reference, level), &rec)
	if err != nil || !ok {
		return CommissionRecord{}, false, err
	}
	if commissionRecordExpired(rec, time.Now()) {
		return CommissionRecord{}, false, nil
	}
	return rec, true, nil
}

// commissionAccountID is the deterministic per-agent commission payable
// account on ledger 700 (same serial scheme as the settlement workflow).
func commissionAccountID(agentID string) string {
	return ledger.AccountID(nsAgentCommission, hashSerial(agentID))
}

func (e *CommissionEngine) ensureAccounts(agentID string) (poolID, acctID string, err error) {
	poolID = ledger.AccountID(nsCommissionsPool, 1)
	if _, berr := e.ledger.Balance(poolID); berr != nil {
		if cerr := e.ledger.CreateAccounts([]ledger.Account{{
			ID: poolID, Ledger: ledger.LedgerCommissions, Code: 5, UserData: "nrs-commissions-pool",
		}}); cerr != nil && !errors.Is(cerr, ledger.ErrAccountExists) {
			return "", "", cerr
		}
	}
	acctID = commissionAccountID(agentID)
	if _, berr := e.ledger.Balance(acctID); berr != nil {
		if cerr := e.ledger.CreateAccounts([]ledger.Account{{
			ID: acctID, Ledger: ledger.LedgerCommissions, Code: 5, UserData: "agent:" + agentID,
		}}); cerr != nil && !errors.Is(cerr, ledger.ErrAccountExists) {
			return "", "", cerr
		}
	}
	return poolID, acctID, nil
}

// Accrue computes and posts commissions for one settled transaction:
// baseKobo at the pack bps for the capturing agent (level 1) and each upline
// ancestor (levels 2..depth cap). Returns the posted records. Fully
// idempotent on reference: a replay with the same payload returns the posted
// records without moving money again; a replay with a different payload is a
// conflict (ErrCommissionConflict).
func (e *CommissionEngine) Accrue(agentID, reference string, baseKobo uint64) ([]CommissionRecord, error) {
	if agentID == "" || reference == "" {
		return nil, fmt.Errorf("agent_id and reference are required")
	}
	if baseKobo == 0 {
		return nil, fmt.Errorf("amount_kobo must be > 0")
	}
	agent, ok, err := e.hierarchy.agents.Get(agentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentID)
	}
	// Earners: level 1 = the agent itself, then upline ancestors.
	type earner struct {
		level int
		agent Agent
	}
	earners := []earner{{level: 1, agent: agent}}
	ancestors, err := e.hierarchy.Ancestors(agentID)
	if err != nil {
		return nil, err
	}
	for i, anc := range ancestors {
		earners = append(earners, earner{level: i + 2, agent: anc})
	}
	// Fund the pool idempotently for this reference (treasury offset, code 4
	// topup) before any holds — deterministic transfer id, so a replay never
	// double-funds.
	var total uint64
	for _, en := range earners {
		if bps, ok := e.rates[en.level]; ok {
			total += computeKobo(baseKobo, bps)
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("commission pack %s computes zero for amount %d kobo", e.PackVersion(), baseKobo)
	}
	treasuryID := ledger.AccountID(nsCommissionsPool, 2)
	if _, err := e.ledger.Balance(treasuryID); err != nil {
		if cerr := e.ledger.CreateAccounts([]ledger.Account{{
			ID: treasuryID, Ledger: ledger.LedgerCommissions, Code: 4, UserData: "nrs-treasury-offset",
		}}); cerr != nil && !errors.Is(cerr, ledger.ErrAccountExists) {
			return nil, cerr
		}
	}
	poolID, _, err := e.ensureAccounts(agentID)
	if err != nil {
		return nil, err
	}
	if _, err := e.ledger.Transfer(ledger.Transfer{
		ID:             ledger.DeterministicTransferID("comm-accrue-fund:" + reference + ":" + fmt.Sprint(total)),
		DebitAccountID: treasuryID, CreditAccountID: poolID, Ledger: ledger.LedgerCommissions,
		Code: ledger.CodeTopup, Amount: total, UserData: "commission-funding:" + reference,
	}); err != nil {
		return nil, fmt.Errorf("pool funding: %w", err)
	}

	out := make([]CommissionRecord, 0, len(earners))
	for _, en := range earners {
		bps, ok := e.rates[en.level]
		if !ok {
			continue // pack carries no rate at this level
		}
		rec, err := e.accrueOne(en.agent, reference, en.level, bps, baseKobo)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// accrueOne runs the hold -> post -> mark saga for one (reference, level).
// Order per platform convention: the durable record is written ONLY after a
// successful post; replay is honoured only for a posted record whose post
// transfer still verifies in the ledger (replay-only-if-posted).
func (e *CommissionEngine) accrueOne(agent Agent, reference string, level int, bps, baseKobo uint64) (CommissionRecord, error) {
	amount := computeKobo(baseKobo, bps)
	hash := payloadHash(agent.ID, reference, level, bps, baseKobo, amount, e.PackVersion())

	if existing, ok, err := e.liveRecord(reference, level); err != nil {
		return CommissionRecord{}, err
	} else if ok {
		if existing.PayloadHash != hash {
			return CommissionRecord{}, ErrCommissionConflict
		}
		if existing.Status == "posted" && existing.PostTransferID != "" {
			if _, lerr := e.ledger.LookupTransfer(existing.PostTransferID); lerr == nil {
				return existing, nil // replay-only-if-posted: verified post
			}
		}
		// Record exists but the post cannot be verified — fall through and
		// re-run the saga under the same deterministic ids (idempotent).
	}

	acctID := commissionAccountID(agent.ID)
	poolID, _, err := e.ensureAccounts(agent.ID)
	if err != nil {
		return CommissionRecord{}, err
	}
	holdID := ledger.DeterministicTransferID(fmt.Sprintf("comm-hold:%s:%d", reference, level))
	postID := ledger.DeterministicTransferID(fmt.Sprintf("comm-post:%s:%d", reference, level))
	posted := false
	if _, err := e.ledger.PendingTransfer(ledger.Transfer{
		ID: holdID, DebitAccountID: poolID, CreditAccountID: acctID, Ledger: ledger.LedgerCommissions,
		Code: ledger.CodeHold, Amount: amount,
		UserData: fmt.Sprintf("commission:%s:%s:L%d", reference, agent.ID, level),
	}); err != nil {
		if !errors.Is(err, ledger.ErrTransferIDConflict) {
			return CommissionRecord{}, fmt.Errorf("commission hold: %w", err)
		}
		// Deterministic hold id already consumed: if the post landed (crash
		// between post and mark), skip to the mark; otherwise this reference
		// collides with a voided/different hold — a true conflict.
		if _, lerr := e.ledger.LookupTransfer(postID); lerr == nil {
			posted = true
		} else {
			return CommissionRecord{}, fmt.Errorf("commission hold id conflict for %s:L%d: %w", reference, level, err)
		}
	}
	if !posted {
		if _, err := e.ledger.PostPendingAs(holdID, postID, amount); err != nil {
			_, _ = e.ledger.VoidPending(holdID) // compensation
			// belt-and-braces: never leave a posted record for an unposted hold
			_, _ = e.st.Delete("commission_records", recordKey(reference, level))
			return CommissionRecord{}, fmt.Errorf("commission post: %w", err)
		}
	}
	rec := CommissionRecord{
		ID:        "cm_" + ledger.DeterministicTransferID(recordKey(reference, level))[:24],
		Reference: reference, Level: level, AgentID: agent.ID, TenantID: agent.TenantID,
		RateBPS: bps, BaseKobo: baseKobo, AmountKobo: amount, AccountID: acctID,
		HoldTransferID: holdID, PostTransferID: postID, Status: "posted",
		PayloadHash: hash, RulePackVersion: e.PackVersion(),
		CreatedAt: nowRFC3339(),
		ExpiresAt: time.Now().Add(commissionRecordTTL).UTC().Format(time.RFC3339),
	}
	// Post -> mark: the durable record is written only after the post landed.
	if err := e.st.Put("commission_records", recordKey(reference, level), rec); err != nil {
		// Post landed under a deterministic id; a replay re-runs the saga
		// idempotently and re-attempts the mark — no double-post.
		return CommissionRecord{}, fmt.Errorf("commission mark: %w", err)
	}
	return rec, nil
}

// RecordsFor returns the unexpired commission records earned by agentID
// (optionally the whole subtree via includeDownline), newest first.
func (e *CommissionEngine) RecordsFor(agentID string, includeDownline bool) ([]CommissionRecord, error) {
	ids := map[string]bool{agentID: true}
	if includeDownline {
		var err error
		ids, err = e.hierarchy.subtreeIDs(agentID)
		if err != nil {
			return nil, err
		}
	}
	var all []CommissionRecord
	if err := e.st.List("commission_records", &all); err != nil {
		return nil, err
	}
	now := time.Now()
	out := []CommissionRecord{}
	for _, rec := range all {
		if ids[rec.AgentID] && !commissionRecordExpired(rec, now) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// accrueCommission handles POST /v1/commissions/accrue. Back-office roles
// (admin/operator) within the agent's tenant only — commission computation is
// never caller-influenced beyond the settled amount + reference.
func (s *server) accrueCommission(w http.ResponseWriter, r *http.Request) {
	if !backOfficeRole(r) {
		httpx.WriteProblem(w, http.StatusForbidden, "forbidden",
			"commission accrual requires admin/operator role")
		return
	}
	var in struct {
		AgentID    string `json:"agent_id"`
		Reference  string `json:"reference"`
		AmountKobo uint64 `json:"amount_kobo"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if in.AgentID == "" || in.Reference == "" || in.AmountKobo == 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation",
			"agent_id, reference and amount_kobo (> 0) are required")
		return
	}
	ag, ok, err := s.agents.Get(in.AgentID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !ok || (requestTenant(r) != DefaultTenant && ag.TenantID != requestTenant(r)) {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	recs, err := s.commissions.Accrue(in.AgentID, in.Reference, in.AmountKobo)
	if err != nil {
		if errors.Is(err, ErrCommissionConflict) {
			httpx.WriteProblem(w, http.StatusConflict, "payload_conflict", err.Error())
			return
		}
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "accrual_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"records": recs, "count": len(recs), "rule_pack_version": s.commissions.PackVersion(),
	})
}

// agentCommissionRecords handles GET /v1/agents/{id}/commissions — the
// agent's posted commission records (?downline=true includes the subtree).
// Tenant- and subtree-scoped via the hierarchy authz.
func (s *server) agentCommissionRecords(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.hierarchy.visibleAgent(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	recs, err := s.commissions.RecordsFor(ag.ID, r.URL.Query().Get("downline") == "true")
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"records": recs, "count": len(recs), "rule_pack_version": s.commissions.PackVersion(),
	})
}
