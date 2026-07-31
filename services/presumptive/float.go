package main

import (
	"fmt"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// §1.5 namespaces for agent float accounts (ledger 100).
const (
	nsFloatAgent    uint64 = 100000100000 // agent float accounts serial base
	nsFloatTreasury uint64 = 100000000001 // NRS float funding treasury
)

// floatLowThresholdKobo triggers wf-psm-float-monitor alerts (₦5,000).
const floatLowThresholdKobo uint64 = 500000

// FloatService manages agent float on ledger 100 with
// DEBITS_MUST_NOT_EXCEED_CREDITS enforced by the LedgerClient.
type FloatService struct {
	st *store.Store
	lc ledger.Client
}

func NewFloatService(st *store.Store, lc ledger.Client) *FloatService {
	return &FloatService{st: st, lc: lc}
}

func agentSerial(agentID string) uint64 {
	h := uint64(1469598103934665603)
	for i := 0; i < len(agentID); i++ {
		h ^= uint64(agentID[i])
		h *= 1099511628211
	}
	return h & 0x0000FFFFFFFFFFFF
}

// Open creates (or returns) the agent float account.
func (s *FloatService) Open(agentID string) (FloatAccount, error) {
	var existing []FloatAccount
	if err := s.st.List("float_accounts", &existing); err != nil {
		return FloatAccount{}, err
	}
	for _, fa := range existing {
		if fa.AgentID == agentID {
			return fa, nil
		}
	}
	serial := agentSerial(agentID)
	acctID := ledger.AccountID(nsFloatAgent, serial)
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID:     acctID,
		Ledger: ledger.LedgerAgentFloat,
		Code:   4,
		Flags:  ledger.FlagDebitsMustNotExceedCredits, // float can never go negative
		// No PII on the ledger: metadata key only.
		UserData: "agent-float:" + fmt.Sprint(serial),
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return FloatAccount{}, err
	}
	fa := FloatAccount{AgentID: agentID, AccountID: acctID, Serial: serial, Currency: "NGN", CreatedAt: nowRFC3339()}
	if err := s.st.Put("float_accounts", agentID, fa); err != nil {
		return FloatAccount{}, err
	}
	return fa, nil
}

