"""Unit: liveness challenge-response state machine (SPEC A §5)."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline.stage_liveness import ChallengeStateMachine, passive_score
from tests.make_fixtures import face_pair


def make():
    sm = ChallengeStateMachine(window_seconds=10.0, required=2)
    return sm


def test_pass_path():
    sm = make()
    sm.set_passive(0.9)
    assert sm.respond("blink", True, 2.0)["accepted"]
    assert sm.respond("turn_left", True, 3.0)["accepted"]
    fin = sm.finalize()
    assert fin["state"] == "passed"


def test_slow_challenge_rejected():
    sm = make()
    sm.set_passive(0.9)
    assert not sm.respond("blink", True, 11.0)["accepted"]  # outside 10s window
    assert "blink" in sm.remaining()


def test_passive_fail_blocks_pass():
    sm = make()
    sm.set_passive(0.5)  # below 0.8
    sm.respond("blink", True, 1.0)
    sm.respond("turn_left", True, 1.0)
    fin = sm.finalize()
    assert fin["state"] == "step_up"   # one retry allowed


def test_second_failure_is_spoof():
    sm = make()
    sm.set_passive(0.5)
    assert sm.finalize()["state"] == "step_up"
    fin = sm.finalize()
    assert fin["state"] == "failed"


def test_closed_session_rejects_responses():
    sm = make()
    sm.set_passive(0.9)
    sm.respond("blink", True, 1.0)
    sm.respond("turn_left", True, 1.0)
    sm.finalize()
    assert sm.respond("blink", True, 1.0)["reason"] == "session_closed"


def test_passive_score_real_frame():
    _, selfie = face_pair(match=True)
    res = passive_score(selfie)
    assert 0.0 <= res["score"] <= 1.0
    assert res["engine"] in ("minifasnetv2", "texture-heuristic")
