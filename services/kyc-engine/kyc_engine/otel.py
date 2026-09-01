"""OpenTelemetry bootstrap for kyc-engine (local copy of meridian_py.otel).

Contract: DESIGN-CONTRACT.md (otel-foundation wave). Attribute names,
env vars and the prod-without-endpoint loud-warning rule match the Go
otelx package.

Hard rule (money paths): telemetry is best-effort. With no
OTEL_EXPORTER_OTLP_ENDPOINT the SDK stays installed but exports nothing
and init_otel never raises during app startup.
"""

from __future__ import annotations

import logging
import os
from typing import Any, Optional

log = logging.getLogger("kyc_engine.otel")

TENANT_KEY = "tenant.id"


def _cfg() -> dict:
    return {
        "endpoint": os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
        "service": os.environ.get("OTEL_SERVICE_NAME")
        or os.environ.get("SERVICE_NAME", "unknown-service"),
        "version": os.environ.get("OTEL_SERVICE_VERSION")
        or os.environ.get("SERVICE_VERSION", "0.0.0"),
        "env": os.environ.get("DEPLOYMENT_ENVIRONMENT")
        or os.environ.get("PROFILE", "dev"),
        "profile": os.environ.get("PROFILE", "dev"),
    }


def _tenant_from_headers(headers: Any) -> str:
    """Resolve tenant from X-Meridian-Tenant, X-Tenant-ID, or the
    (unverified) tenant_id JWT claim. Telemetry labelling only."""
    get = getattr(headers, "get", None)
    if get is None:
        return ""
    t = get("x-meridian-tenant") or get("X-Meridian-Tenant")
    if t:
        return t
    t = get("x-tenant-id") or get("X-Tenant-ID")
    if t:
        return t
    authz = get("authorization") or get("Authorization") or ""
    if authz.startswith("Bearer "):
        import base64
        import json

        parts = authz[7:].split(".")
        if len(parts) == 3:
            try:
                payload = json.loads(
                    base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4))
                )
                return str(payload.get("tenant_id", ""))
            except Exception:
                return ""
    return ""


def init_otel(app: Optional[Any] = None, *, tracer_provider=None) -> bool:
    """Initialise OTel and instrument a FastAPI/Flask app + HTTP clients.

    Returns True when OTLP export is active. Never raises: failures
    degrade to no-op with a logged warning.
    """
    cfg = _cfg()
    try:
        from opentelemetry import baggage, context, trace
        from opentelemetry.sdk.resources import Resource

        if tracer_provider is None:
            from opentelemetry.sdk.trace import TracerProvider

            tracer_provider = TracerProvider(
                resource=Resource.create(
                    {
                        "service.name": cfg["service"],
                        "service.version": cfg["version"],
                        "deployment.environment": cfg["env"],
                    }
                )
            )
            if cfg["endpoint"]:
                from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import (
                    OTLPSpanExporter,
                )
                from opentelemetry.sdk.trace.export import BatchSpanProcessor

                tracer_provider.add_span_processor(
                    BatchSpanProcessor(OTLPSpanExporter(endpoint=cfg["endpoint"]))
                )
            elif cfg["profile"] == "prod":
                log.warning(
                    "otel: PROFILE=prod but OTEL_EXPORTER_OTLP_ENDPOINT is unset; "
                    "service=%s running with NO telemetry export", cfg["service"],
                )
        trace.set_tracer_provider(tracer_provider)

        enabled = bool(cfg["endpoint"])

        if app is not None:
            module = type(app).__module__
            if module.startswith("fastapi"):
                from opentelemetry.instrumentation.fastapi import (
                    FastAPIInstrumentor,
                )

                FastAPIInstrumentor.instrument_app(app, tracer_provider=tracer_provider)
            elif module.startswith("flask"):
                from opentelemetry.instrumentation.flask import FlaskInstrumentor

                FlaskInstrumentor().instrument_app(app, tracer_provider=tracer_provider)
            else:
                log.warning("otel: unsupported app type %s; app not instrumented", module)

        # Client instrumentation (idempotent).
        try:
            from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor

            HTTPXClientInstrumentor().instrument(tracer_provider=tracer_provider)
        except Exception:
            pass
        try:
            from opentelemetry.instrumentation.requests import RequestsInstrumentor

            RequestsInstrumentor().instrument(tracer_provider=tracer_provider)
        except Exception:
            pass
        return enabled
    except Exception as exc:  # telemetry must never break startup
        log.warning("otel: init failed (%s); telemetry disabled", exc)
        return False


class TenantBaggageMiddleware:
    """ASGI middleware: extract tenant per request, stamp it on the active
    span as tenant.id and into baggage for downstream propagation."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        from opentelemetry import baggage, context, trace

        headers = {k.decode(): v.decode() for k, v in scope.get("headers", [])}
        tenant = _tenant_from_headers(headers)
        if tenant:
            span = trace.get_current_span()
            if span.is_recording():
                span.set_attribute(TENANT_KEY, tenant)
            ctx = baggage.set_baggage(TENANT_KEY, tenant)
            token = context.attach(ctx)
            try:
                await self.app(scope, receive, send)
            finally:
                context.detach(token)
        else:
            await self.app(scope, receive, send)


def tenant_attr(tenant: str) -> dict:
    """TenantAttr parity helper for manual spans/log records."""
    return {TENANT_KEY: tenant}
