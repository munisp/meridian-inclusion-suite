package main

import (
	"fmt"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
)

// Operator lifecycle state machine (audit O3): the wf-onb-* workflows drive
// transitions; PATCH /v1/operators/{id} may request a transition but illegal
// ones are rejected with 409 and every applied transition emits an audit
// record (status_audit collection) so the lifecycle is reconstructable.
//
//	registered      -> nin_verified | pending_review | rejected
//	pending_review  -> registered | nin_verified | tin_provisioned | rejected
//	nin_verified    -> tin_provisioned | pending_review
//	tin_provisioned -> graduated | pending_review
//	rejected        -> registered            (re-onboarding)
//	graduated       -> (terminal)
var operatorTransitions = map[string][]string{
	"registered":      {"nin_verified", "pending_review", "rejected"},
	"pending_review":  {"registered", "nin_verified", "tin_provisioned", "rejected"},
	"nin_verified":    {"tin_provisioned", "pending_review"},
	"tin_provisioned": {"graduated", "pending_review"},
	"rejected":        {"registered"},
	"graduated":       {},
}

// OperatorStatuses is the full status enum (validation).
var OperatorStatuses = []string{"registered", "pending_review", "nin_verified", "tin_provisioned", "graduated", "rejected"}

// ValidOperatorStatus reports whether s is a known status.
func ValidOperatorStatus(s string) bool {
	for _, v := range OperatorStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// CanTransition reports whether from->to is an allowed transition
// (same-state is allowed: idempotent re-application).
func CanTransition(from, to string) bool {
	if from == to {
		return true
	}
	for _, t := range operatorTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// StatusAudit is the audit record emitted per applied transition.
type StatusAudit struct {
	ID         string `json:"id"`
	OperatorID string `json:"operator_id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Actor      string `json:"actor"` // workflow name, api caller identity, or ussd gateway
	At         string `json:"at"`
}

// Transition applies a lifecycle transition to op: validates against the
// transition table, persists the operator and appends a status_audit record.
// Same-state transitions are idempotent no-ops (no audit record).
func (r *Registry) Transition(op *Operator, to, actor string) error {
	if !ValidOperatorStatus(to) {
		return fmt.Errorf("invalid status %q", to)
	}
	from := op.Status
	if from == "" {
		from = "registered"
	}
	if from == to {
		op.Status = to
		return r.Update(*op)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("illegal transition %s -> %s", from, to)
	}
	op.Status = to
	if err := r.Update(*op); err != nil {
		return err
	}
	aud := StatusAudit{
		ID: ids.WithPrefix("aud"), OperatorID: op.ID, From: from, To: to, Actor: actor, At: nowRFC3339(),
	}
	return r.st.Put("status_audit", aud.ID, aud)
}

// StatusAuditTrail returns the transition history for an operator.
func (r *Registry) StatusAuditTrail(operatorID string) ([]StatusAudit, error) {
	var all []StatusAudit
	if err := r.st.List("status_audit", &all); err != nil {
		return nil, err
	}
	out := []StatusAudit{}
	for _, a := range all {
		if a.OperatorID == operatorID {
			out = append(out, a)
		}
	}
	return out, nil
}
