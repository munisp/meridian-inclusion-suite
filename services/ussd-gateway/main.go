package main

import (
	"log"
	"os"

	"strings"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	kvstore "github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
)

func main() {
	graph, err := LoadMenuGraph()
	if err != nil {
		log.Fatalf("menu graph: %v", err)
	}
	// I3 fail-closed gate: prod must wire the einvoicing upstream explicitly.
	if _, err := einvConfigFromEnv(); err != nil {
		log.Fatalf("einvoicing config: %v", err)
	}
	bus := events.NewBusFromEnv(serviceName)
	actions := RegisterActions(busAdapter{bus: bus})
	// M-2: USSD PIN gate for sensitive actions (hashed storage, 3-strike lock).
	registerPINActions(actions, NewPINManager(NewInMemPINStore(), busAdapter{bus: bus}))
	engine := NewEngine(graph, actions)
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

	srv := &server{graph: graph, engine: engine, store: store, bus: bus, notifier: NewAggregatorNotifierFromEnv(),
		// M-1: webhook replay guard — fail closed (headers required) in prod.
		guard: webhookguard.NewGuard("X-Aggregator-Timestamp", "X-Aggregator-Nonce", keyx.Prod(), nil),
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8104"
	}
	// M-6/M-7: bounded bodies + origin-scoped CORS wrap the whole chain.
	handler := httpx.MaxBody(httpx.CORS(httpx.Auth(publicPath)(srv.routes())))
	log.Printf("ussd-gateway %s listening on :%s (service_code=%s ttl=%ds menus=%d)",
		serviceVersion, port, graph.ServiceCode, graph.SessionTTLSeconds, len(graph.Menus))
	log.Fatal(httpx.ListenAndServe(":"+port, handler))
}

// publicPath lists routes exempt from httpx.Auth. F-3: /v1/simulate is a
// dev/test convenience that drives the real menu engine + action bus — it is
// never auth-exempt in prod (and the handler itself 404s there, see
// handlers.go), so an unauthenticated prod caller gets 401.
func publicPath(p string) bool {
	return p == "/healthz" || p == "/readyz" || p == "/webhook/ussd" ||
		(p == "/v1/simulate" && !keyx.Prod())
}
