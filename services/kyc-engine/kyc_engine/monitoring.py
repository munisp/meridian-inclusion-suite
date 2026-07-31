"""Ongoing monitoring: scheduled re-screening hook (FATF R.10(d)).

``rescreen_due`` re-screens approved customers whose latest screening check
is older than ``monitoring_rescreen_interval_days`` (env-configurable; 0
disables). Every re-check is recorded: a new ``screening`` KycCheck with
``rescreen: True`` plus a hash-chained ``kyc.rescreen.v1`` audit event — a
hit is never silent: sanctions match flips the case to enhanced_review.

Names come from the restricted ``pii_vault`` column (legitimate-processing
lookup); masked ``fields`` are never used for matching.

Scheduling: call ``rescreen_due`` from a cron/scheduler (or the Temporal
worker) at the configured cadence.
"""
from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

from sqlalchemy import select

from .adapters import audit
from .adapters.screening import get_screening_provider
from .config import get_settings
from .models.db import get_session
from .models.tables import KycCase, KycCheck, KycDocument, KycExtraction


def _last_screening(sess, case_id: str) -> KycCheck | None:
    return sess.execute(
        select(KycCheck).where(KycCheck.case_id == case_id,
                               KycCheck.kind == "screening")
        .order_by(KycCheck.created_at.desc())).scalars().first()


def _vault_names(sess, case_id: str) -> list[tuple[str, str | None]]:
    names: list[tuple[str, str | None]] = []
    exts = sess.execute(
        select(KycExtraction).join(KycDocument)
        .where(KycDocument.case_id == case_id)).scalars().all()
    for ext in exts:
        v = ext.pii_vault or {}
        subject = " ".join(p for p in (v.get("surname"), v.get("first_name")) if p)
        if subject:
            names.append((subject, v.get("dob")))
        for d in (ext.fields or {}).get("directors") or []:  # registry data, not PII
            names.append((str(d), None))
    return names


def rescreen_due(now: datetime | None = None) -> dict[str, Any]:
    """Re-screen approved cases past the configured interval. Returns a
    summary {rescreened, hits}. Never silent: every case gets a recorded
    re-check event."""
    s = get_settings()
    if s.monitoring_rescreen_interval_days <= 0:
        return {"rescreened": 0, "hits": 0}
    now = now or datetime.now(timezone.utc)
    cutoff = now - timedelta(days=s.monitoring_rescreen_interval_days)
    provider = get_screening_provider()
    sess = get_session()
    rescreened = hits = 0
    try:
        cases = sess.execute(
            select(KycCase).where(KycCase.status == "approved")).scalars().all()
        for case in cases:
            last = _last_screening(sess, case.id)
            if last is not None and last.created_at:
                ts = last.created_at
                if ts.tzinfo is None:
                    ts = ts.replace(tzinfo=timezone.utc)
                if ts > cutoff:
                    continue  # screened recently enough
            matches: list[dict[str, Any]] = []
            for name, dob in _vault_names(sess, case.id):
                res = provider.screen(name, dob=dob)
                matches.extend(res.get("matches", []))
            sanctions_hit = any(m["kind"] == "sanctions" for m in matches)
            pep_hit = any(m["kind"] == "pep" for m in matches)
            chk = KycCheck(case_id=case.id, kind="screening",
                           score=0.0 if sanctions_hit else 1.0,
                           passed=not sanctions_hit,
                           detail={"screened": True, "rescreen": True,
                                   "matches": matches, "sanctions_hit": sanctions_hit,
                                   "pep_hit": pep_hit,
                                   "provider": getattr(provider, "provider", "unknown")},
                           sim=bool(getattr(provider, "sim", True)))
            sess.add(chk)
            audit.emit(case.id, "kyc.rescreen.v1",
                       {"matches": len(matches), "sanctions_hit": sanctions_hit,
                        "pep_hit": pep_hit}, session=sess)
            if sanctions_hit:
                case.status = "enhanced_review"
                case.reason_codes = list(case.reason_codes or []) + ["RESCREEN_SANCTIONS_HIT"]
                hits += 1
            rescreened += 1
        sess.commit()
        return {"rescreened": rescreened, "hits": hits}
    finally:
        sess.close()
