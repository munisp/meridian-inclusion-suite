package main

import (
	"log"
	"net/http"
	"os"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/events"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/httpx"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/ledger"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func main() {
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
	wf := NewWorkflows(st, reg, verifier, provisioner, consent, lc, bus)

	srv := &server{
		registry:     reg,
		verifier:     verifier,
		provisioner:  provisioner,
		consent:      consent,
		capture:      capture,
		workflows:    wf,
		associations: NewAssociationService(st, reg, capture),
		crdt:         NewCRDTMergeService(),
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8101"
	}
	handler := httpx.CORS(httpx.Auth(publicPath)(srv.routes()))
	log.Printf("onboarding %s listening on :%s (nimc=%T ledger=%T tin_graph=%s consent_url=%s)",
		serviceVersion, port, verifier, lc, os.Getenv("TIN_GRAPH_URL"), os.Getenv("CONSENT_URL"))
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
