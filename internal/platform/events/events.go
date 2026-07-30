// Package events implements the §1.1 event envelope and the dev in-process
// bus fallback (EVENT_BUS=inproc default). Producers follow the outbox
// pattern: domain write + outbox row, then relay to the bus.
package events

import (
	"sync"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
)

// Envelope is the canonical §1.1 event envelope.
type Envelope struct {
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	Source          string         `json:"source"`
	Time            string         `json:"time"`
	TenantID        string         `json:"tenant_id"`
	TraceID         string         `json:"trace_id"`
	RulePackVersion string         `json:"rule_pack_version"`
	Data            map[string]any `json:"data"`
}

// New builds an envelope with a fresh ULID + RFC3339 timestamp.
func New(typ, source, tenantID, rulePackVersion string, data map[string]any) Envelope {
	return Envelope{
		ID:              ids.New(),
		Type:            typ,
		Source:          source,
		Time:            time.Now().UTC().Format(time.RFC3339),
		TenantID:        tenantID,
		TraceID:         ids.New(),
		RulePackVersion: rulePackVersion,
		Data:            data,
	}
}

// Bus is the publish/subscribe abstraction (inproc dev fallback for Redpanda).
type Bus interface {
	Publish(topic string, env Envelope) error
	Subscribe(topic string, fn func(Envelope))
	// Published returns a copy of every envelope published to a topic (dev/test).
	Published(topic string) []Envelope
}

// InprocBus is the single-binary dev bus implementing Bus.
type InprocBus struct {
	mu     sync.RWMutex
	subs   map[string][]func(Envelope)
	events map[string][]Envelope
}

func NewInprocBus() *InprocBus {
	return &InprocBus{subs: map[string][]func(Envelope){}, events: map[string][]Envelope{}}
}

func (b *InprocBus) Publish(topic string, env Envelope) error {
	b.mu.Lock()
	b.events[topic] = append(b.events[topic], env)
	subs := append([]func(Envelope){}, b.subs[topic]...)
	b.mu.Unlock()
	for _, fn := range subs {
		fn(env)
	}
	return nil
}

func (b *InprocBus) Subscribe(topic string, fn func(Envelope)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], fn)
}

func (b *InprocBus) Published(topic string) []Envelope {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Envelope{}, b.events[topic]...)
}
