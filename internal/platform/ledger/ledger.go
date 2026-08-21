// Package ledger implements the §1.5 TigerBeetle ledger scheme: ledger ids,
// transfer codes, 128-bit account ids (high 64 = namespace, low 64 = entity
// serial), the LedgerClient interface and a dev in-memory implementation with
// TigerBeetle semantics (pending/post/void, DEBITS_MUST_NOT_EXCEED_CREDITS).
// A real core ledger service client is used when LEDGER_URL is set.
package ledger

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// §1.5 ledger ids.
const (
	LedgerAgentFloat      = 100 // agent_float
	LedgerPSMPayments     = 200 // psm_payments
	LedgerVATRemittance   = 300 // vat_remittance
	LedgerPSSPRecon       = 400 // pssp_recon
	LedgerDisputeDeposits = 500 // dispute_deposits
	LedgerT11Attribution  = 600 // t11_attribution
	LedgerCommissions     = 700 // commissions
)

// §1.5 transfer codes.
const (
	CodeAuthorise = 1 // authorise (pending)
	CodeCapture   = 2 // capture (post_pending)
	CodeVoid      = 3 // void
	CodeTopup     = 4 // topup
	CodeSettle    = 5 // settle
	CodeHold      = 6 // hold
	CodeRelease   = 7 // release
)

// Account flags (TigerBeetle semantics).
const (
	FlagDebitsMustNotExceedCredits = 1 << 0
	FlagCreditsMustNotExceedDebits = 1 << 1
)

// AccountID builds a §1.5 128-bit account id: high 64 bits = namespace code,
// low 64 bits = entity serial. Rendered as 32 hex chars.
func AccountID(namespace uint64, serial uint64) string {
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(namespace >> (56 - 8*i))
		b[8+i] = byte(serial >> (56 - 8*i))
	}
	return hex.EncodeToString(b[:])
}

// NewTransferID returns a random 128-bit transfer id (32 hex chars).
func NewTransferID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// DeterministicTransferID derives a stable 128-bit transfer id from a seed
// (e.g. "psm-intent:pay-abc"). Used for idempotent transfer creation: the
// same logical operation always yields the same transfer id, so a retry
// after a crash replays instead of double-posting (TigerBeetle dedup
// semantics on client-supplied ids).
func DeterministicTransferID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// Account is a ledger account. No PII: UserData holds an opaque metadata key;
// the mapping lives in the service's store.
type Account struct {
	ID       string `json:"id"`
	Ledger   uint32 `json:"ledger"`
	Code     uint16 `json:"code"`
	Flags    uint32 `json:"flags"`
	UserData string `json:"user_data,omitempty"`
}

// Balance mirrors TigerBeetle account balance fields (amounts in kobo).
type Balance struct {
	AccountID      string `json:"account_id"`
	Ledger         uint32 `json:"ledger"`
	DebitsPosted   uint64 `json:"debits_posted"`
	CreditsPosted  uint64 `json:"credits_posted"`
	DebitsPending  uint64 `json:"debits_pending"`
	CreditsPending uint64 `json:"credits_pending"`
}

// NetPosted is credits_posted - debits_posted (may be negative conceptually;
// clamped at math level by callers).
func (b Balance) NetPosted() int64 { return int64(b.CreditsPosted) - int64(b.DebitsPosted) }

// Transfer is a double-entry transfer between two accounts (amount in kobo).
type Transfer struct {
	ID              string `json:"id"`
	DebitAccountID  string `json:"debit_account_id"`
	CreditAccountID string `json:"credit_account_id"`
	Ledger          uint32 `json:"ledger"`
	Code            uint16 `json:"code"`
	Amount          uint64 `json:"amount"`
	Pending         bool   `json:"pending"`
	UserData        string `json:"user_data,omitempty"`
}

var (
	ErrAccountExists        = errors.New("ledger: account already exists")
	ErrAccountNotFound      = errors.New("ledger: account not found")
	ErrTransferNotFound     = errors.New("ledger: transfer not found")
	ErrTransferNotPending   = errors.New("ledger: transfer is not pending")
	ErrExceedsCredits       = errors.New("ledger: debits_must_not_exceed_credits violated")
	ErrExceedsDebits        = errors.New("ledger: credits_must_not_exceed_debits violated")
	ErrLedgerMismatch       = errors.New("ledger: accounts on different ledgers")
	ErrAmountZero           = errors.New("ledger: amount must be > 0")
	ErrAmountExceedsPending = errors.New("ledger: amount exceeds pending amount")
	// ErrTransferIDConflict is returned when a client-supplied transfer id
	// already exists with different transfer parameters (a true conflict,
	// not a replay).
	ErrTransferIDConflict = errors.New("ledger: transfer id exists with different parameters")
)

