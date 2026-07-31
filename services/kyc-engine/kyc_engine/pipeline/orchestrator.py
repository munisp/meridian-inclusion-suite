"""KycCaseWorkflow orchestrator (SPEC A §4).

When TEMPORAL_URL is set, stages are registered as activities of the
Temporal workflow `KycCaseWorkflow` (temporalio client, 120s activity
timeout, 3x retry). Otherwise an in-proc runner applies identical
semantics: per-stage retry, timeout budget, DLQ emission after poisoned
retries (topic kyc.dlq.v1 via the audit outbox).
"""
from __future__ import annotations

import time
from datetime import datetime, timezone
from typing import Any, Callable

from sqlalchemy import select

from ..adapters import audit, pii
from ..adapters.storage import get_storage
from ..adapters.tingraph import TinGraphClient, TinGraphDown
from ..config import get_settings
from ..models.db import get_session
from ..models.tables import (KycCase, KycCheck, KycDecision, KycDocument,
                             KycExtraction)
from . import stage_decision, stage_fields, stage_forensics, stage_kyb
from .stage_face import match_faces
from .stage_ocr import run_ocr
from .stage_parse import parse_document

WORKFLOW_NAME = "KycCaseWorkflow"


class StagePoisoned(RuntimeError):
    """Retries exhausted -> DLQ (SPEC A §6)."""


def _run_stage(name: str, fn: Callable[[], Any]) -> Any:
    """Retry wrapper mirroring Temporal activity semantics (3x, 120s)."""
    s = get_settings()
    last: Exception | None = None
    for attempt in range(1, s.stage_max_retries + 1):
        t0 = time.monotonic()
        try:
            out = fn()
            _stage_latency(name, time.monotonic() - t0)
            return out
        except Exception as e:  # noqa: BLE001 - retried, then DLQ
            last = e
    raise StagePoisoned(f"stage {name} failed after {s.stage_max_retries} attempts: {last}")


# --- prometheus-lite metrics registry (exposed at /metrics) -----------------
_STAGE_LATENCY: dict[str, list[float]] = {}
_DECISION_MIX: dict[str, int] = {}
_OCR_CONF: list[float] = []


def _stage_latency(stage: str, seconds: float) -> None:
    _STAGE_LATENCY.setdefault(stage, []).append(seconds)


def record_decision(verdict: str) -> None:
    _DECISION_MIX[verdict] = _DECISION_MIX.get(verdict, 0) + 1


def record_ocr_conf(conf: float) -> None:
    _OCR_CONF.append(conf)


def metrics_text() -> str:
    lines = ["# kyc-engine metrics"]
    for stage, vals in _STAGE_LATENCY.items():
        lines.append(f'kyc_stage_latency_seconds_avg{{stage="{stage}"}} {sum(vals)/len(vals):.6f}')
        lines.append(f'kyc_stage_latency_seconds_count{{stage="{stage}"}} {len(vals)}')
    for verdict, n in _DECISION_MIX.items():
        lines.append(f'kyc_decision_total{{verdict="{verdict}"}} {n}')
    if _OCR_CONF:
        buckets = [0.5, 0.6, 0.7, 0.75, 0.8, 0.9, 1.0]
        for b in buckets:
            lines.append(f'kyc_ocr_conf_bucket{{le="{b}"}} {sum(1 for c in _OCR_CONF if c <= b)}')
        lines.append(f'kyc_ocr_conf_count {len(_OCR_CONF)}')
    return "\n".join(lines) + "\n"


# --- temporal hook ------------------------------------------------------------

def temporal_available() -> bool:
    return bool(get_settings().temporal_url)


async def start_temporal_worker():  # pragma: no cover - needs a Temporal server
    """Prod entrypoint: register KycCaseWorkflow against TEMPORAL_URL."""
    from temporalio.client import Client
    from temporalio.worker import Worker
    client = await Client.connect(get_settings().temporal_url)
    worker = Worker(client, task_queue=get_settings().temporal_task_queue,
                    workflows=[], activities=[run_case])
    await worker.run()


