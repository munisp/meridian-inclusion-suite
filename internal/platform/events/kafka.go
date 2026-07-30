package events

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaBus is the prod Bus backed by Redpanda/Kafka via franz-go (H3).
// Topics are identical to the inproc dev bus (SPEC §1.2); the envelope is
// the §1.1 JSON document. Consumer group id = the service name.
type KafkaBus struct {
	client *kgo.Client
	group  string
}

// NewKafkaBus connects to KAFKA_BROKERS (comma-separated host:port list).
func NewKafkaBus(brokers []string, group string) (*KafkaBus, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.ProduceRequestTimeout(10*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &KafkaBus{client: cl, group: group}, nil
}

// Publish produces the envelope to the topic (key = envelope id).
func (b *KafkaBus) Publish(topic string, env Envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec := &kgo.Record{Topic: topic, Key: []byte(env.ID), Value: payload}
	return b.client.ProduceSync(ctx, rec).FirstErr()
}

// Subscribe registers a handler consumed via the service consumer group.
func (b *KafkaBus) Subscribe(topic string, fn func(Envelope)) {
	b.client.AddConsumeTopics(topic)
	go func() {
		for {
			fetches := b.client.PollFetches(context.Background())
			if fetches.IsClientClosed() {
				return
			}
			fetches.EachError(func(_ string, _ int32, err error) {
				log.Printf("events: kafka consume error topic=%s: %v", topic, err)
			})
			fetches.EachRecord(func(rec *kgo.Record) {
				if rec.Topic != topic {
					return
				}
				var env Envelope
				if err := json.Unmarshal(rec.Value, &env); err != nil {
					log.Printf("events: kafka bad envelope topic=%s: %v", rec.Topic, err)
					return
				}
				fn(env)
			})
		}
	}()
}

// Published is a dev/test helper of the inproc bus; KafkaBus returns nil.
func (b *KafkaBus) Published(topic string) []Envelope { return nil }

// Close shuts the client down.
func (b *KafkaBus) Close() { b.client.Close() }

// NewBusFromEnv selects the bus per H1: KAFKA_BROKERS set → franz-go
// Redpanda producer/consumer (profile=prod); unset → embedded inproc bus
// (profile=dev). Startup NEVER fails because KAFKA_BROKERS is missing.
func NewBusFromEnv(service string) Bus {
	if v := os.Getenv("KAFKA_BROKERS"); strings.TrimSpace(v) != "" {
		brokers := strings.Split(v, ",")
		for i := range brokers {
			brokers[i] = strings.TrimSpace(brokers[i])
		}
		kb, err := NewKafkaBus(brokers, service)
		if err != nil {
			log.Printf("profile=prod component=bus kafka unavailable (%v); falling back to inproc", err)
			return NewInprocBus()
		}
		log.Printf("profile=prod component=bus brokers=%s group=%s", v, service)
		return kb
	}
	log.Printf("profile=dev component=bus (inproc)")
	return NewInprocBus()
}
