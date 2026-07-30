package main

import (
	"fmt"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
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
	err := s.lc.CreateAccounts([]ledger.Account{{
		ID: id, Ledger: ledger.LedgerAgentFloat, Code: 4, UserData: "nrs-float-treasury",
	}})
	if err != nil && err != ledger.ErrAccountExists {
		return "", err
	}
	return id, nil
}

// Topup funds an agent float from the NRS treasury account (code 4 topup).
func (s *FloatService) Topup(agentID string, amountKobo uint64, reference string) (FloatMovement, error) {
	fa, err := s.Open(agentID)
	if err != nil {
		return FloatMovement{}, err
	}
	treasury, err := s.treasuryAccountID()
	if err != nil {
		return FloatMovement{}, err
	}
	txID, err := s.lc.Transfer(ledger.Transfer{
		DebitAccountID:  treasury,
		CreditAccountID: fa.AccountID,
		Ledger:          ledger.LedgerAgentFloat,
		Code:            ledger.CodeTopup,
		Amount:          amountKobo,
		UserData:        "topup:" + reference,
	})
	if err != nil {
		return FloatMovement{}, err
	}
	mv := FloatMovement{ID: ids.WithPrefix("flm"), AgentID: agentID, Kind: "topup", AmountKobo: amountKobo, Reference: reference, TransferID: txID, CreatedAt: nowRFC3339()}
	if err := s.st.Put("float_movements", mv.ID, mv); err != nil {
		return FloatMovement{}, err
	}
	return mv, nil
}

// Debit draws down agent float (e.g. cash collection remittance). The ledger
// enforces DEBITS_MUST_NOT_EXCEED_CREDITS -> ErrExceedsCredits on overdraft.
func (s *FloatService) Debit(agentID string, amountKobo uint64, reference string) (FloatMovement, error) {
	fa, err := s.Open(agentID)
	if err != nil {
		return FloatMovement{}, err
	}
	treasury, err := s.treasuryAccountID()
	if err != nil {
		return FloatMovement{}, err
	}
	txID, err := s.lc.Transfer(ledger.Transfer{
		DebitAccountID:  fa.AccountID,
		CreditAccountID: treasury,
		Ledger:          ledger.LedgerAgentFloat,
		Code:            ledger.CodeSettle,
		Amount:          amountKobo,
		UserData:        "debit:" + reference,
	})
	if err != nil {
		return FloatMovement{}, err // includes ledger.ErrExceedsCredits
	}
	mv := FloatMovement{ID: ids.WithPrefix("flm"), AgentID: agentID, Kind: "debit", AmountKobo: amountKobo, Reference: reference, TransferID: txID, CreatedAt: nowRFC3339()}
	if err := s.st.Put("float_movements", mv.ID, mv); err != nil {
		return FloatMovement{}, err
	}
	return mv, nil
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
