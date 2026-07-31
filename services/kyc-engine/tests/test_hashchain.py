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
