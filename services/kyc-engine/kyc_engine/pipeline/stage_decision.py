"""Stage 9: decision — rule-based weighted scoring -> approve|step_up|reject.

SPEC A §5/§6:
- weighted score 0-100; >=70 approve, 40-69 step_up, <40 reject
- any hard-fail check forces reject regardless
- degraded forensics caps decision at step_up
- unknown doctype never auto-approves
- any required check sim + allow_sim_approve=false -> no auto-approve
"""
from __future__ import annotations

from typing import Any

from ..config import get_settings

WEIGHTS = {
    "ocr": 20,
    "forensics": 25,
    "face_match": 25,
    "liveness": 15,
    "registry": 5,
    "kyb": 10,
}

HARD_FAIL_REASONS = {"FORENSICS_TAMPER", "FACE_MISMATCH", "SPOOF"}


def decide(checks: list[dict[str, Any]], subject_type: str,
           unknown_doctype: bool = False) -> dict[str, Any]:
    """checks: [{kind, score, passed, sim, degraded, hard_fail, reason}]."""
    s = get_settings()
    reasons: list[str] = []
    hard = [c for c in checks if c.get("hard_fail")]
    for c in hard:
        if c.get("reason"):
            reasons.append(c["reason"])

    total_w = 0.0
    acc = 0.0
    for c in checks:
        w = WEIGHTS.get(c["kind"], 0)
        total_w += w
        acc += w * max(0.0, min(1.0, float(c.get("score", 0.0))))
    score = round(100 * acc / total_w) if total_w else 0

    if score >= s.decision_approve:
        verdict = "approve"
    elif score >= s.decision_step_up:
        verdict = "step_up"
    else:
        verdict = "reject"
        reasons.append("SCORE_BELOW_40")

    if hard:
        verdict = "reject"
    if any(c.get("degraded") for c in checks) and verdict == "approve":
        verdict = "step_up"
        reasons.append("DEGRADED_CAP")
    if unknown_doctype and verdict == "approve":
        verdict = "step_up"
        reasons.append("UNKNOWN_DOCTYPE")
    if (not s.allow_sim_approve and verdict == "approve"
            and any(c.get("sim") for c in checks)):
        verdict = "step_up"
        reasons.append("SIM_NO_AUTO_APPROVE")
    return {"verdict": verdict, "score": score, "reasons": reasons}


def decision_hash_payload(verdict: str, score: int, reasons: list[str], actor: str) -> dict:
    return {"verdict": verdict, "score": score, "reasons": sorted(reasons), "actor": actor}
