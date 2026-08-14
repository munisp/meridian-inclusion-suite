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

from sqlalchemy import func, select
from sqlalchemy.orm import selectinload

from .adapters import audit
from .adapters.audit import GENESIS
from .adapters.screening import get_screening_provider
from .config import get_settings
from .models.db import get_session
from .models.tables import KycCase, KycCheck, KycDocument


def _last_screening(checks: list[KycCheck]) -> KycCheck | None:
    """Latest screening check from an eagerly-loaded collection (F-10: no
    per-case query)."""
    screenings = [c for c in checks if c.kind == "screening"]
    if not screenings:
        return None
    return max(screenings, key=lambda c: (c.created_at or datetime.min).replace(tzinfo=None))


def _vault_names(case: KycCase) -> list[tuple[str, str | None]]:
    """Names to screen from the eagerly-loaded documents/extractions
    (F-10: no per-case query)."""
    names: list[tuple[str, str | None]] = []
    for doc in case.documents:
        ext = doc.extraction
        if ext is None:
            continue
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
        # F-10: single query with selectin eager loads (checks + documents +
        # extractions) instead of 2 SQL queries per case. The external
        # provider.screen() call stays per-name — the screening provider API
        # has no batch endpoint today.
        cases = sess.execute(
            select(KycCase).where(KycCase.status == "approved")
            .options(selectinload(KycCase.checks),
                     selectinload(KycCase.documents)
                     .selectinload(KycDocument.extraction))).scalars().all()
        # F-10: one query for all audit-chain heads (prev_hash) instead of a
        # per-case SELECT inside audit.emit.
        head = (select(audit.AuditEvent.case_id.label("cid"),
                       func.max(audit.AuditEvent.created_at).label("m"))
                .group_by(audit.AuditEvent.case_id).subquery())
        prev_by_case = dict(sess.execute(
            select(audit.AuditEvent.case_id, audit.AuditEvent.hash)
            .join(head, (audit.AuditEvent.case_id == head.c.cid)
                  & (audit.AuditEvent.created_at == head.c.m))).all())
        for case in cases:
            last = _last_screening(case.checks)
            if last is not None and last.created_at:
                ts = last.created_at
                if ts.tzinfo is None:
                    ts = ts.replace(tzinfo=timezone.utc)
                if ts > cutoff:
                    continue  # screened recently enough
            matches: list[dict[str, Any]] = []
            for name, dob in _vault_names(case):
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
                        "pep_hit": pep_hit}, session=sess,
                       prev_hash=prev_by_case.get(case.id, GENESIS))
            if sanctions_hit:
                case.status = "enhanced_review"
                case.reason_codes = list(case.reason_codes or []) + ["RESCREEN_SANCTIONS_HIT"]
                hits += 1
            rescreened += 1
        sess.commit()
        return {"rescreened": rescreened, "hits": hits}
    finally:
        sess.close()