// sameTransfer reports whether an existing transfer matches a requested
// creation (idempotent replay check).
func sameTransfer(existing *Transfer, t Transfer, pending bool) bool {
	return existing.DebitAccountID == t.DebitAccountID &&
		existing.CreditAccountID == t.CreditAccountID &&
		existing.Ledger == t.Ledger &&
		existing.Code == t.Code &&
		existing.Amount == t.Amount &&
		existing.Pending == pending
}

// Client is the §1.5 LedgerClient interface.
type Client interface {
	CreateAccounts(accts []Account) error
	Transfer(t Transfer) (string, error)
	PendingTransfer(t Transfer) (string, error)
	PostPending(pendingID string, amount uint64) (string, error)
	// PostPendingAs posts a pending transfer under a caller-chosen post
	// transfer id. Idempotent: if the pending transfer was already consumed
	// and a posted transfer with postID exists, postID is returned without
	// moving money again. This closes the crash window between the ledger
	// post and the record update: the post id is known (and persisted)
	// before the ledger call.
	PostPendingAs(pendingID, postID string, amount uint64) (string, error)
	VoidPending(pendingID string) (string, error)
	// LookupTransfer returns a transfer by id (pending, posted or void).
	LookupTransfer(id string) (Transfer, error)
	Balance(accountID string) (Balance, error)
}

// DevClient is the embedded in-memory TigerBeetle-semantics implementation.
type DevClient struct {
	mu        sync.Mutex
	accounts  map[string]*Account
	balances  map[string]*Balance
	transfers map[string]*Transfer
}

func NewDevClient() *DevClient {
	return &DevClient{
		accounts:  map[string]*Account{},
		balances:  map[string]*Balance{},
		transfers: map[string]*Transfer{},
	}
}

func (c *DevClient) CreateAccounts(accts []Account) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range accts {
		if a.ID == "" {
			return fmt.Errorf("ledger: account id required")
		}
		if _, ok := c.accounts[a.ID]; ok {
			return ErrAccountExists
		}
	}
	for _, a := range accts {
		cp := a
		c.accounts[a.ID] = &cp
		c.balances[a.ID] = &Balance{AccountID: a.ID, Ledger: a.Ledger}
	}
	return nil
}

func (c *DevClient) checkFlags(a *Account, b *Balance, addDebitPending, addDebitPosted, addCreditPending, addCreditPosted uint64) error {
	if a.Flags&FlagDebitsMustNotExceedCredits != 0 {
		if b.DebitsPosted+addDebitPosted+addDebitPending > b.CreditsPosted+addCreditPosted {
			return ErrExceedsCredits
		}
	}
	if a.Flags&FlagCreditsMustNotExceedDebits != 0 {
		if b.CreditsPosted+addCreditPosted+addCreditPending > b.DebitsPosted+addDebitPosted {
			return ErrExceedsDebits
		}
	}
	return nil
}

func (c *DevClient) pair(t Transfer) (*Account, *Balance, *Account, *Balance, error) {
	if t.Amount == 0 {
		return nil, nil, nil, nil, ErrAmountZero
	}
	da, ok := c.accounts[t.DebitAccountID]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("debit account: %w", ErrAccountNotFound)
	}
	ca, ok := c.accounts[t.CreditAccountID]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("credit account: %w", ErrAccountNotFound)
	}
	if da.Ledger != ca.Ledger {
		return nil, nil, nil, nil, ErrLedgerMismatch
	}
	return da, c.balances[da.ID], ca, c.balances[ca.ID], nil
}

// Transfer posts an immediate (non-pending) double-entry transfer.
func (c *DevClient) Transfer(t Transfer) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	da, db, ca, cb, err := c.pair(t)
	if err != nil {
		return "", err
	}
	if t.ID != "" {
		if existing, ok := c.transfers[t.ID]; ok {
			if sameTransfer(existing, t, false) {
				return t.ID, nil // idempotent replay
			}
			return "", ErrTransferIDConflict
		}
	}
	if err := c.checkFlags(da, db, 0, t.Amount, 0, 0); err != nil {
		return "", err
	}
	if err := c.checkFlags(ca, cb, 0, 0, 0, t.Amount); err != nil {
		return "", err
	}
	db.DebitsPosted += t.Amount
	cb.CreditsPosted += t.Amount
	if t.ID == "" {
		t.ID = NewTransferID()
	}
	t.Pending = false
	cp := t
	c.transfers[t.ID] = &cp
	return t.ID, nil
}

