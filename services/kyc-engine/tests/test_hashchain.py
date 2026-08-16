"""Unit: evidence + decision hash chains."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.adapters import audit


def test_chain_valid(env):
    audit.emit("c1", "kyc.case.created.v1", {"a": 1})
    audit.emit("c1", "kyc.pipeline.completed.v1", {"b": 2})
    events = audit.chain_for_case("c1")
    assert len(events) == 2
    assert audit.verify_chain(events)
    assert events[0].prev_hash == audit.GENESIS
    assert events[1].prev_hash == events[0].hash


def test_chain_tamper_detected(env):
    audit.emit("c1", "e1", {"a": 1})
    audit.emit("c1", "e2", {"b": 2})
    events = audit.chain_for_case("c1")
    events[0].payload = {"a": 999}   # tamper
    assert not audit.verify_chain(events)


def test_chain_break_detected(env):
    audit.emit("c1", "e1", {"a": 1})
    audit.emit("c1", "e2", {"b": 2})
    events = audit.chain_for_case("c1")
    events[1].prev_hash = "ff" * 32
    assert not audit.verify_chain(events)


def test_empty_chain_valid():
    assert audit.verify_chain([])


def test_outbox_drain_marks_published(env):
    audit.emit("c1", "e1", {"a": 1})
    n = audit.drain_outbox()
    assert n == 1
    assert audit.drain_outbox() == 0


def test_new_schema_fields_default_and_hashed(env, monkeypatch):
    monkeypatch.setenv("GIT_SHA", "deadbeef123")
    ev = audit.emit("c2", "kyc.case.created.v1", {"a": 1},
                    actor_role="agent", target_version=None)
    # trace_id/op_id auto-generated ULIDs; revision from env
    assert ev.trace_id and len(ev.trace_id) == 26
    assert ev.op_id and len(ev.op_id) == 26
    assert ev.trace_id != ev.op_id
    assert ev.code_revision == "deadbeef123"
    assert ev.actor_role == "agent"
    assert ev.approval_ref is None and ev.failure_code is None
    events = audit.chain_for_case("c2")
    assert audit.verify_chain(events)
    # tamper with each new field -> chain invalid
    for attr, bad in (("trace_id", "X" * 26), ("op_id", "Y" * 26),
                      ("actor_role", "admin"), ("code_revision", "0" * 11),
                      ("failure_code", "E500"), ("approval_ref", "apr-1")):
        fresh = audit.chain_for_case("c2")
        setattr(fresh[0], attr, bad)
        assert not audit.verify_chain(fresh), attr


def test_code_revision_defaults_unknown(env, monkeypatch):
    monkeypatch.delenv("GIT_SHA", raising=False)
    ev = audit.emit("c3", "e1", {})
    assert ev.code_revision == "unknown"
    assert audit.verify_chain(audit.chain_for_case("c3"))


def test_trace_id_propagated_from_caller(env):
    ev = audit.emit("c4", "e1", {}, trace_id="trace-from-header")
    assert ev.trace_id == "trace-from-header"
    assert audit.verify_chain(audit.chain_for_case("c4"))


def test_legacy_shape_events_still_verify(env):
    """Pre-R4 rows (all new fields NULL) keep verifying: empty-string field
    updates are hash-neutral, so the migration is backward compatible."""
    from datetime import datetime, timezone
    from kyc_engine.models.tables import AuditEvent
    prev = audit.GENESIS
    ts = datetime.now(timezone.utc).isoformat()
    import hashlib
    h = hashlib.sha256()
    h.update(prev.encode()); h.update(b"e1")
    h.update(audit.canonical({}).encode()); h.update(ts.encode())
    ev = AuditEvent(case_id="c5", event_type="e1", payload={},
                    prev_hash=prev, hash=h.hexdigest())
    ev.created_at = datetime.fromisoformat(ts)
    assert audit.verify_chain([ev])
