package main

import (
	"context"
	"log"
	"os"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/otelx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

func main() {
	// OTel bootstrap (DESIGN-CONTRACT.md): fail-soft — no endpoint means
	// no-op providers, never a startup failure.
	otelCtx := context.Background()
	providers := otelx.InitProviders(otelCtx)
	defer providers.Shutdown(otelCtx)

	st, err := store.OpenFromEnvProfile()
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	bus := events.NewBusFromEnv(serviceName)
	reg := NewRegistry(st)
	verifier := NewNINVerifierFromEnv()
	provisioner := NewTINProvisionerFromEnv()
	consent := NewConsentService(st)
	capture := NewCaptureService(st, reg, verifier)
	lc := ledger.NewClientFromEnv()
	// O1: durable workflow runner — TEMPORAL_URL required (fail-closed) in prod.
	runner, err := workflowx.NewRunnerFromEnv(storeRunStore{st}, nil)
	if err != nil {
		log.Fatalf("workflow runner: %v", err)
	}
	wf := NewWorkflowsWithRunner(st, reg, verifier, provisioner, consent, lc, bus, runner)

	// O4: document backend — MinIO WORM presign in prod, dev FS fallback.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	docBackend, err := NewDocBackendFromEnv(dataDir)
	if err != nil {
		log.Fatalf("doc backend: %v", err)
	}
	srv := &server{
		registry:     reg,
		verifier:     verifier,
		provisioner:  provisioner,
		consent:      consent,
		capture:      capture,
		workflows:    wf,
		associations: NewAssociationService(st, reg, capture),
		crdt:         NewCRDTMergeService(),
		agents:       NewAgentRegistry(st),
		docs:         NewDocService(st, reg, docBackend),
	}
	// I6: agent hierarchy + rule-pack commission engine (fail-closed pack
	// load: COMMISSION_PACK_VERSION must match the carried table).
	srv.hierarchy = NewHierarchy(srv.agents)
	ce, err := LoadCommissionEngine(st, srv.hierarchy, lc)
	if err != nil {
		log.Fatalf("commission engine: %v", err)
	}
	srv.commissions = ce
	if fs, ok := docBackend.(*fsDocBackend); ok {
		srv.fsBackend = fs
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8101"
	}
	// M-6/M-7: bounded bodies + origin-scoped CORS wrap the whole chain.
	handler := otelx.Middleware(httpx.MaxBody(httpx.CORS(httpx.Auth(publicPath)(srv.routes()))))
	log.Printf("onboarding %s listening on :%s (nimc=%T ledger=%T tin_graph=%s consent_url=%s)",
		serviceVersion, port, verifier, lc, os.Getenv("TIN_GRAPH_URL"), os.Getenv("CONSENT_URL"))
	log.Fatal(httpx.ListenAndServe(":"+port, handler))
}
