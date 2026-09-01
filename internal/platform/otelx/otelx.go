// Package otelx provides shared OpenTelemetry bootstrap for all Meridian
// inclusion-suite Go services (OTEL foundation wave). It wires OTLP gRPC exporters for traces,
// metrics and logs from OTEL_EXPORTER_OTLP_ENDPOINT, installs the global
// tracer/meter/logger providers and propagators, and ships HTTP middleware +
// client wrappers that carry tenant.id on every span.
//
// HARD RULE (money paths): telemetry is best-effort. When the OTLP endpoint
// is unset the package degrades to no-op providers and NEVER fails startup.
// PROFILE=prod without an endpoint emits a loud startup warning instead.
package otelx

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	logapi "go.opentelemetry.io/otel/log"
	nooplog "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// Providers bundles the installed SDK providers for shutdown.
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	LoggerProvider logapi.LoggerProvider
	enabled        bool
	shutdown       []func(context.Context) error
}

// Enabled reports whether OTLP exporters were actually configured.
func (p *Providers) Enabled() bool { return p != nil && p.enabled }

// Shutdown flushes and stops all providers. Safe on disabled providers.
func (p *Providers) Shutdown(ctx context.Context) {
	if p == nil {
		return
	}
	for i := len(p.shutdown) - 1; i >= 0; i-- {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := p.shutdown[i](cctx); err != nil {
			log.Printf("otelx: provider shutdown error: %v", err)
		}
		cancel()
	}
}

// Config mirrors the environment-driven bootstrap knobs (DESIGN-CONTRACT.md).
type Config struct {
	Endpoint string // OTEL_EXPORTER_OTLP_ENDPOINT (gRPC, e.g. otel-collector:4317)
	Service  string // OTEL_SERVICE_NAME, fallback SERVICE_NAME
	Version  string // OTEL_SERVICE_VERSION, fallback SERVICE_VERSION
	Env      string // DEPLOYMENT_ENVIRONMENT, fallback PROFILE
	Profile  string // PROFILE (dev|prod)
	Insecure bool   // OTEL_EXPORTER_OTLP_INSECURE=true (default true in dev)
}

// ConfigFromEnv reads the standard env vars.
func ConfigFromEnv() Config {
	c := Config{
		Endpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Service:  firstEnv("OTEL_SERVICE_NAME", "SERVICE_NAME"),
		Version:  firstEnv("OTEL_SERVICE_VERSION", "SERVICE_VERSION"),
		Env:      firstEnv("DEPLOYMENT_ENVIRONMENT", "PROFILE"),
		Profile:  os.Getenv("PROFILE"),
	}
	c.Insecure = os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") != "false"
	if c.Service == "" {
		c.Service = "unknown-service"
	}
	if c.Version == "" {
		c.Version = "0.0.0"
	}
	if c.Env == "" {
		c.Env = "dev"
	}
	return c
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// InitProviders bootstraps OTel from the environment. When no endpoint is
// configured it returns disabled (no-op) providers and a nil error; with
// PROFILE=prod it also logs a loud warning. Exporter construction failures
// are logged and likewise degrade to no-op: telemetry must never break
// money paths.
func InitProviders(ctx context.Context) *Providers {
	return InitProvidersWith(ctx, ConfigFromEnv())
}

// InitProvidersWith is InitProviders against an explicit Config (tests).
func InitProvidersWith(ctx context.Context, cfg Config) *Providers {
	if cfg.Endpoint == "" {
		if cfg.Profile == "prod" {
			log.Printf("otelx: WARNING PROFILE=prod but OTEL_EXPORTER_OTLP_ENDPOINT is unset; "+
				"service=%s running with NO telemetry export (traces/metrics/logs dropped)", cfg.Service)
		}
		return disabledProviders()
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.Service),
			semconv.ServiceVersionKey.String(cfg.Version),
			semconv.DeploymentEnvironmentKey.String(cfg.Env),
		),
	)
	if err != nil {
		log.Printf("otelx: resource build failed (%v); telemetry disabled", err)
		return disabledProviders()
	}

	p := &Providers{enabled: true}

	// Traces
	if tp, err := buildTracer(ctx, cfg, res); err != nil {
		log.Printf("otelx: trace exporter failed (%v); traces disabled", err)
		p.TracerProvider = nooptrace.NewTracerProvider()
	} else {
		p.TracerProvider = tp
		p.shutdown = append(p.shutdown, tp.Shutdown)
	}
	otel.SetTracerProvider(p.TracerProvider)

	// Metrics
	if mp, err := buildMeter(ctx, cfg, res); err != nil {
		log.Printf("otelx: metric exporter failed (%v); metrics disabled", err)
		p.MeterProvider = noopmetric.NewMeterProvider()
	} else {
		p.MeterProvider = mp
		p.shutdown = append(p.shutdown, mp.Shutdown)
	}
	otel.SetMeterProvider(p.MeterProvider)

	// Logs
	if lp, err := buildLogger(ctx, cfg, res); err != nil {
		log.Printf("otelx: log exporter failed (%v); logs disabled", err)
		p.LoggerProvider = nooplog.NewLoggerProvider()
	} else {
		p.LoggerProvider = lp
		p.shutdown = append(p.shutdown, lp.Shutdown)
	}

	// Propagation: W3C tracecontext + baggage, platform-wide.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return p
}

func disabledProviders() *Providers {
	p := &Providers{
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
		LoggerProvider: nooplog.NewLoggerProvider(),
	}
	otel.SetTracerProvider(p.TracerProvider)
	otel.SetMeterProvider(p.MeterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	return p
}

func buildTracer(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracegrpc.New(ctx, grpcOpts(cfg)...)
	if err != nil {
		return nil, err
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	), nil
}

func buildMeter(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	exp, err := otlpmetricgrpc.New(ctx, metricGRPCOpts(cfg)...)
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(30*time.Second))),
	), nil
}

func buildLogger(ctx context.Context, cfg Config, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	exp, err := otlploggrpc.New(ctx, logGRPCOpts(cfg)...)
	if err != nil {
		return nil, err
	}
	return sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	), nil
}

// sampleRatio reads OTEL_TRACES_SAMPLER_ARG (default 1.0; prod should set
// e.g. 0.1 — see DESIGN-CONTRACT.md exemplar/sampling policy).
func sampleRatio() float64 {
	v := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if v == "" {
		return 1.0
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%g", &f); err != nil || f < 0 || f > 1 {
		return 1.0
	}
	return f
}

// grpc option helpers — each exporter family has its own option type.

func grpcOpts(cfg Config) []otlptracegrpc.Option {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(stripScheme(cfg.Endpoint))}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return opts
}

func metricGRPCOpts(cfg Config) []otlpmetricgrpc.Option {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(stripScheme(cfg.Endpoint))}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return opts
}

func logGRPCOpts(cfg Config) []otlploggrpc.Option {
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(stripScheme(cfg.Endpoint))}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	return opts
}

// stripScheme removes an http(s):// prefix: gRPC exporter options want
// host:port.
func stripScheme(ep string) string {
	for _, p := range []string{"http://", "https://"} {
		if len(ep) > len(p) && ep[:len(p)] == p {
			return ep[len(p):]
		}
	}
	return ep
}
