package main

import (
	"strings"
	"testing"
)

// B2-#5 regression: board-gate local-file fallback must fail closed in prod
// when the board source (reg-watch) is unavailable.
func TestGateFallbackFailClosedInProd(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	g := NewGateClient()
	g.base = "http://127.0.0.1:1" // unreachable reg-watch
	if _, err := g.Gates(); err == nil || !strings.Contains(err.Error(), "fail closed") {
		t.Fatalf("prod Gates() with reg-watch down must fail closed, got %v", err)
	}
	if _, err := g.Flip(presumptiveGateID, true); err == nil || !strings.Contains(err.Error(), "fail closed") {
		t.Fatalf("prod Flip() with reg-watch down must fail closed, got %v", err)
	}
}

// Dev keeps the local-file fallback (existing behaviour preserved).
func TestGateFallbackAllowedInDev(t *testing.T) {
	t.Setenv("APP_PROFILE", "dev")
	t.Setenv("GATE_FILE", t.TempDir()+"/gates.json")
	g := NewGateClient()
	g.base = "http://127.0.0.1:1"
	gates, err := g.Gates()
	if err != nil {
		t.Fatalf("dev fallback must still work: %v", err)
	}
	if gs, ok := gates[presumptiveGateID]; !ok || gs.Open {
		t.Fatalf("dev fallback must default presumptive gate CLOSED: %+v", gates)
	}
}
