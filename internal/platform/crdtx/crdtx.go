// Package crdtx implements the offline-first CRDT sync protocol (I18): an
// operation-based OR-Set (observed-remove set) with Hybrid Logical Clock
// timestamps for the agent capture outbox. Duplicate deliveries are no-ops
// and out-of-order delivery converges — retries and replays are safe.
package crdtx

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// HLC is a Hybrid Logical Clock timestamp: physical wall time + logical
// counter + node id (total order, tie-broken deterministically).
type HLC struct {
	Wall  int64  `json:"wall"`  // unix nano
	Count uint32 `json:"count"` // logical counter
	Node  string `json:"node"`  // node/agent id
}

// Less orders HLCs: wall, then count, then node id.
func (h HLC) Less(o HLC) bool {
	if h.Wall != o.Wall {
		return h.Wall < o.Wall
	}
	if h.Count != o.Count {
		return h.Count < o.Count
	}
	return h.Node < o.Node
}

func (h HLC) String() string { return fmt.Sprintf("%d.%d.%s", h.Wall, h.Count, h.Node) }

// Clock is a per-node HLC generator.
type Clock struct {
	mu   sync.Mutex
	node string
	last HLC
}

func NewClock(node string) *Clock { return &Clock{node: node} }

// Now returns a fresh HLC for a local event.
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall := time.Now().UnixNano()
	if wall <= c.last.Wall {
		wall = c.last.Wall
		c.last.Count++
	} else {
		c.last.Count = 0
	}
	c.last = HLC{Wall: wall, Count: c.last.Count, Node: c.node}
	return c.last
}

// Update merges a received HLC (clock drift protection) and returns a fresh
// local HLC strictly after it.
func (c *Clock) Update(remote HLC) HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall := time.Now().UnixNano()
	maxWall := c.last.Wall
	if remote.Wall > maxWall {
		maxWall = remote.Wall
	}
	var count uint32
	switch {
	case wall > maxWall:
		count = 0
	case maxWall == c.last.Wall && maxWall == remote.Wall:
		count = max32(c.last.Count, remote.Count) + 1
	case maxWall == c.last.Wall:
		count = c.last.Count + 1
	default:
		count = remote.Count + 1
	}
	c.last = HLC{Wall: max32i64(wall, maxWall), Count: count, Node: c.node}
	return c.last
}

func max32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}
func max32i64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Op is one sync operation (add or remove an element).
type Op struct {
	ID      string `json:"id"`                // unique op id (uuid) — dedup key
	Kind    string `json:"kind"`              // add|remove
	Element string `json:"element"`           // e.g. client_ref of the capture record
	Tag     HLC    `json:"tag"`               // unique tag for adds; target tag for removes
	Payload string `json:"payload,omitempty"` // opaque element payload (JSON)
}

// ORSet is an observed-remove set: adds carry unique HLC tags; removes
// tombstone only the tags the remover had observed, so concurrent adds win
// over removes (add-wins semantics).
type ORSet struct {
	adds    map[string]map[string]Op // element -> tag string -> op
	removes map[string]map[string]bool
	seen    map[string]bool // processed op ids (idempotency)
}

func NewORSet() *ORSet {
	return &ORSet{adds: map[string]map[string]Op{}, removes: map[string]map[string]bool{}, seen: map[string]bool{}}
}

// Add stages a local add op.
func (s *ORSet) Add(clk *Clock, element, payload string) Op {
	op := Op{ID: fmt.Sprintf("%s-%d", clk.Now().String(), time.Now().UnixNano()), Kind: "add", Element: element, Tag: clk.Now(), Payload: payload}
	s.Apply(op)
	return op
}

// Remove stages a local remove op for all currently observed tags.
func (s *ORSet) Remove(clk *Clock, element string) []Op {
	var ops []Op
	for tagStr := range s.adds[element] {
		if s.removes[element][tagStr] {
			continue
		}
		var tag HLC
		fmt.Sscanf(tagStr, "%d.%d.%s", &tag.Wall, &tag.Count, &tag.Node)
		op := Op{ID: fmt.Sprintf("rm-%s-%s", element, tagStr), Kind: "remove", Element: element, Tag: tag}
		s.Apply(op)
		ops = append(ops, op)
	}
	return ops
}

// Apply merges one op. Idempotent (op id dedup) and order-independent.
func (s *ORSet) Apply(op Op) bool {
	if op.ID == "" || s.seen[op.ID] {
		return false // duplicate delivery: no-op
	}
	s.seen[op.ID] = true
	ts := op.Tag.String()
	switch op.Kind {
	case "add":
		if s.adds[op.Element] == nil {
			s.adds[op.Element] = map[string]Op{}
		}
		s.adds[op.Element][ts] = op
	case "remove":
		if s.removes[op.Element] == nil {
			s.removes[op.Element] = map[string]bool{}
		}
		s.removes[op.Element][ts] = true
	default:
		return false
	}
	return true
}

// Merge applies a batch of ops (any order, duplicates tolerated) and returns
// the number of NEW ops applied.
func (s *ORSet) Merge(ops []Op) int {
	n := 0
	for _, op := range ops {
		if s.Apply(op) {
			n++
		}
	}
	return n
}

// Contains reports whether the element is live in the set.
func (s *ORSet) Contains(element string) bool {
	for ts := range s.adds[element] {
		if !s.removes[element][ts] {
			return true
		}
	}
	return false
}

// Elements returns the live elements, sorted for determinism.
func (s *ORSet) Elements() []string {
	var out []string
	for el := range s.adds {
		if s.Contains(el) {
			out = append(out, el)
		}
	}
	sort.Strings(out)
	return out
}
