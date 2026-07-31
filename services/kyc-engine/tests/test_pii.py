"""Unit + integration: PII pseudonymisation at rest (K5)."""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.adapters import pii
from kyc_engine.adapters.pii import PiiKeyMissing, protect_fields


def test_protect_fields_masks_and_vaults():
    fields = {"nin": "12345678901", "surname": "ADEYEMI", "first_name": "CHIAMAKA",
              "dob": "1990-05-14", "nin_format_ok": True, "_conf": 0.9}
    sanitized, vault = protect_fields(fields)
    assert sanitized["nin"] == "123****901"
    assert sanitized["nin_hmac"] == pii.hmac_pseudonym("12345678901")
    assert sanitized["surname"] == "A*****I"
    assert sanitized["dob"] == "****-**-**"
    assert sanitized["_pii_protected"] is True
    assert vault == {"nin": "12345678901", "surname": "ADEYEMI",
                     "first_name": "CHIAMAKA", "dob": "1990-05-14"}
    blob = str(sanitized)
    assert "12345678901" not in blob and "CHIAMAKA" not in blob and "1990-05-14" not in blob


def test_hmac_pseudonym_deterministic_and_distinct():
    a = pii.hmac_pseudonym("12345678901")
    assert a == pii.hmac_pseudonym("12345678901")
    assert a != pii.hmac_pseudonym("12345678902")
    assert len(a) == 64


def test_fail_closed_in_prod_without_key(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.delenv("PII_HMAC_KEY", raising=False)
    with pytest.raises(PiiKeyMissing):
        protect_fields({"nin": "12345678901"})


def test_pipeline_persists_no_raw_pii(env):
    """End-to-end: run_case persists masked fields + restricted vault only."""
    from kyc_engine.models.db import get_session
    from kyc_engine.models.tables import KycCase, KycDocument, KycExtraction
    from kyc_engine.pipeline.orchestrator import run_case
    from kyc_engine.pipeline.stage_ingest import ingest
    from tests.make_fixtures import nin_slip

    sess = get_session()
    case = KycCase(subject_type="individual", channel="api", status="created")
    sess.add(case)
    sess.commit()
    data = nin_slip("12345678901")
    doc = ingest(case.id, "nin.png", data, "nin_slip")
    sess.commit()
    case_id = case.id
    sess.close()

    run_case(case_id)

    sess = get_session()
    ext = sess.query(KycExtraction).filter_by(document_id=doc.id).one()
    assert ext.fields["nin"] == "123****901"
    assert "12345678901" not in str(ext.fields)
    assert ext.fields["_pii_protected"] is True
    # reversible lookup lives only in the restricted vault column
    assert ext.pii_vault["nin"] == "12345678901"
    assert ext.pii_vault["surname"] == "ADEYEMI"
    sess.close()
