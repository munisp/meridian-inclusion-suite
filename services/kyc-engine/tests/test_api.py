"""API e2e with fallbacks (SPEC A §7 integration, light deps only)."""
from __future__ import annotations

import base64
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from tests.make_fixtures import cac_cert, face_pair, nin_slip, valid_rc

AGENT = {"X-Dev-Role": "kyc.agent"}
REVIEWER = {"X-Dev-Role": "kyc.reviewer"}


def _upload(client, case_id, png, doc_type):
    return client.put(f"/v1/cases/{case_id}/documents",
                      files={"file": ("doc.png", png, "image/png")},
                      data={"doc_type": doc_type}, headers=AGENT)


def test_health_ready(client):
    assert client.get("/healthz").json()["status"] == "ok"
    assert client.get("/readyz").json()["status"] == "ready"
    m = client.get("/metrics")
    assert m.status_code == 200


def test_auth_required(client):
    assert client.post("/v1/cases", json={"subject_type": "individual"}).status_code == 401
    assert client.get("/v1/cases/xyz", headers=AGENT).status_code == 404


def test_full_individual_case_step_up_via_sim(client):
    """End-to-end: create -> upload nin+selfie -> process. OCR is sim-tagged
    in this env, so auto-approve must be blocked (SPEC A §6)."""
    r = client.post("/v1/cases", json={"subject_type": "individual",
                                       "channel": "api"}, headers=AGENT)
    assert r.status_code == 201
    cid = r.json()["case_id"]
    assert _upload(client, cid, nin_slip(), "nin_slip").status_code == 202
    id_png, sf_png = face_pair(match=True)
    _upload(client, cid, sf_png, "selfie")
    r = client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    assert r.status_code == 202
    case = client.get(f"/v1/cases/{cid}", headers=AGENT).json()
    assert case["decision"] in ("step_up", "approve", "reject")
    if case["decision"] == "step_up":
        assert "SIM_NO_AUTO_APPROVE" in case["reasons"] or case["status"] == "liveness_pending"
    kinds = {c["kind"] for c in case["checks"]}
    assert "ocr" in kinds and "forensics" in kinds


def test_duplicate_upload_idempotent(client):
    cid = client.post("/v1/cases", json={"subject_type": "individual"},
                      headers=AGENT).json()["case_id"]
    a = _upload(client, cid, nin_slip(), "nin_slip").json()
    b = _upload(client, cid, nin_slip(), "nin_slip").json()
    assert a["document_id"] == b["document_id"]
    assert b.get("idempotent") is True


def test_business_case_kyb_check(client):
    cid = client.post("/v1/cases", json={"subject_type": "business"},
                      headers=AGENT).json()["case_id"]
    _upload(client, cid, cac_cert(valid_rc("123456")), "cac_cert")
    client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    case = client.get(f"/v1/cases/{cid}", headers=AGENT).json()
    kyb = [c for c in case["checks"] if c["kind"] == "kyb"]
    assert kyb and kyb[0]["sim"] is True
    assert "ubo" in kyb[0]["detail"]


def test_review_flow(client):
    cid = client.post("/v1/cases", json={"subject_type": "individual"},
                      headers=AGENT).json()["case_id"]
    _upload(client, cid, nin_slip(conf=0.6), "nin_slip")  # low conf -> step_up
    client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    case = client.get(f"/v1/cases/{cid}", headers=AGENT).json()
    assert case["status"] == "step_up", case
    # reviewer role required
    assert client.post(f"/v1/cases/{cid}/review",
                       json={"action": "approve", "note": "ok"},
                       headers=AGENT).status_code == 403
    r = client.post(f"/v1/cases/{cid}/review",
                    json={"action": "reject", "note": "blurry"}, headers=REVIEWER)
    assert r.status_code == 200
    assert client.get(f"/v1/cases/{cid}", headers=AGENT).json()["status"] == "rejected"


def test_evidence_chain_endpoint(client):
    cid = client.post("/v1/cases", json={"subject_type": "individual"},
                      headers=AGENT).json()["case_id"]
    _upload(client, cid, nin_slip(), "nin_slip")
    client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    ev = client.get(f"/v1/cases/{cid}/evidence", headers=AGENT).json()
    assert ev["chain_valid"] is True
    assert len(ev["links"]) >= 2
    types = [l["event_type"] for l in ev["links"]]
    assert "kyc.case.created.v1" in types


def test_liveness_ws_session(client):
    cid = client.post("/v1/cases", json={"subject_type": "individual",
                                         "channel": "selfie"}, headers=AGENT).json()["case_id"]
    id_png, sf_png = face_pair(match=True)
    _upload(client, cid, sf_png, "selfie")
    client.post(f"/v1/cases/{cid}/process", headers=AGENT)
    r = client.post(f"/v1/cases/{cid}/liveness/session", headers=AGENT)
    assert r.status_code == 200
    body = r.json()
    assert body["ws_url"].startswith("/liveness/")
    assert body["challenge"] == ["blink", "turn_left"]

    with client.websocket_connect(body["ws_url"]) as ws:
        assert ws.receive_json()["type"] == "challenge"
        ws.send_json({"type": "passive",
                      "frame_png_b64": base64.b64encode(sf_png).decode()})
        v = ws.receive_json()
        assert v["type"] == "verdict" and v["kind"] == "passive"
        ws.send_json({"type": "response", "challenge": "blink",
                      "detected": True, "elapsed": 2.0})
        assert ws.receive_json()["accepted"] is True
        ws.send_json({"type": "response", "challenge": "turn_left",
                      "detected": True, "elapsed": 3.0})
        ws.receive_json()
        fin = ws.receive_json()
        assert fin["type"] == "final"


def test_unknown_ws_session(client):
    with client.websocket_connect("/liveness/nope") as ws:
        assert ws.receive_json()["type"] == "error"