// PendingTransfer creates a pending (authorise/hold) transfer.
func (c *DevClient) PendingTransfer(t Transfer) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	da, db, ca, cb, err := c.pair(t)
	if err != nil {
		return "", err
	}
	if t.ID != "" {
		if existing, ok := c.transfers[t.ID]; ok {
			if sameTransfer(existing, t, true) {
				return t.ID, nil // idempotent replay
			}
			return "", ErrTransferIDConflict
		}
	}
	// TigerBeetle: pending debits count against the flag invariant.
	if err := c.checkFlags(da, db, db.DebitsPending+t.Amount, 0, 0, 0); err != nil {
		return "", err
	}
	if err := c.checkFlags(ca, cb, 0, 0, cb.CreditsPending+t.Amount, 0); err != nil {
		return "", err
	}
	db.DebitsPending += t.Amount
	cb.CreditsPending += t.Amount
	if t.ID == "" {
		t.ID = NewTransferID()
	}
	t.Pending = true
	cp := t
	c.transfers[t.ID] = &cp
	return t.ID, nil
}

// PostPending captures a pending transfer (≤ pending amount supported).
func (c *DevClient) PostPending(pendingID string, amount uint64) (string, error) {
	return c.PostPendingAs(pendingID, "", amount)
}

// PostPendingAs captures a pending transfer under a caller-chosen post id.
// Idempotent replay: when the pending transfer was already consumed and a
// posted transfer with postID exists for the same accounts/amount, postID
// is returned without moving money again.
func (c *DevClient) PostPendingAs(pendingID, postID string, amount uint64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt, ok := c.transfers[pendingID]
	if !ok {
		return "", ErrTransferNotFound
	}
	if !pt.Pending {
		// Idempotent replay: the pending transfer was already consumed; if
		// the expected post transfer exists and matches, report success.
		if postID != "" {
			if post, ok2 := c.transfers[postID]; ok2 && !post.Pending &&
				post.DebitAccountID == pt.DebitAccountID &&
				post.CreditAccountID == pt.CreditAccountID &&
				(amount == 0 || post.Amount == amount) {
				return postID, nil
			}
		}
		return "", ErrTransferNotPending
	}
	if amount == 0 {
		amount = pt.Amount
	}
	if amount > pt.Amount {
		return "", ErrAmountExceedsPending
	}
	da, db := c.accounts[pt.DebitAccountID], c.balances[pt.DebitAccountID]
	ca, cb := c.accounts[pt.CreditAccountID], c.balances[pt.CreditAccountID]
	if err := c.checkFlags(da, db, 0, amount, 0, 0); err != nil {
		return "", err
	}
	if err := c.checkFlags(ca, cb, 0, 0, 0, amount); err != nil {
		return "", err
	}
	db.DebitsPending -= pt.Amount
	cb.CreditsPending -= pt.Amount
	db.DebitsPosted += amount
	cb.CreditsPosted += amount
	pt.Pending = false
	id := postID
	if id == "" {
		id = NewTransferID()
	}
	post := Transfer{
		ID:              id,
		DebitAccountID:  pt.DebitAccountID,
		CreditAccountID: pt.CreditAccountID,
		Ledger:          pt.Ledger,
		Code:            pt.Code, // reuse the pending transfer's code (TB post_pending semantics)
		Amount:          amount,
		Pending:         false,
		UserData:        pt.UserData,
	}
	c.transfers[id] = &post
	if amount < pt.Amount {
		// remainder released (TigerBeetle posts full or voids remainder; we release)
		pt.Amount = 0
	}
	return id, nil
}

// VoidPending releases a pending transfer without posting.
func (c *DevClient) VoidPending(pendingID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt, ok := c.transfers[pendingID]
	if !ok {
		return "", ErrTransferNotFound
	}
	if !pt.Pending {
		return "", ErrTransferNotPending
	}
	db := c.balances[pt.DebitAccountID]
	cb := c.balances[pt.CreditAccountID]
	db.DebitsPending -= pt.Amount
	cb.CreditsPending -= pt.Amount
	pt.Pending = false
	id := NewTransferID()
	void := Transfer{
		ID:              id,
		DebitAccountID:  pt.DebitAccountID,
		CreditAccountID: pt.CreditAccountID,
		Ledger:          pt.Ledger,
		Code:            CodeVoid,
		Amount:          pt.Amount,
		Pending:         false,
		UserData:        pt.UserData,
	}
	c.transfers[id] = &void
	return id, nil
}

// LookupTransfer returns a transfer by id (pending, posted or void).
func (c *DevClient) LookupTransfer(id string) (Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.transfers[id]
	if !ok {
		return Transfer{}, ErrTransferNotFound
	}
	return *t, nil
}

func (c *DevClient) Balance(accountID string) (Balance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.balances[accountID]
	if !ok {
		return Balance{}, ErrAccountNotFound
	}
	return *b, nil
}
