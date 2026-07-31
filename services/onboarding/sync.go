package main

import (
	"net/http"
	"sync"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/crdtx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// sync.go — server side of the CRDT sync protocol (I18): agents ship
// operation batches from their offline outbox; the server merges them into a
// shared OR-Set. Conflict-free under retries/replay because op application is
// idempotent and order-independent (crdtx).

// CRDTMergeService is the server merge endpoint state.
type CRDTMergeService struct {
	mu  sync.Mutex
	set *crdtx.ORSet
}

func NewCRDTMergeService() *CRDTMergeService {
	return &CRDTMergeService{set: crdtx.NewORSet()}
}

type mergeRequest struct {
	Ops []crdtx.Op `json:"ops"`
}

type mergeResponse struct {
	Applied  int      `json:"applied"`
	Ignored  int      `json:"ignored"` // duplicate/replayed ops
	Elements []string `json:"elements"`
}

// Merge applies an op batch; safe to call with duplicates or out of order.
func (m *CRDTMergeService) Merge(ops []crdtx.Op) mergeResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	applied := m.set.Merge(ops)
	return mergeResponse{Applied: applied, Ignored: len(ops) - applied, Elements: m.set.Elements()}
}

func (s *server) crdtMerge(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(req.Ops) == 0 || len(req.Ops) > 2000 {
		httpx.WriteProblem(w, http.StatusBadRequest, "validation", "ops must contain 1..2000 operations")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.crdt.Merge(req.Ops))
}