func (s *FloatService) treasuryAccountID() (string, error) {
	id := ledger.AccountID(nsFloatTreasury, 1)
	if _, err := s.lc.Balance(id); err == nil {
		return id, nil
	}
	// Treasury is debited on every top-up: enforce funding control
	// (DEBITS_MUST_NOT_EXCEED_CREDITS) so float creation can never push the
	// treasury account negative (audit Flow 4c).
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerAgentFloat, Code: 4,
		Flags: ledger.FlagDebitsMustNotExceedCredits, UserData: "nrs-float-treasury",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// movementID is the deterministic dedup key for a float movement: one
// logical movement per (kind, reference), forever.
func movementID(kind, reference string) string {
	sum := fmt.Sprintf("flm-%s-%s", kind, reference)
	h := uint64(1469598103934665603)
	for i := 0; i < len(sum); i++ {
		h ^= uint64(sum[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("flm-%x", h)
}

// findByReference returns the existing movement for (kind, reference), if
// any — this is the 200-replay path for retried top-ups/debits.
func (s *FloatService) findByReference(kind, reference string) (FloatMovement, bool) {
	if reference == "" {
		return FloatMovement{}, false
	}
	var mv FloatMovement
	ok, err := s.st.Get("float_movements", movementID(kind, reference), &mv)
	if err != nil || !ok {
		return FloatMovement{}, false
	}
	return mv, true
}

// move runs one float movement as a saga:
//
//	1. dedup on reference (replay returns the existing movement, 200)
//	2. pending transfer (deterministic id, ledger-dedup safe)
//	3. persist the movement record (status=pending, transfer ids)
//	4. post the pending transfer (idempotent PostPendingAs)
//	5. mark the movement posted
//
// Failure at step 4 voids the pending transfer (compensation) and marks the
// movement voided. A crash anywhere leaves a durable record the recovery
// path (SweepFloatMovements) can finish or void.
func (s *FloatService) move(kind string, agentID string, amountKobo uint64, reference string) (FloatMovement, error) {
	if reference == "" {
		return FloatMovement{}, fmt.Errorf("reference is required (idempotency)")
	}
	if mv, ok := s.findByReference(kind, reference); ok {
		return mv, nil // idempotent replay
	}
	fa, err := s.Open(agentID)
	if err != nil {
		return FloatMovement{}, err
	}
	treasury, err := s.treasuryAccountID()
	if err != nil {
		return FloatMovement{}, err
	}
	debit, credit, code := treasury, fa.AccountID, uint16(ledger.CodeTopup)
	if kind == "debit" {
		debit, credit, code = fa.AccountID, treasury, ledger.CodeSettle
	}
	mv := FloatMovement{
		ID: movementID(kind, reference), AgentID: agentID, Kind: kind,
		AmountKobo: amountKobo, Reference: reference,
		PendingTransferID: ledger.DeterministicTransferID("float-pending:" + kind + ":" + reference),
		TransferID:        ledger.DeterministicTransferID("float-post:" + kind + ":" + reference),
		Status:            "pending", CreatedAt: nowRFC3339(),
	}
	if _, err := s.lc.PendingTransfer(ledger.Transfer{
		ID: mv.PendingTransferID, DebitAccountID: debit, CreditAccountID: credit,
		Ledger: ledger.LedgerAgentFloat, Code: code, Amount: amountKobo,
		UserData: kind + ":" + reference,
	}); err != nil {
		return FloatMovement{}, err // includes ledger.ErrExceedsCredits
	}
	if err := s.st.Put("float_movements", mv.ID, mv); err != nil {
		// compensation: release the hold
		_, _ = s.lc.VoidPending(mv.PendingTransferID)
		return FloatMovement{}, err
	}
	if _, err := s.lc.PostPendingAs(mv.PendingTransferID, mv.TransferID, amountKobo); err != nil {
		// compensation: void the hold, never leave a silent pending
		_, _ = s.lc.VoidPending(mv.PendingTransferID)
		mv.Status = "voided"
		mv.FailReason = "post pending: " + err.Error()
		_ = s.st.Put("float_movements", mv.ID, mv)
		return FloatMovement{}, err
	}
	mv.Status = "posted"
	if err := s.st.Put("float_movements", mv.ID, mv); err != nil {
		return FloatMovement{}, err
	}
	return mv, nil
}

// SweepFloatMovements finishes or voids movements stuck in "pending" after a
// crash between the ledger hold and the post. Called by the recovery worker.
func (s *FloatService) SweepFloatMovements() (finished, voided int, err error) {
	var all []FloatMovement
	if err := s.st.List("float_movements", &all); err != nil {
		return 0, 0, err
	}
	for _, mv := range all {
		if mv.Status != "pending" {
			continue
		}
		if _, err := s.lc.PostPendingAs(mv.PendingTransferID, mv.TransferID, mv.AmountKobo); err != nil {
			if _, verr := s.lc.VoidPending(mv.PendingTransferID); verr == nil {
				mv.Status = "voided"
				mv.FailReason = "recovery: post failed: " + err.Error()
				_ = s.st.Put("float_movements", mv.ID, mv)
				voided++
			}
			continue
		}
		mv.Status = "posted"
		if err := s.st.Put("float_movements", mv.ID, mv); err == nil {
			finished++
		}
	}
	return finished, voided, nil
}

// Topup funds an agent float from the NRS treasury account (code 4 topup).
// Idempotent per reference: retried top-ups replay, never double-credit.
// Executed as a pending->post saga with void compensation (see move).
func (s *FloatService) Topup(agentID string, amountKobo uint64, reference string) (FloatMovement, error) {
	return s.move("topup", agentID, amountKobo, reference)
}

// Debit draws down agent float (e.g. cash collection remittance). The ledger
// enforces DEBITS_MUST_NOT_EXCEED_CREDITS -> ErrExceedsCredits on overdraft.
// Idempotent per reference; pending->post saga with void compensation.
func (s *FloatService) Debit(agentID string, amountKobo uint64, reference string) (FloatMovement, error) {
	return s.move("debit", agentID, amountKobo, reference)
}

// Balance returns the agent float ledger balance.
func (s *FloatService) Balance(agentID string) (ledger.Balance, error) {
	fa, err := s.Open(agentID)
	if err != nil {
		return ledger.Balance{}, err
	}
	return s.lc.Balance(fa.AccountID)
}

// Movements lists the audit trail for an agent.
func (s *FloatService) Movements(agentID string) ([]FloatMovement, error) {
	var all []FloatMovement
	if err := s.st.List("float_movements", &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, m := range all {
		if m.AgentID == agentID {
			out = append(out, m)
		}
	}
	return out, nil
}
