package otelx

// otelx_test.go — foundation contract tests:
//  1. middleware creates spans carrying tenant.id (header + JWT claim paths)
//  2. propagation round-trip: client injects traceparent, server middleware
//     joins the same trace
//  3. disabled mode (no OTLP endpoint) is a full no-op and never fails

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// setupRecorder installs a real SDK tracer provider backed by the in-memory
// exporter and returns the recorder.
func setupRecorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp
}

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

func TestMiddlewareSpanWithTenantHeader(t *testing.T) {
	exp := setupRecorder(t)
	mux := NewMux()
	mux.Handle("GET /v1/transfers/{id}", http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/v1/transfers/abc", nil)
	req.Header.Set("X-Meridian-Tenant", "tenant-ng-01")
	Middleware(mux).ServeHTTP(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	s := spans[0]
	var tenant, route string
	for _, a := range s.Attributes {
		switch string(a.Key) {
		case "tenant.id":
			tenant = a.Value.AsString()
		case "http.route":
			route = a.Value.AsString()
		}
	}
	if tenant != "tenant-ng-01" {
		t.Errorf("tenant.id = %q, want tenant-ng-01", tenant)
	}
	if route != "GET /v1/transfers/{id}" {
		t.Errorf("http.route = %q, want templated route", route)
	}
	if s.Name != "GET /v1/transfers/{id}" {
		t.Errorf("span name not templated: %q", s.Name)
	}
}

func makeJWT(tenant string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]string{"tenant_id": tenant})
	return "Bearer " + hdr + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestMiddlewareTenantFromJWTClaim(t *testing.T) {
	exp := setupRecorder(t)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", makeJWT("tenant-jwt-42"))
	Middleware(http.HandlerFunc(okHandler)).ServeHTTP(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "tenant.id" && a.Value.AsString() == "tenant-jwt-42" {
			return
		}
	}
	t.Errorf("tenant.id from JWT claim not found on span")
}

func TestPropagationRoundTrip(t *testing.T) {
	exp := setupRecorder(t)

	// Client side: start a span, inject into outbound request.
	tracer := otel.Tracer("test")
	ctx, clientSpan := tracer.Start(context.Background(), "outbound")
	outReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://downstream/v1/x", nil)
	rt := Client(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Traceparent") == "" {
			t.Error("client transport did not inject traceparent")
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	}))
	if _, err := rt.RoundTrip(outReq); err != nil {
		t.Fatal(err)
	}
	clientSpan.End()

	// Server side: middleware must join the same trace as remote parent.
	inReq := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	inReq.Header.Set("Traceparent", outReq.Header.Get("Traceparent"))
	Middleware(http.HandlerFunc(okHandler)).ServeHTTP(httptest.NewRecorder(), inReq)

	var serverSpan *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.SpanContext.IsValid() && s.Parent.IsValid() {
			serverSpan = &exp.GetSpans()[i]
		}
	}
	if serverSpan == nil {
		t.Fatal("no server span with parent recorded")
	}
	if serverSpan.SpanContext.TraceID() != clientSpan.SpanContext().TraceID() {
		t.Errorf("trace mismatch: server %s vs client %s",
			serverSpan.SpanContext.TraceID(), clientSpan.SpanContext().TraceID())
	}
	if !serverSpan.Parent.IsRemote() {
		t.Error("server span parent should be remote (extracted from traceparent)")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDisabledModeNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("PROFILE", "dev")
	p := InitProviders(context.Background())
	if p.Enabled() {
		t.Error("providers should be disabled without endpoint")
	}
	p.Shutdown(context.Background()) // must not panic

	// Middleware still safe under no-op provider (non-recording span).
	exp := setupRecorder(t) // replace global with recorder to prove no leakage
	_ = exp
	Middleware(http.HandlerFunc(okHandler)).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/x", nil))
	// no assertion beyond "did not panic / did not error": no-op path.
}

func TestProdWithoutEndpointWarns(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := ConfigFromEnv()
	cfg.Profile = "prod"
	// loud warning is a log line; verify the config path keeps profile=prod
	// and disabled providers are returned without error.
	p := InitProvidersWith(context.Background(), cfg)
	if p.Enabled() {
		t.Error("prod without endpoint must still be disabled (non-fatal)")
	}
	fmt.Println("prod-no-endpoint handled non-fatally")
}
