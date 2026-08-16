"""Audit evidence: hash-chained outbox -> Kafka topic kyc.evidence.v1.

Every pipeline event is appended to the audit_event outbox table with a
SHA-256 chain (prev_hash || payload). A relay drains unpublished rows to
Kafka; in dev (no kafka_bootstrap) the drain is a no-op in-proc marker.
"""
from __future__ import annotations

import hashlib
import json
import os
import time
import secrets
from datetime import datetime, timezone

from sqlalchemy import select

from ..config import get_settings
from ..models.db import get_session
from ..models.tables import AuditEvent

GENESIS = "0" * 64

_CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def new_ulid() -> str:
    """ULID: 48-bit ms timestamp + 80-bit randomness, Crockford base32."""
    ms = int(time.time() * 1000) & ((1 << 48) - 1)
    rand = int.from_bytes(secrets.token_bytes(10), "big")
    val = (ms << 80) | rand
    chars = []
    for _ in range(26):
        chars.append(_CROCKFORD[val & 31])
        val >>= 5
    return "".join(reversed(chars))


def code_revision() -> str:
    """Deploy-injected GIT_SHA; 'unknown' when not injected."""
    return os.environ.get("GIT_SHA") or "unknown"


def canonical(payload: dict) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str)


def compute_hash(prev_hash: str, event_type: str, payload: dict, ts: str,
                 trace_id: str | None = None, op_id: str | None = None,
                 actor_role: str | None = None, target_version: str | None = None,
                 approval_ref: str | None = None, failure_code: str | None = None,
                 revision: str | None = None) -> str:
    h = hashlib.sha256()
    h.update(prev_hash.encode())
    h.update(event_type.encode())
    h.update(canonical(payload).encode())
    h.update(ts.encode())
    for f in (trace_id, op_id, actor_role, target_version,
              approval_ref, failure_code, revision):
        h.update((f or "").encode())
    return h.hexdigest()


def emit(case_id: str, event_type: str, payload: dict, session=None,
         prev_hash: str | None = None, trace_id: str | None = None,
         op_id: str | None = None, actor_role: str | None = None,
         target_version: str | None = None, approval_ref: str | None = None,
         failure_code: str | None = None) -> AuditEvent:
    """Append a hash-chained evidence event to the outbox.

    prev_hash lets batch callers (e.g. monitoring.rescreen_due, F-10) supply
    a prefetched chain head instead of one SELECT per event.

    trace_id/op_id default to a generated ULID when the caller did not
    propagate one (HTTP layer forwards X-Trace-Id when present)."""
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
        revision = code_revision()
        trace_id = trace_id or new_ulid()
        op_id = op_id or new_ulid()
        ev = AuditEvent(
            case_id=case_id, event_type=event_type, payload=payload,
            prev_hash=prev,
            hash=compute_hash(prev, event_type, payload, ts, trace_id, op_id,
                              actor_role, target_version, approval_ref,
                              failure_code, revision),
            trace_id=trace_id, op_id=op_id, actor_role=actor_role,
            target_version=target_version, approval_ref=approval_ref,
            failure_code=failure_code, code_revision=revision,
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
        if ev.hash != compute_hash(prev, ev.event_type, ev.payload, ts,
                                   ev.trace_id, ev.op_id, ev.actor_role,
                                   ev.target_version, ev.approval_ref,
                                   ev.failure_code, ev.code_revision):
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
                    "trace_id": r.trace_id, "op_id": r.op_id,
                    "actor_role": r.actor_role, "target_version": r.target_version,
                    "approval_ref": r.approval_ref, "failure_code": r.failure_code,
                    "code_revision": r.code_revision,
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
