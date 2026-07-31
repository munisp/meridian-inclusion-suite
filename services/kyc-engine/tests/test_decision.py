"""Unit: decision engine thresholds (SPEC A §5/§6)."""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline.stage_decision import decide


def chk(kind, score, passed=True, sim=False, degraded=False, hard=False, reason=None):
    d = {"kind": kind, "score": score, "passed": passed, "sim": sim,
         "degraded": degraded, "hard_fail": hard}
    if reason:
        d["reason"] = reason
    return d


GOOD = [chk("ocr", 0.95), chk("forensics", 0.95), chk("face_match", 0.9),
        chk("liveness", 1.0)]


def test_approve_when_all_strong():
    d = decide(GOOD, "individual")
    assert d["verdict"] == "approve"
    assert d["score"] >= 70


def test_step_up_mid_score():
    d = decide([chk("ocr", 0.5), chk("forensics", 0.5), chk("face_match", 0.5),
                chk("liveness", 0.5)], "individual")
    assert d["verdict"] == "step_up"


def test_reject_low_score():
    d = decide([chk("ocr", 0.1), chk("forensics", 0.1), chk("face_match", 0.1),
                chk("liveness", 0.0)], "individual")
    assert d["verdict"] == "reject"


def test_hard_fail_forces_reject():
    d = decide(GOOD + [chk("forensics", 1.0, hard=True, reason="FORENSICS_TAMPER")],
               "individual")
    assert d["verdict"] == "reject"
    assert "FORENSICS_TAMPER" in d["reasons"]


def test_degraded_caps_at_step_up():
    d = decide([chk("ocr", 0.95), chk("forensics", 0.95, degraded=True),
                chk("face_match", 0.9), chk("liveness", 1.0)], "individual")
    assert d["verdict"] == "step_up"
    assert "DEGRADED_CAP" in d["reasons"]


def test_sim_no_auto_approve(monkeypatch):
    monkeypatch.setenv("ALLOW_SIM_APPROVE", "false")
    d = decide([chk("ocr", 0.95, sim=True), chk("forensics", 0.95),
                chk("face_match", 0.9), chk("liveness", 1.0)], "individual")
    assert d["verdict"] == "step_up"
    assert "SIM_NO_AUTO_APPROVE" in d["reasons"]


def test_sim_approve_allowed_when_configured(monkeypatch):
    monkeypatch.setenv("ALLOW_SIM_APPROVE", "true")
    d = decide([chk("ocr", 0.95, sim=True), chk("forensics", 0.95),
                chk("face_match", 0.9), chk("liveness", 1.0)], "individual")
    assert d["verdict"] == "approve"


def test_unknown_doctype_never_auto_approves():
    d = decide(GOOD, "individual", unknown_doctype=True)
    assert d["verdict"] == "step_up"
    assert "UNKNOWN_DOCTYPE" in d["reasons"]