# --- case pipeline ------------------------------------------------------------

def _record_check(sess, case_id: str, kind: str, score: float, passed: bool,
                  detail: dict, sim: bool, degraded: bool = False) -> KycCheck:
    chk = KycCheck(case_id=case_id, kind=kind, score=score, passed=passed,
                   detail=detail, sim=sim, degraded=degraded)
    sess.add(chk)
    return chk


def _decision_hashes(sess, case_id: str, verdict: str, score: int,
                     reasons: list[str], actor: str) -> KycDecision:
    last = sess.execute(
        select(KycDecision).where(KycDecision.case_id == case_id)
        .order_by(KycDecision.created_at.desc())).scalars().first()
    prev = last.hash if last else audit.GENESIS
    ts = datetime.now(timezone.utc).isoformat()
    payload = stage_decision.decision_hash_payload(verdict, score, reasons, actor)
    h = audit.compute_hash(prev, "kyc.decision", payload, ts)
    dec = KycDecision(case_id=case_id, verdict=verdict, score=score,
                      reasons=reasons, actor=actor, prev_hash=prev, hash=h)
    dec.created_at = datetime.fromisoformat(ts)
    sess.add(dec)
    return dec


def _selfie_and_id(sess, case_id: str) -> tuple[bytes | None, bytes | None]:
    docs = sess.execute(select(KycDocument).where(KycDocument.case_id == case_id)).scalars().all()
    storage = get_storage()
    s = get_settings()
    selfie = id_photo = None
    for d in docs:
        data = storage.get(s.minio_bucket_raw, d.minio_key)
        if d.doc_type == "selfie":
            selfie = data
        elif d.doc_type in ("nin_slip", "passport", "drivers_license"):
            id_photo = data
    return selfie, id_photo


def run_case(case_id: str) -> dict[str, Any]:
    """Stages 2-9 for all documents of a case. Returns the decision dict."""
    s = get_settings()
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        if case is None:
            raise KeyError(f"case {case_id} not found")
        case.status = "processing"
        sess.commit()
        storage = get_storage()
        unknown_doctype = False
        selfie_pending = case.channel in ("selfie", "agent_pwa")

        docs = sess.execute(select(KycDocument).where(KycDocument.case_id == case_id)).scalars().all()
        try:
            for doc in docs:
                data = storage.get(s.minio_bucket_raw, doc.minio_key)
                parsed = _run_stage("parse", lambda: parse_document(data, doc.mime))
                page = parsed.page_images[0] if parsed.page_images else data

                ocr = _run_stage("ocr", lambda: run_ocr(page))
                record_ocr_conf(ocr.conf_avg)
                fields = _run_stage("fields", lambda: stage_fields.extract_fields(doc.doc_type, ocr.tokens))
                unknown_doctype = unknown_doctype or bool(fields.get("_unknown_doctype"))

                # PII at rest (K5): persist masked + HMAC-pseudonymised
                # fields; raw values only in the restricted vault column.
                sanitized, vault = pii.protect_fields(fields)
                ext = KycExtraction(document_id=doc.id, fields=sanitized,
                                    pii_vault=vault,
                                    ocr_conf_avg=ocr.conf_avg,
                                    extractor_version=stage_fields.EXTRACTOR_VERSION)
                sess.add(ext)
                _record_check(sess, case_id, "ocr", ocr.conf_avg,
                              ocr.conf_avg >= s.ocr_conf_step_up,
                              {"engine": ocr.engine, "conf_avg": ocr.conf_avg,
                               "tokens": len(ocr.tokens)}, sim=ocr.sim)

                if doc.doc_type == "selfie":
                    continue  # forensics applies to ID docs, not selfies
                fx = _run_stage("forensics", lambda: stage_forensics.run_forensics(page))
                ts = fx["tamper_score"]
                _record_check(sess, case_id, "forensics", 1.0 - ts,
                              ts < s.forensics_step_up,
                              {"tamper_score": ts, "signals": fx["classical"]["signals"],
                               "vlm": fx.get("vlm")}, sim=fx.get("sim", False),
                              degraded=fx.get("degraded", False))

                if doc.doc_type == "cac_cert":
                    kyb = _run_stage("kyb", lambda: stage_kyb.run_kyb(fields))
                    ok = not kyb["issues"]
                    _record_check(sess, case_id, "kyb", 1.0 if ok else 0.5, ok,
                                  {"ubo": kyb["ubo"], "directors": kyb["directors"],
                                   "issues": kyb["issues"],
                                   "registry": kyb.get("registry")},
                                  sim=kyb["sim"])
        except StagePoisoned as e:
            case.status = "failed"
            sess.commit()
            audit.emit(case_id, "kyc.dlq.v1", {"error": str(e)}, session=sess)
            sess.commit()
            return {"verdict": "reject", "score": 0, "reasons": ["PIPELINE_DLQ"]}

        # face match: selfie vs ID photo
        selfie, id_photo = _selfie_and_id(sess, case_id)
        if selfie and id_photo:
            fm = _run_stage("face_match", lambda: match_faces(id_photo, selfie))
            cos = fm["cosine"]
            _record_check(sess, case_id, "face_match", cos, cos >= s.face_pass,
                          fm, sim=fm.get("sim", False))

        decision = evaluate_decision(case_id, sess, unknown_doctype)
        if selfie and selfie_pending and decision["verdict"] != "reject":
            case.status = "liveness_pending"
        else:
            _apply_decision(sess, case, decision, actor="system")
        sess.commit()
        audit.emit(case_id, "kyc.pipeline.completed.v1",
                   {"decision": decision["verdict"], "score": decision["score"]}, session=sess)
        sess.commit()
        return decision
    finally:
        sess.close()


