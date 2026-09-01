package otelx

// middleware.go — server + client HTTP instrumentation wrapping the existing
// httpx servers. One span per request named "<METHOD> <route-template>" using
// the Go 1.22 ServeMux pattern (low cardinality), with tenant.id set from
// TenantFromRequest. The client wrapper injects W3C tracecontext + baggage
// into outbound requests.
//
// NOTE: http.Request.Pattern is only available from Go 1.23; this repo still
// builds with the Go 1.22 toolchain (Dockerfile.go-service). Route templates
// are therefore stamped by the Mux wrapper below (otelx.NewMux), which knows
// the pattern at registration time. Requests served by a plain mux (or
// unmatched paths) are labelled with the "unmatched" route — never the raw
// path (cardinality ban, DESIGN-CONTRACT.md).

import (
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/munisp/meridian-inclusion-suite/internal/platform/otelx"

// spanStatusRecorder mirrors httpx's recorder: default 200, capture explicit
// WriteHeader, and satisfy http.Flusher for streaming handlers.
type spanStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *spanStatusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *spanStatusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware wraps an httpx handler chain: extracts inbound trace context,
// starts a server span, labels it with tenant.id, and lets the Mux route
// wrapper stamp the route template. Unmatched/unknown routes are labelled
// "unmatched" (never the raw path).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(),
			propagationHeaderCarrier(r.Header))
		tracer := otel.Tracer(tracerName)
		ctx, span := tracer.Start(ctx, r.Method+" unmatched",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				// default route label; a Mux-registered handler overwrites
				// it with the matched template (never the raw path).
				semconv.HTTPRoute("unmatched"),
			))
		defer span.End()

		tenant := TenantFromRequest(r.WithContext(ctx))
		if tenant != "" {
			span.SetAttributes(TenantAttr(tenant))
			// Reflect tenant into baggage so downstream hops inherit it.
			if m, err := baggage.NewMember(TenantKey, tenant); err == nil {
				if bg, err := baggage.New(m); err == nil {
					ctx = baggage.ContextWithBaggage(ctx, bg)
				}
			}
		}

		sr := &spanStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r.WithContext(ctx))

		span.SetAttributes(semconv.HTTPResponseStatusCode(sr.status))
		if sr.status >= 500 {
			span.SetStatus(codes.Error, fmt.Sprintf("status %d", sr.status))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}

// Mux is a drop-in *http.ServeMux replacement whose registrations stamp the
// matched route template onto the active server span (Go 1.22 lacks
// http.Request.Pattern). Use otelx.NewMux() in place of http.NewServeMux()
// and wrap the mux with otelx.Middleware.
type Mux struct{ *http.ServeMux }

func NewMux() *Mux { return &Mux{http.NewServeMux()} }

func (m *Mux) Handle(pattern string, handler http.Handler) {
	m.ServeMux.Handle(pattern, routeHandler{pattern: pattern, next: handler})
}

func (m *Mux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

// routeHandler stamps the registered route template onto the server span
// started by Middleware and records the pattern in context.
type routeHandler struct {
	pattern string
	next    http.Handler
}

func (rh routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	span := trace.SpanFromContext(r.Context())
	// Go 1.22 patterns usually embed the method ("GET /v1/x"); avoid "GET GET".
	name := rh.pattern
	if !strings.HasPrefix(rh.pattern, r.Method+" ") {
		name = r.Method + " " + rh.pattern
	}
	span.SetName(name)
	span.SetAttributes(semconv.HTTPRoute(rh.pattern))
	rh.next.ServeHTTP(w, r)
}

// propagationHeaderCarrier adapts http.Header to propagation.TextMapCarrier.
type propagationHeaderCarrier http.Header

func (c propagationHeaderCarrier) Get(key string) string { return http.Header(c).Get(key) }
func (c propagationHeaderCarrier) Set(key, value string) { http.Header(c).Set(key, value) }
func (c propagationHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Client instruments an outbound HTTP client: traceparent/tracestate/baggage
// are injected from the request context and a client span is created per call.
// Pass nil to wrap http.DefaultTransport.
func Client(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &clientTransport{base: rt}
}

type clientTransport struct{ base http.RoundTripper }

func (t *clientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tracer := otel.Tracer(tracerName)
	ctx, span := tracer.Start(req.Context(), req.Method+" "+req.URL.Host+req.URL.Path,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(req.Method),
			semconv.URLFull(req.URL.String()),
			attribute.String("server.address", req.URL.Host),
		))
	defer span.End()
	otel.GetTextMapPropagator().Inject(ctx, propagationHeaderCarrier(req.Header))
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("status %d", resp.StatusCode))
	}
	return resp, nil
}
