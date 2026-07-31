"""kyc-engine FastAPI app (SPEC A §2). All routes under /v1 except ops."""
from __future__ import annotations

from fastapi import Depends, FastAPI, File, Form, HTTPException, UploadFile, WebSocket
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import PlainTextResponse
from sqlalchemy import select

from .auth import ROLE_ADMIN, ROLE_AGENT, ROLE_REVIEWER, require_role
from .adapters import audit
from .adapters.storage import get_storage
from .config import get_settings
from .models.db import create_all, get_session
from .models.tables import KycCase, KycCheck, KycDocument, LivenessSession
from .pipeline import orchestrator
from .pipeline.orchestrator import review_case, run_case
from .pipeline.stage_ingest import ingest
from .pipeline.stage_liveness import ChallengeStateMachine
from .schemas.api import (CaseOut, CheckOut, CreateCaseRequest, CreateCaseResponse,
                          EvidenceChainOut, EvidenceLink, LivenessSessionOut,
                          ReviewRequest)
from .workers import liveness_ws

app = FastAPI(title="Meridian KYC/KYB Engine", version=get_settings().version)
app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"],
                   allow_headers=["*"])


@app.on_event("startup")
def _startup():
    create_all()


# ------------------------------------------------------------------ ops

@app.get("/healthz")
def healthz():
    s = get_settings()
    return {"status": "ok", "service": s.service_name, "version": s.version}


@app.get("/readyz")
def readyz():
    create_all()
    return {"status": "ready", "temporal": orchestrator.temporal_available()}


@app.get("/metrics", response_class=PlainTextResponse)
def metrics():
    return orchestrator.metrics_text()


# ------------------------------------------------------------------ cases

@app.post("/v1/cases", response_model=CreateCaseResponse, status_code=201,
          dependencies=[Depends(require_role(ROLE_AGENT, ROLE_ADMIN))])
def create_case(req: CreateCaseRequest):
    sess = get_session()
    try:
        case = KycCase(subject_type=req.subject_type, channel=req.channel,
                       subject_ref=req.subject_ref, declared_value=req.declared_value,
                       status="created")
        sess.add(case)
        sess.commit()
        sess.refresh(case)
        audit.emit(case.id, "kyc.case.created.v1",
                   {"subject_type": req.subject_type, "channel": req.channel}, session=sess)
        sess.commit()
        return CreateCaseResponse(
            case_id=case.id, status=case.status,
            upload_urls=[f"/v1/cases/{case.id}/documents"])
    finally:
        sess.close()


@app.put("/v1/cases/{case_id}/documents", status_code=202,
         dependencies=[Depends(require_role(ROLE_AGENT, ROLE_ADMIN))])
async def upload_document(case_id: str, file: UploadFile = File(...),
                          doc_type: str = Form("unknown")):
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        if case is None:
            raise HTTPException(404, "case not found")
        data = await file.read()
        # idempotent: same case + same sha256 -> reuse existing document row
        from .adapters.storage import sha256_hex
        digest = sha256_hex(data)
        existing = sess.execute(
            select(KycDocument).where(KycDocument.case_id == case_id,
                                      KycDocument.sha256 == digest)).scalars().first()
        if existing:
            return {"document_id": existing.id, "sha256": digest, "idempotent": True}
        doc = ingest(case_id, file.filename or "upload", data, doc_type)
        case.status = "documents_received"
        sess.commit()
        audit.emit(case_id, "kyc.document.ingested.v1",
                   {"document_id": doc.id, "sha256": digest, "doc_type": doc_type},
                   session=sess)
        sess.commit()
        return {"document_id": doc.id, "sha256": digest}
    finally:
        sess.close()


@app.post("/v1/cases/{case_id}/process", status_code=202,
          dependencies=[Depends(require_role(ROLE_AGENT, ROLE_ADMIN))])
def process_case(case_id: str):
    """Run the pipeline (in-proc; dispatched to Temporal when TEMPORAL_URL set)."""
    try:
        decision = run_case(case_id)
    except KeyError:
        raise HTTPException(404, "case not found")
    return {"case_id": case_id, "decision": decision}


@app.get("/v1/cases/{case_id}", response_model=CaseOut,
         dependencies=[Depends(require_role(ROLE_AGENT, ROLE_REVIEWER, ROLE_ADMIN))])
def get_case(case_id: str):
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        if case is None:
            raise HTTPException(404, "case not found")
        checks = sess.execute(select(KycCheck).where(KycCheck.case_id == case_id)).scalars().all()
        return CaseOut(
            case_id=case.id, subject_type=case.subject_type, status=case.status,
            decision=case.decision, risk_score=case.risk_score,
            reasons=list(case.reason_codes or []),
            checks=[CheckOut(kind=c.kind, score=c.score, passed=c.passed, sim=c.sim,
                             degraded=c.degraded, detail=c.detail) for c in checks],
            created_at=case.created_at)
    finally:
        sess.close()


# ------------------------------------------------------------------ liveness

@app.post("/v1/cases/{case_id}/liveness/session", response_model=LivenessSessionOut,
          dependencies=[Depends(require_role(ROLE_AGENT, ROLE_ADMIN))])
def create_liveness_session(case_id: str):
    sess = get_session()
    try:
        case = sess.get(KycCase, case_id)
        if case is None:
            raise HTTPException(404, "case not found")
        sm = ChallengeStateMachine()
        challenges = sm.CHALLENGES[: sm.required]
        ls = LivenessSession(case_id=case_id, challenges=challenges)
        sess.add(ls)
        sess.commit()
        sess.refresh(ls)
        liveness_ws.open_session(ls.id, case_id, challenges)
        audit.emit(case_id, "kyc.liveness.session.v1",
                   {"session_id": ls.id, "challenges": challenges}, session=sess)
        sess.commit()
        return LivenessSessionOut(session_id=ls.id,
                                  ws_url=f"/liveness/{ls.id}",
                                  challenge=challenges)
    finally:
        sess.close()


@app.websocket("/liveness/{session_id}")
async def liveness_socket(websocket: WebSocket, session_id: str):
    await liveness_ws.liveness_ws(websocket, session_id)


# ------------------------------------------------------------------ review

@app.post("/v1/cases/{case_id}/review",
          dependencies=[Depends(require_role(ROLE_REVIEWER, ROLE_ADMIN))])
def review(case_id: str, req: ReviewRequest):
    try:
        return review_case(case_id, req.action, req.note)
    except KeyError:
        raise HTTPException(404, "case not found")
    except ValueError as e:
        raise HTTPException(409, str(e))
    except Exception as e:
        from .adapters.tingraph import TinGraphDown
        if isinstance(e, TinGraphDown):
            raise HTTPException(502, f"tin-graph unavailable: {e}")
        raise


# ------------------------------------------------------------------ evidence

@app.get("/v1/cases/{case_id}/evidence", response_model=EvidenceChainOut,
         dependencies=[Depends(require_role(ROLE_AGENT, ROLE_REVIEWER, ROLE_ADMIN))])
def evidence(case_id: str):
    events = audit.chain_for_case(case_id)
    links = [EvidenceLink(seq=i, event_type=e.event_type, hash=e.hash,
                          prev_hash=e.prev_hash, timestamp=e.created_at,
                          payload=e.payload) for i, e in enumerate(events)]
    return EvidenceChainOut(case_id=case_id,
                            chain_valid=audit.verify_chain(events), links=links)


def main():  # python -m kyc_engine.main
    import uvicorn
    uvicorn.run("kyc_engine.main:app", host="0.0.0.0", port=8105)


if __name__ == "__main__":
    main()
