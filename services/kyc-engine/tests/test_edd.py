"""Unit + integration: EDD triggers + ongoing monitoring re-screening (K4)."""
from __future__ import annotations

import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline import stage_decision


def test_pep_flag_caps_approve_at_enhanced_review():
    checks = [{"kind": "ocr", "score": 1.0, "passed": True, "sim": False}]
    dec = stage_decision.decide(checks, "individual", risk_flags=["PEP_MATCH"])
    assert dec["verdict"] == "enhanced_review"
    assert "PEP_MATCH" in dec["reasons"]


def test_no_flags_still_approves():
    checks = [{"kind": "ocr", "score": 1.0, "passed": True, "sim": False}]
    assert stage_decision.decide(checks, "individual")["verdict"] == "approve"


def test_flags_do_not_override_reject():
    checks = [{"kind": "ocr", "score": 0.0, "passed": False, "sim": False}]
    dec = stage_decision.decide(checks, "individual", risk_flags=["PEP_MATCH"])
    assert dec["verdict"] == "reject"


def _case_with_check(env, channel="api", declared=None, pep=False):
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycCheck
    sess = get_session()
    case = KycCase(subject_type="individual", channel=channel,
                   declared_value=declared, status="processing")
    sess.add(case)
    sess.flush()
    detail = {"screened": True, "matches": [], "sanctions_hit": False,
              "pep_hit": pep}
    sess.add(KycCheck(case_id=case.id, kind="ocr", score=1.0, passed=True,
                      detail={}, sim=False))
    sess.add(KycCheck(case_id=case.id, kind="screening",
                      score=0.0 if False else 1.0, passed=True, detail=detail,
                      sim=False))
    sess.commit()
    cid = case.id
    sess.close()
    return cid


def test_evaluate_decision_pep_and_non_f2f(env):
    from kyc_engine.pipeline.orchestrator import evaluate_decision
    cid = _case_with_check(env, channel="api", pep=True)
    dec = evaluate_decision(cid)
    assert dec["verdict"] == "enhanced_review"
    assert "PEP_MATCH" in dec["reasons"]
    assert "NON_FACE_TO_FACE" in dec["reasons"]


def test_evaluate_decision_f2f_clean_approves(env):
    from kyc_engine.pipeline.orchestrator import evaluate_decision
    cid = _case_with_check(env, channel="selfie", pep=False)
    assert evaluate_decision(cid)["verdict"] == "approve"


def test_high_value_trigger(env, monkeypatch):
    from kyc_engine.pipeline.orchestrator import evaluate_decision
    monkeypatch.setenv("EDD_HIGH_VALUE_THRESHOLD", "1000000")
    cid = _case_with_check(env, channel="selfie", declared=5_000_000)
    dec = evaluate_decision(cid)
    assert dec["verdict"] == "enhanced_review"
    assert "HIGH_VALUE" in dec["reasons"]


def test_rescreen_due_records_recheck(env, monkeypatch):
    from kyc_engine.adapters import audit
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycCheck, KycDocument, KycExtraction
    from kyc_engine.monitoring import rescreen_due

    sess = get_session()
    case = KycCase(subject_type="individual", channel="api", status="approved")
    sess.add(case)
    sess.flush()
    doc = KycDocument(case_id=case.id, doc_type="nin_slip", sha256="x" * 64,
                      minio_key="k", mime="image/png")
    sess.add(doc)
    sess.flush()
    # restricted vault holds the raw name for legitimate re-screening
    sess.add(KycExtraction(document_id=doc.id,
                           fields={"_pii_protected": True},
                           pii_vault={"surname": "EXAMPLE POLITICALLY EXPOSED",
                                      "first_name": "PERSON"}))
    old = datetime.now(timezone.utc) - timedelta(days=120)
    chk = KycCheck(case_id=case.id, kind="screening", score=1.0, passed=True,
                   detail={"screened": True, "matches": []}, sim=True)
    chk.created_at = old
    sess.add(chk)
    sess.commit()
    cid = case.id
    sess.close()

    out = rescreen_due()
    assert out["rescreened"] == 1
    sess = get_session()
    checks = [c for c in sess.query(KycCheck).filter_by(case_id=cid, kind="screening")]
    assert len(checks) == 2
    recheck = [c for c in checks if c.detail.get("rescreen")]
    assert recheck and recheck[0].detail["pep_hit"] is True
    # PEP on rescreen: recorded, case NOT auto-rejected; audit event emitted
    events = audit.chain_for_case(cid, session=sess)
    assert any(e.event_type == "kyc.rescreen.v1" for e in events)
    assert audit.verify_chain(events)
    sess.close()


def test_rescreen_skips_recently_screened(env):
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycCheck
    from kyc_engine.monitoring import rescreen_due
    sess = get_session()
    case = KycCase(subject_type="individual", channel="api", status="approved")
    sess.add(case)
    sess.flush()
    sess.add(KycCheck(case_id=case.id, kind="screening", score=1.0, passed=True,
                      detail={"screened": True, "matches": []}, sim=True))
    sess.commit()
    cid = case.id
    sess.close()
    assert rescreen_due()["rescreened"] == 0


def test_rescreen_sanctions_hit_flips_to_enhanced_review(env):
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycDocument, KycExtraction
    from kyc_engine.monitoring import rescreen_due
    sess = get_session()
    case = KycCase(subject_type="individual", channel="api", status="approved")
    sess.add(case)
    sess.flush()
    doc = KycDocument(case_id=case.id, doc_type="nin_slip", sha256="y" * 64,
                      minio_key="k2", mime="image/png")
    sess.add(doc)
    sess.flush()
    sess.add(KycExtraction(document_id=doc.id, fields={"_pii_protected": True},
                           pii_vault={"surname": "EXAMPLE SANCTIONED",
                                      "first_name": "INDIVIDUAL"}))
    sess.commit()
    cid = case.id
    sess.close()
    out = rescreen_due()
    assert out["hits"] == 1
    sess = get_session()
    case = sess.get(KycCase, cid)
    assert case.status == "enhanced_review"
    assert "RESCREEN_SANCTIONS_HIT" in case.reason_codes
    sess.close()
