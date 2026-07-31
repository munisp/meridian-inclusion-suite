"""Unit: retention — expired records anonymised, never deleted (K6)."""
from __future__ import annotations

import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.retention import RETENTION_FLAG, purge_expired


def _mk_case(env, age_days: int, subject_ref="SUBJ-001"):
    from kyc_engine.adapters import audit
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycDecision, KycDocument, KycExtraction
    sess = get_session()
    case = KycCase(subject_type="individual", channel="api",
                   subject_ref=subject_ref, status="approved")
    case.created_at = datetime.now(timezone.utc) - timedelta(days=age_days)
    sess.add(case)
    sess.flush()
    doc = KycDocument(case_id=case.id, doc_type="nin_slip", sha256="z" * 64,
                      minio_key="k", mime="image/png")
    sess.add(doc)
    sess.flush()
    sess.add(KycExtraction(document_id=doc.id,
                           fields={"nin": "123****901", "nin_hmac": "ab" * 32,
                                   "_pii_protected": True},
                           pii_vault={"nin": "12345678901", "surname": "ADEYEMI"}))
    sess.commit()
    audit.emit(case.id, "kyc.case.created.v1", {"seed": True}, session=sess)
    sess.commit()
    cid = case.id
    sess.close()
    return cid


def test_expired_records_anonymised_not_deleted(env):
    from kyc_engine.adapters import audit
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycExtraction
    cid = _mk_case(env, age_days=6 * 365)

    out = purge_expired()
    assert out["anonymised"] == 1
    assert out["retention_years"] == 5

    sess = get_session()
    case = sess.get(KycCase, cid)
    assert case is not None                     # record kept, not deleted
    assert RETENTION_FLAG in case.reason_codes
    assert case.subject_ref is not None and case.subject_ref.startswith("anon:")
    assert "SUBJ-001" not in str(case.subject_ref)
    ext = sess.query(KycExtraction).one()
    assert ext.fields == {"_anonymised": True, "_pii_protected": True}
    assert ext.pii_vault == {}
    # audit hash chain intact (incl. the retention event appended)
    events = audit.chain_for_case(cid, session=sess)
    assert any(e.event_type == "kyc.retention.anonymised.v1" for e in events)
    assert audit.verify_chain(events)
    sess.close()


def test_recent_records_untouched(env):
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycExtraction
    cid = _mk_case(env, age_days=30)
    assert purge_expired()["anonymised"] == 0
    sess = get_session()
    ext = sess.query(KycExtraction).one()
    assert ext.pii_vault["nin"] == "12345678901"
    sess.close()


def test_purge_idempotent(env):
    cid = _mk_case(env, age_days=6 * 365)
    assert purge_expired()["anonymised"] == 1
    assert purge_expired()["anonymised"] == 0


def test_retention_window_configurable(env, monkeypatch):
    monkeypatch.setenv("RETENTION_YEARS", "1")
    cid = _mk_case(env, age_days=400)
    assert purge_expired()["anonymised"] == 1
