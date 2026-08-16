package main

// commissions_storefault_test.go — §6.3 cells for the commission (COM)
// flow (assurance R7): same-key-different-payload (amount binding on the
// payout marker) and db timeout/deadlock on the marker state write.

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

var errSimDBTimeout = errorString("simulated db timeout on state write")
var errSimDeadlock = errorString("simulated db deadlock (SQLSTATE 40P01)")

type errorString string

func (e errorString) Error() string { return string(e) }

func newCommissionStack(t *testing.T) (*store.Store, *Registry, ledger.Client, *Workflows) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(st)
	lc := ledger.NewDevClient()
	wf := NewWorkflows(st, reg, NIMCSimulator{}, LocalTINProvisioner{}, NewConsentService(st), lc, events.NewInprocBus())
	return st, reg, lc, wf
}

func provisionOp(t *testing.T, reg *Registry, wf *Workflows, nin, agentID string) {
	t.Helper()
	op := Operator{NINHash: NINHash(nin), FullName: "Op " + nin, AgentID: agentID, CapturedAt: nowRFC3339()}
	if err := reg.Create(&op); err != nil {
		t.Fatal(err)
	}
	if run := wf.TINProvision(op.ID, nin); run.Status != "completed" {
		t.Fatalf("provision %s: %s", nin, run.Error)
	}
}

// same (agent, period) key with a DIFFERENT computed amount must surface a
// conflict (manual reconciliation), never silently claim settlement.
func TestCommissionPayoutAmountConflict(t *testing.T) {
	_, reg, lc, wf := newCommissionStack(t)
	provisionOp(t, reg, wf, "11111111111", "agent-c1")
	run1 := wf.CommissionSettlementForPeriod("2026-09")
	if run1.Status != "completed" {
		t.Fatalf("run1: %s", run1.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-c1"))
	bal1, _ := lc.Balance(acctID)
	if bal1.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("paid once: %+v", bal1)
	}
	// a second verified operator for the same agent changes the computed
	// amount for the SAME period key
	provisionOp(t, reg, wf, "22222222222", "agent-c1")
	run2 := wf.CommissionSettlementForPeriod("2026-09")
	if run2.Status != "completed" {
		t.Fatalf("run2: %s", run2.Error)
	}
	res, ok := run2.Result.(map[string]any)
	if !ok {
		t.Fatalf("run2 result: %#v", run2.Result)
	}
	conflicts, _ := res["conflicts"].([]map[string]any)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 amount conflict, got %#v", res)
	}
	if conflicts[0]["agent_id"] != "agent-c1" {
		t.Fatalf("conflict: %#v", conflicts[0])
	}
	// no double pay: the agent's balance is still the original amount
	bal2, _ := lc.Balance(acctID)
	if bal2.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("conflict must not double-pay: %+v", bal2)
	}
}

// db timeout on the marker write (state write after the ledger post): the
// run fails, and a re-run after recovery replays the deterministic post
// idempotently and writes the marker — exactly one payout.
func TestCommissionPayoutMarkerWriteDBTimeout(t *testing.T) {
	st, reg, lc, wf := newCommissionStack(t)
	provisionOp(t, reg, wf, "33333333333", "agent-c2")
	var armed atomic.Bool
	armed.Store(true)
	st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" && coll == "commission_payouts" {
			return errSimDBTimeout
		}
		return nil
	})
	run1 := wf.CommissionSettlementForPeriod("2026-09")
	if run1.Status != "failed" || !strings.Contains(run1.Error, "mark payout") {
		t.Fatalf("run1 must fail at the marker write: %+v", run1)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-c2"))
	bal1, _ := lc.Balance(acctID)
	if bal1.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("post landed once: %+v", bal1)
	}
	// recover and re-run: replay posts idempotently, marker written
	armed.Store(false)
	run2 := wf.CommissionSettlementForPeriod("2026-09")
	if run2.Status != "completed" {
		t.Fatalf("run2: %s", run2.Error)
	}
	bal2, _ := lc.Balance(acctID)
	if bal2.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("must not double-pay: %+v", bal2)
	}
	var markers []CommissionPayout
	if err := st.List("commission_payouts", &markers); err != nil || len(markers) != 1 {
		t.Fatalf("markers: %v %v", markers, err)
	}
}

// db deadlock on the marker write: same convergence guarantee.
func TestCommissionPayoutMarkerWriteDeadlock(t *testing.T) {
	st, reg, lc, wf := newCommissionStack(t)
	provisionOp(t, reg, wf, "44444444444", "agent-c3")
	var armed atomic.Bool
	armed.Store(true)
	st.SetFaultHook(func(op, coll, id string) error {
		if armed.Load() && op == "put" && coll == "commission_payouts" {
			return errSimDeadlock
		}
		return nil
	})
	run1 := wf.CommissionSettlementForPeriod("2026-09")
	if run1.Status != "failed" {
		t.Fatalf("run1 must fail: %+v", run1)
	}
	armed.Store(false)
	run2 := wf.CommissionSettlementForPeriod("2026-09")
	if run2.Status != "completed" {
		t.Fatalf("run2: %s", run2.Error)
	}
	acctID := ledger.AccountID(nsAgentCommission, hashSerial("agent-c3"))
	bal, _ := lc.Balance(acctID)
	if bal.CreditsPosted != commissionPerVerifiedKobo {
		t.Fatalf("must not double-pay: %+v", bal)
	}
}