def evaluate_decision(case_id: str, sess=None, unknown_doctype: bool = False) -> dict[str, Any]:
    """Assemble checks into the SPEC §5 weighted decision (pure, testable)."""
    s = get_settings()
    own = sess is None
    sess = sess or get_session()
    try:
        sess.flush()  # sessionmaker is autoflush=False; make new checks visible
        case = sess.get(KycCase, case_id)
        checks = sess.execute(select(KycCheck).where(KycCheck.case_id == case_id)).scalars().all()
        assembled: list[dict[str, Any]] = []
        for c in checks:
            item: dict[str, Any] = {"kind": c.kind, "score": c.score, "passed": c.passed,
                                    "sim": c.sim, "degraded": c.degraded}
            if c.kind == "forensics":
                ts = c.detail.get("tamper_score", 0.0)
                if ts >= s.forensics_reject:
                    item.update(hard_fail=True, reason="FORENSICS_TAMPER")
                item["score"] = max(0.0, 1.0 - ts)
            elif c.kind == "face_match":
                cos = c.detail.get("cosine", c.score)
                if cos < s.face_step_up:
                    item.update(hard_fail=True, reason="FACE_MISMATCH")
                item["score"] = min(1.0, cos / max(s.face_pass, 1e-9))
            elif c.kind == "liveness":
                if c.detail.get("state") == "failed":
                    item.update(hard_fail=True, reason="SPOOF")
            assembled.append(item)
        dec = stage_decision.decide(assembled, case.subject_type, unknown_doctype,
                                    risk_flags=_edd_risk_flags(case, assembled))
        return dec
    finally:
        if own:
            sess.close()


