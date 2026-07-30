package main

import (
	"log"
	"net/http"
	"os"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
)

func main() {
	graph, err := LoadMenuGraph()
	if err != nil {
		log.Fatalf("menu graph: %v", err)
	}
	bus := events.NewBusFromEnv(serviceName)
	engine := NewEngine(graph, RegisterActions(busAdapter{bus: bus}))
	store := NewInMemSessionStore(graph.SessionTTLSeconds)

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
