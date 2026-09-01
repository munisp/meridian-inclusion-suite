"""Span smoke tests for app.otel (DESIGN-CONTRACT parity): tenant.id on the
server span, fail-soft init, and the PROFILE=prod loud warning."""
from __future__ import annotations

import base64
import json

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from app import otel


@pytest.fixture()
def recorder():
    exp = InMemorySpanExporter()
    tp = TracerProvider()
    tp.add_span_processor(SimpleSpanProcessor(exp))
    yield exp, tp
    exp.clear()


def _jwt(tenant: str) -> str:
    def b64(o):
        return base64.urlsafe_b64encode(json.dumps(o).encode()).rstrip(b"=").decode()

    return f"Bearer {b64({'alg': 'none'})}.{b64({'tenant_id': tenant})}.sig"


def test_fastapi_span_has_tenant(recorder, monkeypatch):
    exp, tp = recorder
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    monkeypatch.setenv("PROFILE", "dev")
    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    app = FastAPI()

    @app.get("/v1/pack/{ref}")
    def get_pack(ref: str):
        return {"ref": ref}

    # baggage first, then init_otel adds the server-span middleware outermost
    app.add_middleware(otel.TenantBaggageMiddleware)
    assert otel.init_otel(app, tracer_provider=tp) is False  # no endpoint

    client = TestClient(app)
    r = client.get("/v1/pack/rp-education-ng", headers={"X-Meridian-Tenant": "tenant-edu-3"})
    assert r.status_code == 200

    spans = exp.get_finished_spans()
    assert spans, "no spans recorded"
    server = [s for s in spans if s.kind == trace.SpanKind.SERVER]
    assert server
    tenants = {s.attributes.get("tenant.id") for s in server if s.attributes}
    assert "tenant-edu-3" in tenants


def test_tenant_from_jwt_claim():
    assert otel._tenant_from_headers({"Authorization": _jwt("tenant-jwt-5")}) == "tenant-jwt-5"
    assert otel._tenant_from_headers({"X-Meridian-Tenant": "t1"}) == "t1"
    assert otel._tenant_from_headers({}) == ""


def test_prod_without_endpoint_warns(monkeypatch, caplog):
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.setenv("PROFILE", "prod")
    with caplog.at_level("WARNING", logger="education.otel"):
        enabled = otel.init_otel(None, tracer_provider=None)
    assert enabled is False
    assert any("OTEL_EXPORTER_OTLP_ENDPOINT" in r.message for r in caplog.records)


def test_disabled_mode_never_raises(monkeypatch):
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.setenv("PROFILE", "dev")
    assert otel.init_otel(object(), tracer_provider=None) is False
