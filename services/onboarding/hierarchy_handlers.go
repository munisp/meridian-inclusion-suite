package main

// hierarchy_handlers.go — I6 HTTP surface for the agent hierarchy. All
// endpoints are tenant-scoped and JWT-bound: admin/operator manage any agent
// in their own tenant; agent principals manage only their own subtree.

import (
	"errors"
	"net/http"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

// attachSubAgent handles POST /v1/agents/{id}/parent: links agent {id} under
// the supplied parent_id ("" detaches). The caller must be allowed to manage
// BOTH the child and the new parent (no grafting your subtree under someone
// else's, no absorbing another tree into yours).
func (s *server) attachSubAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ParentID string `json:"parent_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	child, ok := s.hierarchy.visibleAgent(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if in.ParentID != "" {
		if _, ok := s.hierarchy.visibleAgent(w, r, in.ParentID); !ok {
			return
		}
	}
	ag, err := s.hierarchy.Attach(child.ID, in.ParentID)
	if err != nil {
		switch {
		case errors.Is(err, ErrHierarchyCycle):
			httpx.WriteProblem(w, http.StatusConflict, "hierarchy_cycle", err.Error())
		case errors.Is(err, ErrDepthCap):
			httpx.WriteProblem(w, http.StatusUnprocessableEntity, "depth_cap", err.Error())
		case errors.Is(err, ErrTenantMismatch):
			httpx.WriteProblem(w, http.StatusForbidden, "tenant_mismatch", err.Error())
		default:
			httpx.WriteProblem(w, http.StatusBadRequest, "validation", err.Error())
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ag)
}

// agentDownline handles GET /v1/agents/{id}/downline: the full subtree under
// agent {id} (self included), tenant- and subtree-scoped.
func (s *server) agentDownline(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.hierarchy.visibleAgent(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	sub, err := s.hierarchy.Subtree(ag.ID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agents": sub, "count": len(sub), "root": ag.ID})
}
