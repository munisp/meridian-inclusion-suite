"""Unit: KYB UBO extraction + CAC registry cross-check."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.adapters.cac_registry import CacRegistrySim
from kyc_engine.pipeline.stage_kyb import run_kyb
from tests.make_fixtures import valid_rc


def test_sim_registry_deterministic():
    reg = CacRegistrySim()
    rc = valid_rc("123456")
    a, b = reg.lookup(rc), reg.lookup(rc)
    assert a == b
    assert a["sim"] is True


def test_ubo_over_25pct_extracted(monkeypatch):
    import kyc_engine.pipeline.stage_kyb as kyb
    class FakeReg:
        sim = True
        def lookup(self, rc):
            return {"rc_number": rc, "company_name": "X", "status": "active",
                    "directors": [{"name": "A", "ownership_pct": 60.0},
                                  {"name": "B", "ownership_pct": 40.0}],
                    "sim": True}
    monkeypatch.setattr(kyb, "get_registry", lambda: FakeReg())
    out = run_kyb({"rc_number": "RC1234562", "rc_format_ok": True})
    assert [u["name"] for u in out["ubo"]] == ["A", "B"]


def test_ubo_under_threshold_excluded(monkeypatch):
    # default threshold is 5% (CAMA PSC); strictly-greater semantics: exactly
    # 5.0% is NOT a PSC, 5.01% is.
    import kyc_engine.pipeline.stage_kyb as kyb
    class FakeReg:
        sim = True
        def lookup(self, rc):
            return {"rc_number": rc, "company_name": "X",
                    "directors": [{"name": "A", "ownership_pct": 5.0},
                                  {"name": "B", "ownership_pct": 3.0}],
                    "sim": True}
    monkeypatch.setattr(kyb, "get_registry", lambda: FakeReg())
    out = run_kyb({"rc_number": "RC1234562", "rc_format_ok": True})
    assert out["ubo"] == []


def test_ubo_just_over_5pct_included(monkeypatch):
    import kyc_engine.pipeline.stage_kyb as kyb
    class FakeReg:
        sim = True
        def lookup(self, rc):
            return {"rc_number": rc, "company_name": "X",
                    "directors": [{"name": "A", "ownership_pct": 5.01}],
                    "sim": True}
    monkeypatch.setattr(kyb, "get_registry", lambda: FakeReg())
    out = run_kyb({"rc_number": "RC1234562", "rc_format_ok": True})
    assert [u["name"] for u in out["ubo"]] == ["A"]


def test_rc_format_fail_issue():
    out = run_kyb({"rc_number": "RC1234560", "rc_format_ok": False})
    assert "rc_format_fail" in out["issues"]


def test_missing_rc_issue():
    out = run_kyb({"rc_number": None, "rc_format_ok": False})
    assert "rc_number_missing" in out["issues"]
