package main

import (
	"log"
	"net/http"
	"os"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/keyx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
)

func main() {
	st, err := store.OpenFromEnvProfile()
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	bus := events.NewBusFromEnv(serviceName)
	lc := ledger.NewClientFromEnv()
	engine, err := LoadBandEngine()
	if err != nil {
		log.Fatalf("band engine: %v", err)
	}
	hub := NewPSSPHub()
	psspReg := NewPSSPRegistry(st, hub)
	gates := NewGateClient()
	certs := NewCertificateService(st)
	floats := NewFloatService(st, lc)
	pay := NewPaymentService(st, lc, hub, engine, gates, certs, bus)
	wf := NewPSMWorkflows(st, pay, floats, engine, gates, lc, bus)
	// Recovery worker: resume/compensate interrupted capture sagas and
	// expire abandoned pending intents (boot sweep + interval).
	NewRecoverySweeper(pay, lc, bus).StartRecovery(nil)

	srv := &server{
		pay:     pay,
		float:   floats,
		engine:  engine,
		gates:   gates,
		certs:   certs,
		wf:      wf,
		devices: NewDeviceService(st),
		bus:     bus,
		limiter: NewRateLimiter(20, 20.0/60.0), // 20 verify calls/min per client
		pssps:   psspReg,
		// Webhook replay guard (audit funds-flow #5): timestamp tolerance
		// ±5 min + signature nonce cache; fail-closed in profile=prod.
		guard: webhookguard.NewGuard("X-PSSP-Timestamp", "X-PSSP-Signature", keyx.Prod(), nil),
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8102"
	}
	handler := httpx.CORS(httpx.Auth(publicPath)(srv.routes()))
	log.Printf("presumptive %s listening on :%s (ledger=%T reg_watch=%s packs=%d)",
		serviceVersion, port, lc, os.Getenv("REG_WATCH_URL"), len(engine.Packs()))
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
