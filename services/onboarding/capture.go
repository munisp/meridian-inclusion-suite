package main

import (
	"fmt"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

// Registry is the operator registry (CRUD) backed by the embedded store.
type Registry struct {
	st     *store.Store
	serial uint64
}

func NewRegistry(st *store.Store) *Registry { return &Registry{st: st, serial: 1000} }

func (r *Registry) nextSerial() uint64 {
	r.serial++
	return r.serial
}

func (r *Registry) Create(op *Operator) error {
	if op.ID == "" {
		op.ID = ids.WithPrefix("op")
	}
	if op.Serial == 0 {
		op.Serial = r.nextSerial()
	}
	if op.Status == "" {
		op.Status = "registered"
	}
	now := nowRFC3339()
	if op.CreatedAt == "" {
		op.CreatedAt = now
	}
	op.UpdatedAt = now
	return r.st.Put("operators", op.ID, *op)
}

func (r *Registry) Get(id string) (Operator, bool, error) {
	var op Operator
	ok, err := r.st.Get("operators", id, &op)
	return op, ok, err
}

func (r *Registry) Update(op Operator) error {
	op.UpdatedAt = nowRFC3339()
	return r.st.Put("operators", op.ID, op)
}

func (r *Registry) List() ([]Operator, error) {
	var ops []Operator
	if err := r.st.List("operators", &ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// FindByNINHash returns the operator with the given pseudonymised NIN, if any.
func (r *Registry) FindByNINHash(ninHash string) (Operator, bool, error) {
	ops, err := r.List()
	if err != nil {
		return Operator{}, false, err
	}
	for _, op := range ops {
		if op.NINHash == ninHash {
			return op, true, nil
		}
	}
	return Operator{}, false, nil
}

// FindByClientRef finds an operator previously ingested from an agent's
// client-side reference (idempotent re-sync).
func (r *Registry) FindByClientRef(agentID, clientRef string) (Operator, bool, error) {
	ops, err := r.List()
	if err != nil {
		return Operator{}, false, err
	}
	for _, op := range ops {
		if op.ClientRef == clientRef && op.AgentID == agentID {
			return op, true, nil
		}
	}
	return Operator{}, false, nil
}

// CaptureService implements the offline-first batch ingest API (SPEC §4 T5):
// idempotency keys + conflict resolution + ≥72h offline tolerance.
type CaptureService struct {
	st       *store.Store
	registry *Registry
	verifier NINVerifier
}

func NewCaptureService(st *store.Store, reg *Registry, v NINVerifier) *CaptureService {
	return &CaptureService{st: st, registry: reg, verifier: v}
}

// maxOfflineAge documents the ≥72h offline tolerance design: batches captured
// longer ago than this are still accepted but flagged for review.
const maxOfflineAge = 72 * time.Hour

// Ingest processes a batch. Idempotency: the same Idempotency-Key returns the
// stored prior result (status "duplicate"). Per item:
//   - same (agent, client_ref) seen before  -> duplicate_client_ref
//   - same nin_hash exists                  -> conflict resolved by captured_at
//     (last-writer-wins on field values; identity fields never downgraded)
//   - otherwise                             -> created
func (c *CaptureService) Ingest(agentID, idemKey string, items []CaptureItem) (CaptureBatch, error) {
	if idemKey == "" {
		return CaptureBatch{}, fmt.Errorf("Idempotency-Key header is required")
	}
	if len(items) == 0 {
		return CaptureBatch{}, fmt.Errorf("batch must contain at least one item")
	}
	if len(items) > 500 {
		return CaptureBatch{}, fmt.Errorf("batch too large (max 500 items)")
	}
	// idempotency replay
	var prior CaptureBatch
	ok, err := c.st.Get("capture_batches", idemKey, &prior)
	if err != nil {
		return CaptureBatch{}, err
	}
	if ok {
		prior.Status = "duplicate"
		return prior, nil
	}

	batch := CaptureBatch{
		ID:             ids.WithPrefix("bat"),
		IdempotencyKey: idemKey,
		AgentID:        agentID,
		Items:          items,
		Status:         "processed",
		CreatedAt:      nowRFC3339(),
	}
	now := time.Now().UTC()
	for _, it := range items {
		res := CaptureItemResult{ClientRef: it.ClientRef}
		capturedAt, perr := time.Parse(time.RFC3339, it.CapturedAt)
		if perr != nil {
			res.Outcome = "rejected"
			res.Detail = "captured_at must be RFC3339"
			batch.Results = append(batch.Results, res)
			continue
		}
		age := now.Sub(capturedAt)
		res.OfflineAgeHours = int(age.Hours())
		if it.NIN == "" || it.FullName == "" {
			res.Outcome = "rejected"
			res.Detail = "nin and full_name are required"
			batch.Results = append(batch.Results, res)
			continue
		}
		ninHash := NINHash(it.NIN)

		// 1) client_ref idempotency (agent retried the same record)
		if it.ClientRef != "" {
			if existing, found, err := c.registry.FindByClientRef(agentID, it.ClientRef); err != nil {
				return CaptureBatch{}, err
			} else if found {
				res.Outcome = "duplicate_client_ref"
				res.OperatorID = existing.ID
				res.Detail = "record already synced by this agent; skipped"
				batch.Results = append(batch.Results, res)
				continue
			}
		}

		// 2) identity conflict: same NIN already registered
		if existing, found, err := c.registry.FindByNINHash(ninHash); err != nil {
			return CaptureBatch{}, err
		} else if found {
			// last-writer-wins by captured_at; never downgrade verification status
			existingCaptured, _ := time.Parse(time.RFC3339, existing.CapturedAt)
			if capturedAt.After(existingCaptured) {
				existing.FullName = it.FullName
				existing.Phone = firstNonEmpty(it.Phone, existing.Phone)
				existing.State = firstNonEmpty(it.State, existing.State)
				existing.LGA = firstNonEmpty(it.LGA, existing.LGA)
				existing.TradeCategory = firstNonEmpty(it.TradeCategory, existing.TradeCategory)
				existing.CapturedAt = it.CapturedAt
				existing.SyncedAt = nowRFC3339()
				if err := c.registry.Update(existing); err != nil {
					return CaptureBatch{}, err
				}
				res.Detail = "conflict: newer captured_at wins; profile fields updated"
			} else {
				res.Detail = "conflict: existing record is newer or equal; kept as-is"
			}
			res.Outcome = "conflict_resolved"
			res.OperatorID = existing.ID
			batch.Results = append(batch.Results, res)
			continue
		}

		// 3) create new operator
		op := Operator{
			NINHash:       ninHash,
			FullName:      it.FullName,
			Phone:         it.Phone,
			State:         it.State,
			LGA:           it.LGA,
			TradeCategory: it.TradeCategory,
			AgentID:       agentID,
			ConsentID:     it.ConsentID,
			ClientRef:     it.ClientRef,
			CapturedAt:    it.CapturedAt,
			SyncedAt:      nowRFC3339(),
		}
		if age > maxOfflineAge {
			op.Status = "registered" // flagged via result detail; review queue
		}
		if err := c.registry.Create(&op); err != nil {
			return CaptureBatch{}, err
		}
		res.Outcome = "created"
		res.OperatorID = op.ID
		if age > maxOfflineAge {
			res.Detail = fmt.Sprintf("offline age %dh exceeds 72h tolerance; accepted but flagged for agent review", res.OfflineAgeHours)
		}
		batch.Results = append(batch.Results, res)
	}
	if err := c.st.Put("capture_batches", idemKey, batch); err != nil {
		return CaptureBatch{}, err
	}
	return batch, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
