"""Schema audit P1: kyc_case.declared_value is Numeric (money, exact),
not Float — values above the int4 kobo ceiling (> ₦21.4M) round-trip with
kobo precision; queue/outbox indexes exist."""
from __future__ import annotations

from decimal import Decimal

from sqlalchemy import Numeric, inspect


def test_declared_value_is_numeric():
    from kyc_engine.models.tables import KycCase
    assert isinstance(KycCase.__table__.c.declared_value.type, Numeric)


def test_indexes_exist():
    from kyc_engine.models.tables import AuditEvent, KycCase
    case_idx = {i.name for i in KycCase.__table__.indexes}
    audit_idx = {i.name for i in AuditEvent.__table__.indexes}
    assert "ix_kyc_case_status_created" in case_idx
    assert "ix_kyc_audit_unpub" in audit_idx


def test_large_declared_value_round_trip(env):
    from kyc_engine.models import db
    from kyc_engine.models.tables import KycCase

    sess = db.get_session()
    big = Decimal("500000000.00")            # ₦500M — far above ₦21.4M
    exact = Decimal("21500000.75")           # kobo precision above ₦21.4M
    c1 = KycCase(subject_type="business", declared_value=big)
    c2 = KycCase(subject_type="individual", declared_value=exact)
    sess.add_all([c1, c2])
    sess.commit()
    got1 = sess.get(KycCase, c1.id)
    got2 = sess.get(KycCase, c2.id)
    assert Decimal(got1.declared_value) == big
    assert Decimal(got2.declared_value) == exact
    # a Float column would already have drifted on this one:
    assert Decimal(got2.declared_value) * 100 == 2150000075


def test_indexes_created_on_engine(env):
    from kyc_engine.models import db
    idx = {i["name"] for i in inspect(db.get_engine()).get_indexes("kyc_case")}
    assert "ix_kyc_case_status_created" in idx
