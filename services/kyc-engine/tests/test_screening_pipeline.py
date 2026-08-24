"""Regression: B4-1 — sanctions/PEP screening must actually run in the pipeline.

Pre-fix, ``stage_screening.run_screening`` had zero call sites: the pipeline
never executed screening, never recorded a ``screening`` check, yet decision
assembly claimed a screening verdict. These tests prove:
1. processing a case records a screening check (screening executed);
2. a sanctions hit hard-fails the case to ``reject`` with SANCTIONS_HIT;
3. a screener outage fails CLOSED (DLQ + reject — never a silent pass).
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from tests.make_fixtures import nin_slip

AGENT = {"X-Dev-Role": "kyc.agent"}


class _HitProvider:
    sim = False
    provider = "test-hit"

    def screen(self, name, dob=None):
        return {"matches": [{"list": "OFAC_SDN_SAMPLE", "kind": "sanctions",
                             "name": name, "score": 0.99, "program": "TEST"}],
                "sanctions_hit": True, "pep_hit": False, "sim": False}


class _DownProvider:
    sim = False
    provider = "test-down"

    def screen(self, name, dob=None):
        raise ConnectionError("screening provider unreachable")


@pytest.fixture()
def _provider(monkeypatch):
    from kyc_engine.pipeline import stage_screening

    def set_provider(p):
        monkeypatch.setattr(stage_screening, "get_screening_provider", lambda: p)
    return set_provider


def _process_case(client):
    cid = client.post("/v1/cases", json={"subject_type": "individual",
                                         "channel": "api"}, headers=AGENT).json()["case_id"]
    client.put(f"/v1/cases/{cid}/documents",
               files={"file": ("doc.png", nin_slip(), "image/png")},
               data={"doc_type": "nin_slip"}, headers=AGENT)
    r = client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    assert r.status_code == 202
    return client.get(f"/v1/cases/{cid}", headers=AGENT).json()


def test_pipeline_executes_and_records_screening(client):
    case = _process_case(client)
    checks = [c for c in case["checks"] if c["kind"] == "screening"]
    assert checks, f"no screening check recorded: {[c['kind'] for c in case['checks']]}"
    assert checks[0]["detail"]["screened"] is True


def test_pipeline_sanctions_hit_hard_rejects(client, _provider):
    _provider(_HitProvider())
    case = _process_case(client)
    assert case["decision"] == "reject", case
    assert "SANCTIONS_HIT" in case["reasons"]
    assert case["status"] == "rejected"


def test_pipeline_screener_outage_fails_closed(client, _provider):
    _provider(_DownProvider())
    case = _process_case(client)
    # screener down must never silently pass: DLQ + reject, not approve/step_up
    assert case["decision"] == "reject", case
    assert "PIPELINE_DLQ" in case["reasons"]
    assert case["status"] == "failed"
