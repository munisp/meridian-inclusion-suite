"""WebSocket liveness challenge-response worker (SPEC A §2).

Protocol (JSON frames):
  client -> {"type": "passive", "frame_png_b64": "..."}        passive score
  client -> {"type": "response", "challenge": "blink",
             "detected": true, "elapsed": 2.1}                  challenge result
  server -> {"type": "verdict", ...} per message, and a final verdict when
             the state machine resolves (passed|step_up|failed).
"""
from __future__ import annotations

import base64
import time

from fastapi import WebSocket, WebSocketDisconnect

from ..pipeline.orchestrator import finalize_liveness
from ..pipeline.stage_liveness import ChallengeStateMachine, passive_score

_sessions: dict[str, tuple[ChallengeStateMachine, str, float]] = {}


def open_session(session_id: str, case_id: str, challenges: list[str]) -> None:
    sm = ChallengeStateMachine()
    if challenges:
        sm.CHALLENGES = list(challenges)
    _sessions[session_id] = (sm, case_id, time.monotonic())


def get_session(session_id: str):
    return _sessions.get(session_id)


async def liveness_ws(websocket: WebSocket, session_id: str):
    await websocket.accept()
    entry = get_session(session_id)
    if entry is None:
        await websocket.send_json({"type": "error", "reason": "unknown_session"})
        await websocket.close(code=4404)
        return
    sm, case_id, _ = entry
    await websocket.send_json({"type": "challenge", "challenge": sm.remaining()})
    try:
        while True:
            msg = await websocket.receive_json()
            mtype = msg.get("type")
            if mtype == "passive":
                frame = base64.b64decode(msg.get("frame_png_b64", ""))
                res = passive_score(frame)
                sm.set_passive(res["score"])
                await websocket.send_json({"type": "verdict", "kind": "passive", **res})
            elif mtype == "response":
                res = sm.respond(msg.get("challenge", ""), bool(msg.get("detected")),
                                 float(msg.get("elapsed", 999)))
                await websocket.send_json({"type": "verdict", "kind": "challenge", **res})
                if not sm.remaining():
                    fin = sm.finalize()
                    await websocket.send_json({"type": "final", **fin})
                    if fin["state"] in ("passed", "failed"):
                        finalize_liveness(case_id, fin["state"],
                                          passive_score=1.0 if fin["passive_ok"] else 0.0)
                        _sessions.pop(session_id, None)
                        await websocket.close()
                        return
                    await websocket.send_json({"type": "challenge", "challenge": sm.remaining()})
            elif mtype == "finalize":
                fin = sm.finalize()
                await websocket.send_json({"type": "final", **fin})
                if fin["state"] in ("passed", "failed"):
                    finalize_liveness(case_id, fin["state"],
                                      passive_score=1.0 if fin["passive_ok"] else 0.0)
                    _sessions.pop(session_id, None)
                    await websocket.close()
                    return
            else:
                await websocket.send_json({"type": "error", "reason": "bad_message_type"})
    except WebSocketDisconnect:
        return
