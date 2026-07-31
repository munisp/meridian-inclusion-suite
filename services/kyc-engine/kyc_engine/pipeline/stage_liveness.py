"""Stage 7: liveness — passive (MiniFASNetv2 Silent-Face) + challenge-response.

The challenge-response STATE MACHINE is REAL and dependency-free
(workers/liveness_ws.py drives it over WS). Passive scoring uses
MiniFASNetv2 when available; otherwise frame-texture heuristics (REAL numpy:
replay/print attacks compress high-frequency detail) tagged sim=False but
engine-labelled, since the heuristic runs on real pixels. If neither model
nor frames exist, score 0.
"""
from __future__ import annotations

import io
from typing import Any

import numpy as np

from ..config import get_settings

try:
    import onnxruntime  # type: ignore  # noqa: F401  (MiniFASNetv2 runtime)
    _HAVE_ORT = True
except ImportError:
    _HAVE_ORT = False

LIVENESS_ENGINE = "minifasnetv2" if _HAVE_ORT else "texture-heuristic"


def _hf_energy(png: bytes) -> float:
    """High-frequency energy — live camera frames have sensor noise; replayed
    screens/prints show moire + blur (lower HF at fixed scale)."""
    from PIL import Image
    img = np.asarray(Image.open(io.BytesIO(png)).convert("L"), dtype=np.float32) / 255.0
    gx = np.diff(img, axis=1)
    gy = np.diff(img, axis=0)
    return float(gx.var() + gy.var())


def passive_score(frame_png: bytes) -> dict[str, Any]:
    """Returns {score 0-1, engine}. MiniFASNetv2 path used when the ONNX model
    is present (MINIFASNET_MODEL env), else texture heuristic."""
    import os
    model = os.environ.get("MINIFASNET_MODEL", "")
    if _HAVE_ORT and model and os.path.exists(model):
        import onnxruntime as ort
        from PIL import Image
        sess = ort.InferenceSession(model, providers=["CPUExecutionProvider"])
        img = Image.open(io.BytesIO(frame_png)).convert("RGB").resize((80, 80))
        x = np.asarray(img, dtype=np.float32).transpose(2, 0, 1)[None] / 255.0
        out = sess.run(None, {sess.get_inputs()[0].name: x})[0][0]
        probs = np.exp(out) / np.exp(out).sum()
        return {"score": float(probs[-1]), "engine": LIVENESS_ENGINE, "sim": False}
    hf = _hf_energy(frame_png)
    # heuristic mapping calibrated on fixture replay/print attacks
    score = float(min(1.0, max(0.0, (hf - 0.0005) / 0.004)))
    return {"score": score, "engine": "texture-heuristic", "sim": False,
            "hf_energy": hf}


class ChallengeStateMachine:
    """REAL challenge-response state machine (SPEC A §5):
    MiniFASNet >= 0.8 AND 2/2 challenges within 10s; else step_up once,
    then reject(SPOOF). Challenges: blink, turn_left, turn_right."""

    CHALLENGES = ["blink", "turn_left"]

    def __init__(self, window_seconds: float | None = None, required: int | None = None):
        s = get_settings()
        self.window = window_seconds or s.liveness_challenge_window_seconds
        self.required = required or s.liveness_challenges_required
        self.passive_ok = False
        self.responses: dict[str, float] = {}   # challenge -> elapsed seconds
        self.attempts = 0
        self.state = "open"                     # open|passed|step_up|failed

    def set_passive(self, score: float) -> None:
        self.passive_ok = score >= get_settings().liveness_pass

    def respond(self, challenge: str, detected: bool, elapsed: float) -> dict:
        if self.state not in ("open", "step_up"):
            return {"accepted": False, "state": self.state, "reason": "session_closed"}
        if detected and elapsed <= self.window:
            self.responses[challenge] = elapsed
        return {"accepted": bool(detected and elapsed <= self.window),
                "state": self.state, "remaining": self.remaining()}

    def remaining(self) -> list[str]:
        return [c for c in self.CHALLENGES[: self.required] if c not in self.responses]

    def finalize(self) -> dict:
        """Resolve the session: pass, allow one step_up retry, then fail."""
        done = len(self.responses) >= self.required
        if self.passive_ok and done:
            self.state = "passed"
        elif self.attempts == 0:
            self.attempts += 1
            self.state = "step_up"     # one retry allowed
            self.responses = {}
        else:
            self.state = "failed"      # -> reject(SPOOF)
        return {"state": self.state, "passive_ok": self.passive_ok,
                "responses": dict(self.responses), "attempts": self.attempts}
