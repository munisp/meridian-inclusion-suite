package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
)

// presumptiveGateID is the reg-watch gate gating presumptive collections
// (SPEC §4 T12: "collections blocked until presumptive gate open").
const presumptiveGateID = "G8.presumptive_reg"

// GateClient enforces reg-watch gates with a local gate-file fallback.
type GateClient struct {
	base string // REG_WATCH_URL, "" => local file fallback
	file string // GATE_FILE, default ./gates.dev.json
	hc   *http.Client
	mu   sync.Mutex
}

func NewGateClient() *GateClient {
	f := os.Getenv("GATE_FILE")
	if f == "" {
		f = "gates.dev.json"
	}
	return &GateClient{
		base: strings.TrimRight(os.Getenv("REG_WATCH_URL"), "/"),
		file: f,
		hc:   &http.Client{Timeout: 8 * time.Second},
	}
}

func (g *GateClient) localRead() (map[string]GateState, error) {
	b, err := os.ReadFile(g.file)
	if err != nil {
		if os.IsNotExist(err) {
			// default: presumptive gate CLOSED until regulation confirmed
			return map[string]GateState{
				presumptiveGateID: {
					ID: presumptiveGateID, Open: false,
					Description: "Presumptive taxation regulation (post-regulation gate; collections blocked until open)",
					UpdatedAt:   nowRFC3339(), Source: "local_file",
				},
			}, nil
		}
		return nil, err
	}
	var gates map[string]GateState
	if err := json.Unmarshal(b, &gates); err != nil {
		return nil, err
	}
	return gates, nil
}

func (g *GateClient) localWrite(gates map[string]GateState) error {
	if err := os.MkdirAll(filepath.Dir(g.file), 0o755); err != nil && filepath.Dir(g.file) != "." {
		return err
	}
	b, err := json.MarshalIndent(gates, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.file, b, 0o644)
}

// Gates returns all known gates (reg-watch when configured, else local file).
func (g *GateClient) Gates() (map[string]GateState, error) {
	if g.base != "" {
		req, _ := http.NewRequest(http.MethodGet, g.base+"/v1/gates", nil)
		req.Header.Set("X-Dev-Role", "operator")
		resp, err := g.hc.Do(req)
		if err == nil && resp.StatusCode < 300 {
			defer resp.Body.Close()
			var payload struct {
				Gates []GateState `json:"gates"`
			}
			if derr := json.NewDecoder(resp.Body).Decode(&payload); derr == nil {
				out := map[string]GateState{}
				for _, gs := range payload.Gates {
					gs.Source = "reg_watch"
					out[gs.ID] = gs
				}
				return out, nil
			}
		} else if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		// reg-watch unreachable -> local fallback
	}
	// B2-#5: board-gate state is authoritative ONLY at reg-watch. In prod a
	// local file (writable by whoever has fs access) must never substitute —
	// fail closed rather than risk collections proceeding on a forged/stale
	// local gate file while the board source is down.
	if keyx.Prod() {
		return nil, fmt.Errorf("reg-watch unreachable and profile=prod: refusing local-file gate fallback (fail closed)")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.localRead()
}

// CollectionsOpen reports whether presumptive collections may proceed.
func (g *GateClient) CollectionsOpen() (bool, error) {
	gates, err := g.Gates()
	if err != nil {
		return false, err
	}
	gs, ok := gates[presumptiveGateID]
	if !ok {
		return false, nil // unknown gate => fail closed
	}
	return gs.Open, nil
}

// Flip flips a gate (board-authorized; dev mode) via reg-watch or local file.
func (g *GateClient) Flip(id string, open bool) (GateState, error) {
	if g.base != "" {
		body, _ := json.Marshal(map[string]bool{"open": open})
		req, _ := http.NewRequest(http.MethodPost, g.base+"/v1/gates/"+id+"/flip", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dev-Role", "admin")
		resp, err := g.hc.Do(req)
		if err == nil && resp.StatusCode < 300 {
			defer resp.Body.Close()
			var gs GateState
			if derr := json.NewDecoder(resp.Body).Decode(&gs); derr == nil {
				gs.Source = "reg_watch"
				return gs, nil
			}
		} else if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	// B2-#5: no local-file board flips in prod — a flip is a board action and
	// must go through reg-watch (with authorization_ref) or not happen.
	if g.base != "" && keyx.Prod() {
		return GateState{}, fmt.Errorf("reg-watch unavailable in profile=prod: refusing local-file gate flip (fail closed)")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	gates, err := g.localRead()
	if err != nil {
		return GateState{}, err
	}
	gs, ok := gates[id]
	if !ok {
		gs = GateState{ID: id, Description: "locally registered gate"}
	}
	gs.Open = open
	gs.UpdatedAt = nowRFC3339()
	gs.Source = "local_file"
	gates[id] = gs
	if err := g.localWrite(gates); err != nil {
		return GateState{}, err
	}
	return gs, nil
}

// ErrGateClosed is returned (as problem detail) when collections are blocked.
var ErrGateClosed = fmt.Errorf("presumptive collections are blocked: gate %s is closed", presumptiveGateID)
