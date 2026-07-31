"""Record retention: FATF R.11 >=5y floor reconciled with NDPA storage
limitation — expired records are ANONYMISED, never deleted, so the
hash-chained decision/audit evidence stays verifiable (retrievability).

``purge_expired`` (call from cron/scheduler) anonymises cases whose
created_at is older than ``retention_years`` (default 5):
- extraction ``fields`` -> {"_anonymised": True}; ``pii_vault`` purged
- case ``subject_ref`` -> one-way tombstone (HMAC pseudonym prefix)
- a hash-chained ``kyc.retention.anonymised.v1`` event is appended per case
- KycDecision / AuditEvent rows are NEVER touched (chain integrity kept)
"""
from __future__ import annotations

import hashlib
from datetime import datetime, timedelta, timezone
from typing import Any

from sqlalchemy import select

from .adapters import audit
from .config import get_settings
from .models.db import get_session
from .models.tables import KycCase, KycDocument, KycExtraction

RETENTION_FLAG = "RETENTION_ANONYMISED"


def _tombstone(subject_ref: str | None) -> str | None:
    if not subject_ref:
        return None
    return "anon:" + hashlib.sha256(subject_ref.encode()).hexdigest()[:16]


def purge_expired(now: datetime | None = None) -> dict[str, Any]:
    """Anonymise records past the retention window. Idempotent."""
    s = get_settings()
    now = now or datetime.now(timezone.utc)
    cutoff = now - timedelta(days=365 * s.retention_years)
    sess = get_session()
    anonymised = 0
    try:
        cases = sess.execute(select(KycCase)).scalars().all()
        for case in cases:
            ts = case.created_at
            if ts is None:
                continue
            if ts.tzinfo is None:
                ts = ts.replace(tzinfo=timezone.utc)
            if ts > cutoff:
                continue
            if RETENTION_FLAG in (case.reason_codes or []):
                continue  # idempotent
            exts = sess.execute(
                select(KycExtraction).join(KycDocument)
                .where(KycDocument.case_id == case.id)).scalars().all()
            for ext in exts:
                ext.fields = {"_anonymised": True, "_pii_protected": True}
                ext.pii_vault = {}
            case.subject_ref = _tombstone(case.subject_ref)
            case.reason_codes = list(case.reason_codes or []) + [RETENTION_FLAG]
            audit.emit(case.id, "kyc.retention.anonymised.v1",
                       {"retention_years": s.retention_years,
                        "extractions_anonymised": len(exts)}, session=sess)
            anonymised += 1
        sess.commit()
        return {"anonymised": anonymised, "retention_years": s.retention_years}
    finally:
        sess.close()
