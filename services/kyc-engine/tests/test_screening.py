"""Unit: sanctions/PEP screening — fuzzy matching, stage, decision wiring (K2)."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.adapters.screening import (OfflineListScreening,
                                           name_similarity, normalize_name)
from kyc_engine.pipeline import stage_decision
from kyc_engine.pipeline.stage_screening import run_screening


def test_normalize_and_similarity():
    assert normalize_name("  Example-Sanctioned, INDIVIDUAL ") == "example sanctioned individual"
    assert name_similarity("EXAMPLE SANCTIONED INDIVIDUAL",
                           "EXAMPLE SANCTIONED INDIVIDUAL") == 1.0
    # 1-edit fuzzy drift still scores high
    assert name_similarity("EXAMPLE SANCTIONED INDIVIDUL",
                           "EXAMPLE SANCTIONED INDIVIDUAL") > 0.9
    # token overlap: reordered names
    assert name_similarity("INDIVIDUAL SANCTIONED EXAMPLE",
                           "EXAMPLE SANCTIONED INDIVIDUAL") == 1.0
    assert name_similarity("CHIAMAKA ADEYEMI", "EXAMPLE SANCTIONED INDIVIDUAL") < 0.5


def test_offline_list_sanctions_hit_sim_tagged():
    scr = OfflineListScreening()
    out = scr.screen("Example Sanctioned Individual")
    assert out["sanctions_hit"] is True
    assert out["matches"][0]["list"] == "OFAC_SDN_SAMPLE"
    assert out["sim"] is True            # honestly tagged [SIM]
    assert out["provider"] == "offline-sample"


def test_offline_list_nga_local_hit():
    out = OfflineListScreening().screen("EXAMPLE LOCAL WATCHLIST ENTRY")
    assert out["sanctions_hit"] is True
    assert out["matches"][0]["list"] == "NGA_LOCAL_SAMPLE"


def test_offline_list_pep_hit_not_sanctions():
    out = OfflineListScreening().screen("Example Politically Exposed Person")
    assert out["pep_hit"] is True
    assert out["sanctions_hit"] is False


def test_dob_disambiguation():
    # same-ish name but different DOB -> not the listed person
    out = OfflineListScreening().screen("EXAMPLE SANCTIONED INDIVIDUAL",
                                        dob="1990-05-14")
    assert out["sanctions_hit"] is False
    out = OfflineListScreening().screen("EXAMPLE SANCTIONED INDIVIDUAL",
                                        dob="1975-03-11")
    assert out["sanctions_hit"] is True


def test_clean_name_no_match():
    out = OfflineListScreening().screen("CHIAMAKA ADEYEMI")
    assert out["screened"] is True
    assert out["matches"] == []
    assert out["sanctions_hit"] is False and out["pep_hit"] is False


def test_stage_screens_subject_and_directors():
    fields = {"surname": "ADEYEMI", "first_name": "CHIAMAKA",
              "directors": ["EXAMPLE SANCTIONED INDIVIDUAL"]}
    out = run_screening(fields)
    assert out["sanctions_hit"] is True
    assert out["matches"][0]["screened_name"] == "EXAMPLE SANCTIONED INDIVIDUAL"
    assert "ADEYEMI CHIAMAKA" in out["names"]


def test_decision_sanctions_hit_hard_reject():
    checks = [{"kind": "ocr", "score": 1.0, "passed": True, "sim": False},
              {"kind": "screening", "score": 0.0, "passed": False, "sim": False,
               "hard_fail": True, "reason": "SANCTIONS_HIT"}]
    dec = stage_decision.decide(checks, "individual")
    assert dec["verdict"] == "reject"
    assert "SANCTIONS_HIT" in dec["reasons"]


def test_decision_pep_does_not_reject():
    checks = [{"kind": "ocr", "score": 1.0, "passed": True, "sim": False},
              {"kind": "screening", "score": 1.0, "passed": True, "sim": False,
               "pep_hit": True}]
    dec = stage_decision.decide(checks, "individual")
    assert dec["verdict"] != "reject"
