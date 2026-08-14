"""F-10: rescreen_due must not issue per-case SQL (N+1 regression guard).

With N approved cases each holding a document+extraction and a prior
screening check, the number of SELECT statements stays constant regardless
of case count (one for cases + one per selectin-loaded relationship), versus
2N+1 before the fix.
"""
from datetime import datetime, timedelta, timezone

import pytest
from sqlalchemy import event


def _seed_approved_cases(sess, n: int) -> None:
    from kyc_engine.models.tables import (KycCase, KycCheck, KycDocument,
                                          KycExtraction)
    old = datetime.now(timezone.utc) - timedelta(days=120)
    for i in range(n):
        case = KycCase(subject_type="individual", channel="api",
                       status="approved")
        sess.add(case)
        sess.flush()
        doc = KycDocument(case_id=case.id, doc_type="nin_slip",
                          sha256=f"{i}" * 64, minio_key="k", mime="image/png")
        sess.add(doc)
        sess.flush()
        sess.add(KycExtraction(document_id=doc.id,
                               fields={"_pii_protected": True},
                               pii_vault={"surname": f"TEST-{i}",
                                          "first_name": "PERSON"}))
        chk = KycCheck(case_id=case.id, kind="screening", score=1.0,
                       passed=True, detail={"screened": True}, sim=True)
        chk.created_at = old
        sess.add(chk)
    sess.commit()


def _count_selects(env, n: int) -> int:
    from kyc_engine.models.db import get_engine, get_session
    from kyc_engine.monitoring import rescreen_due

    sess = get_session()
    _seed_approved_cases(sess, n)
    sess.close()

    selects = 0

    def before_cursor_execute(conn, cursor, statement, parameters, context,
                              executemany):
        nonlocal selects
        if statement.lstrip().upper().startswith("SELECT"):
            selects += 1

    eng = get_engine()
    event.listen(eng, "before_cursor_execute", before_cursor_execute)
    try:
        out = rescreen_due()
        assert out["rescreened"] == n
    finally:
        event.remove(eng, "before_cursor_execute", before_cursor_execute)
    return selects


@pytest.mark.parametrize("n", [3, 9])
def test_rescreen_due_constant_query_count(env, n):
    """SELECT count must not grow with case count (no N+1)."""
    selects = _count_selects(env, n)
    # cases + selectin(checks) + selectin(documents) + selectin(extractions)
    # + audit/event lookups — all constant; pre-fix this was 2N+1.
    assert selects <= 12, f"{n} cases caused {selects} SELECTs (N+1?)"
