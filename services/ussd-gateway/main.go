package main

import (
	"log"
	"net/http"
	"os"

	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	kvstore "github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func main() {
	graph, err := LoadMenuGraph()
	if err != nil {
		log.Fatalf("menu graph: %v", err)
	}
	bus := events.NewBusFromEnv(serviceName)
	engine := NewEngine(graph, RegisterActions(busAdapter{bus: bus}))
	// Session store: Redis when REDIS_URL is set (multi-node), otherwise the
	// durable embedded-KV store (restart-safe; enables session resume), with
	// in-mem as the bare-dev last resort when the KV store can't open.
	var store SessionStore
	if addr := os.Getenv("REDIS_URL"); addr != "" {
		rs, err := NewRedisSessionStore(strings.TrimPrefix(addr, "redis://"), graph.SessionTTLSeconds)
		if err != nil {
			log.Fatalf("REDIS_URL set but unusable (fail closed): %v", err)
		}
		store = rs
		log.Printf("profile=prod component=ussd-sessions store=redis addr=%s", addr)
	} else if kv, err := kvstore.OpenFromEnvProfile(); err == nil {
		store = NewKVSessionStore(kv, graph.SessionTTLSeconds)
		log.Printf("profile=dev component=ussd-sessions store=embedded-kv (resume enabled)")
	} else {
		store = NewInMemSessionStore(graph.SessionTTLSeconds)
		log.Printf("profile=dev component=ussd-sessions store=in-mem (no resume): %v", err)
	}

	srv := &server{graph: graph, engine: engine, store: store, bus: bus, notifier: NewAggregatorNotifierFromEnv()}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8104"
	}
	handler := httpx.CORS(httpx.Auth(func(p string) bool {
		return p == "/healthz" || p == "/readyz" || p == "/webhook/ussd" || p == "/v1/simulate"
	})(srv.routes()))
	log.Printf("ussd-gateway %s listening on :%s (service_code=%s ttl=%ds menus=%d)",
		serviceVersion, port, graph.ServiceCode, graph.SessionTTLSeconds, len(graph.Menus))
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
