"""Audit evidence: hash-chained outbox -> Kafka topic kyc.evidence.v1.

Every pipeline event is appended to the audit_event outbox table with a
SHA-256 chain (prev_hash || payload). A relay drains unpublished rows to
Kafka; in dev (no kafka_bootstrap) the drain is a no-op in-proc marker.
"""
from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone

from sqlalchemy import select

from ..config import get_settings
from ..models.db import get_session
from ..models.tables import AuditEvent

GENESIS = "0" * 64


def canonical(payload: dict) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str)


def compute_hash(prev_hash: str, event_type: str, payload: dict, ts: str) -> str:
    h = hashlib.sha256()
    h.update(prev_hash.encode())
    h.update(event_type.encode())
    h.update(canonical(payload).encode())
    h.update(ts.encode())
    return h.hexdigest()


def emit(case_id: str, event_type: str, payload: dict, session=None,
         prev_hash: str | None = None) -> AuditEvent:
    """Append a hash-chained evidence event to the outbox.

    prev_hash lets batch callers (e.g. monitoring.rescreen_due, F-10) supply
    a prefetched chain head instead of one SELECT per event."""
    own = session is None
    sess = session or get_session()
    try:
        if prev_hash is not None:
            prev = prev_hash
        else:
            last = sess.execute(
                select(AuditEvent).where(AuditEvent.case_id == case_id)
                .order_by(AuditEvent.created_at.desc())
            ).scalars().first()
            prev = last.hash if last else GENESIS
        ts = datetime.now(timezone.utc).isoformat()
        ev = AuditEvent(
            case_id=case_id, event_type=event_type, payload=payload,
            prev_hash=prev, hash=compute_hash(prev, event_type, payload, ts),
        )
        # store the exact ts used for hashing so chain verification is stable
        ev.created_at = datetime.fromisoformat(ts)
        sess.add(ev)
        if own:
            sess.commit()
        return ev
    finally:
        if own:
            sess.close()


def _ts_iso(dt) -> str:
    """Normalize DB datetimes (SQLite returns naive) to the exact ISO string
    used at emit time (UTC, +00:00 suffix)."""
    ts = dt.isoformat()
    if "+" not in ts and not ts.endswith("Z"):
        ts += "+00:00"
    return ts


def verify_chain(events: list[AuditEvent]) -> bool:
    prev = GENESIS
    for ev in events:
        ts = _ts_iso(ev.created_at)
        if ev.prev_hash != prev:
            return False
        if ev.hash != compute_hash(prev, ev.event_type, ev.payload, ts):
            return False
        prev = ev.hash
    return True


def chain_for_case(case_id: str, session=None) -> list[AuditEvent]:
    own = session is None
    sess = session or get_session()
    try:
        return list(sess.execute(
            select(AuditEvent).where(AuditEvent.case_id == case_id)
            .order_by(AuditEvent.created_at.asc())
        ).scalars().all())
    finally:
        if own:
            sess.close()


def drain_outbox() -> int:
    """Relay unpublished outbox rows to Kafka kyc.evidence.v1 (at-least-once
    from the outbox; the topic consumer applies idempotent keys). Dev mode
    (no kafka_bootstrap) marks rows published in-proc."""
    s = get_settings()
    sess = get_session()
    n = 0
    try:
        rows = sess.execute(select(AuditEvent).where(AuditEvent.published.is_(False))).scalars().all()
        producer = None
        if s.kafka_bootstrap:
            from kafka import KafkaProducer  # type: ignore
            producer = KafkaProducer(bootstrap_servers=s.kafka_bootstrap,
                                     value_serializer=lambda v: json.dumps(v).encode())
        for r in rows:
            if producer:
                producer.send(s.kafka_topic_evidence, {
                    "id": r.id, "case_id": r.case_id, "type": r.event_type,
                    "payload": r.payload, "hash": r.hash, "prev_hash": r.prev_hash,
                    "ts": r.created_at.isoformat(),
                })
            r.published = True
            n += 1
        if producer:
            producer.flush()
        sess.commit()
        return n
    finally:
        sess.close()