def _edd_risk_flags(case: KycCase, assembled: list[dict[str, Any]]) -> list[str]:
    """EDD triggers (FATF R.10/R.12): PEP match, screening hit, high-value
    threshold, non-face-to-face channel. Any trigger -> enhanced_review."""
    s = get_settings()
    flags: list[str] = []
    if any(c.get("pep_hit") for c in assembled):
        flags.append("PEP_MATCH")
    if case.channel and case.channel in s.edd_non_f2f_channel_set:
        flags.append("NON_FACE_TO_FACE")
    if s.edd_high_value_threshold > 0 and (case.declared_value or 0.0) >= s.edd_high_value_threshold:
        flags.append("HIGH_VALUE")
    return flags


def _apply_decision(sess, case: KycCase, decision: dict, actor: str) -> None:
    case.decision = decision["verdict"]
    case.risk_score = 100 - decision["score"]
    case.reason_codes = decision["reasons"]
    case.status = {"approve": "approved", "reject": "rejected", "step_up": "step_up",
                   "enhanced_review": "enhanced_review"}[decision["verdict"]]
    _decision_hashes(sess, case.id, decision["verdict"], decision["score"],
                     decision["reasons"], actor)
    record_decision(decision["verdict"])


def finalize_liveness(case_id: str, state: str, passive_score: float) -> dict[str, Any]:
    """Called by the liveness WS worker when a session resolves."""
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        passed = state == "passed"
        _record_check(sess, case_id, "liveness", passive_score if passed else 0.0,
                      passed, {"state": state, "passive_score": passive_score},
                      sim=False)
        decision = evaluate_decision(case_id, sess)
        _apply_decision(sess, case, decision, actor="system")
        sess.commit()
        audit.emit(case_id, "kyc.liveness.finalized.v1",
                   {"state": state, "decision": decision["verdict"]}, session=sess)
        sess.commit()
        return decision
    finally:
        sess.close()


def review_case(case_id: str, action: str, note: str, actor: str = "user") -> dict[str, Any]:
    """Human-in-loop for step_up (SPEC A §2). Business approve -> tin-graph
    CAC provision (fail-closed)."""
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        if case is None:
            raise KeyError(f"case {case_id} not found")
        if case.status not in ("step_up", "enhanced_review"):
            raise ValueError(f"case {case_id} not in review (status={case.status})")
        verdict = "approve" if action == "approve" else "reject"
        reasons = ["MANUAL_REVIEW"] + ([f"note:{note}"] if note else [])
        _apply_decision(sess, case, {"verdict": verdict,
                                     "score": 100 - (case.risk_score or 50),
                                     "reasons": reasons}, actor=actor)
        sess.commit()
        audit.emit(case_id, "kyc.review.v1",
                   {"action": action, "note": note, "actor": actor}, session=sess)
        sess.commit()
        if verdict == "approve" and case.subject_type == "business":
            provision_business_tin(case_id)
        return {"case_id": case_id, "decision": verdict, "reasons": reasons}
    finally:
        sess.close()


def provision_business_tin(case_id: str) -> dict[str, Any]:
    """On business approval: CAC -> TIN provision via tin-graph. Fail-closed:
    a tin-graph outage flips the case back to step_up (never silently skip)."""
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        ext = sess.execute(
            select(KycExtraction).join(KycDocument)
            .where(KycDocument.case_id == case_id, KycDocument.doc_type == "cac_cert")
        ).scalars().first()
        rc = (ext.fields.get("rc_number") if ext else None) or ""
        name = (ext.fields.get("company_name") if ext else None) or ""
        try:
            resp = TinGraphClient().provision_cac_tin(rc, name, case.subject_ref)
        except TinGraphDown as e:
            case.status = "step_up"
            case.reason_codes = list(case.reason_codes or []) + ["TINGRAPH_UNAVAILABLE"]
            sess.commit()
            audit.emit(case_id, "kyc.tingraph.failed.v1", {"error": str(e)}, session=sess)
            sess.commit()
            raise
        audit.emit(case_id, "kyc.tin.provisioned.v1", {"cac_rc": rc, "response": resp}, session=sess)
        sess.commit()
        return resp
    finally:
        sess.close()
