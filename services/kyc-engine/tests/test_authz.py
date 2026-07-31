"""Security (audit H-6): JWT audience validation + object-level authz."""
from __future__ import annotations

import base64
import json
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

AGENT_A = {"X-Dev-Role": "kyc.agent", "X-Dev-Subject": "agent-a"}
AGENT_B = {"X-Dev-Role": "kyc.agent", "X-Dev-Subject": "agent-b"}
ADMIN = {"X-Dev-Role": "kyc.admin", "X-Dev-Subject": "admin-1"}
REVIEWER = {"X-Dev-Role": "kyc.reviewer", "X-Dev-Subject": "rev-1"}


def _create_case(client, headers) -> str:
    r = client.post("/v1/cases", json={"subject_type": "individual",
                                       "channel": "api"}, headers=headers)
    assert r.status_code == 201, r.text
    return r.json()["case_id"]


def test_agent_reads_own_case_only(client):
    cid = _create_case(client, AGENT_A)
    assert client.get(f"/v1/cases/{cid}", headers=AGENT_A).status_code == 200
    r = client.get(f"/v1/cases/{cid}", headers=AGENT_B)
    assert r.status_code == 403           # cross-agent read blocked (BOLA)


def test_admin_and_reviewer_cross_agent_allowed(client):
    cid = _create_case(client, AGENT_A)
    assert client.get(f"/v1/cases/{cid}", headers=ADMIN).status_code == 200
    assert client.get(f"/v1/cases/{cid}", headers=REVIEWER).status_code == 200


def test_cross_agent_process_blocked(client):
    from tests.make_fixtures import nin_slip
    cid = _create_case(client, AGENT_A)
    r = client.put(f"/v1/cases/{cid}/documents",
                   files={"file": ("nin.png", nin_slip(), "image/png")},
                   data={"doc_type": "nin_slip"}, headers=AGENT_B)
    assert r.status_code == 403
    assert client.post(f"/v1/cases/{cid}/process", headers=AGENT_B).status_code == 403
    assert client.get(f"/v1/cases/{cid}/evidence", headers=AGENT_B).status_code == 403
    # owner can still act
    assert client.post(f"/v1/cases/{cid}/process", headers=AGENT_A).status_code == 202


# --------------------------------------------------------------- JWT aud (a)

def _b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def _make_token(key, kid: str, aud, iss: str, roles=("kyc.agent",)) -> str:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import padding
    header = _b64url(json.dumps({"alg": "RS256", "kid": kid}).encode())
    payload = _b64url(json.dumps({
        "sub": "agent-1", "iss": iss, "aud": aud,
        "exp": int(time.time()) + 300, "roles": list(roles)}).encode())
    sig = key.sign(f"{header}.{payload}".encode(), padding.PKCS1v15(), hashes.SHA256())
    return f"{header}.{payload}.{_b64url(sig)}"


@pytest.fixture()
def keycloak_env(env, monkeypatch):
    """RS256 keypair + stubbed JWKS cache; AUTH_MODE=keycloak."""
    from cryptography.hazmat.primitives.asymmetric import rsa
    from kyc_engine import auth
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    nums = key.public_key().public_numbers()
    jwk = {"kid": "k1", "kty": "RSA", "alg": "RS256",
           "n": _b64url(nums.n.to_bytes((nums.n.bit_length() + 7) // 8, "big")),
           "e": _b64url(nums.e.to_bytes((nums.e.bit_length() + 7) // 8, "big"))}
    monkeypatch.setattr(auth._jwks, "keys", {"k1": jwk})
    monkeypatch.setattr(auth._jwks, "fetched", time.time())
    monkeypatch.setattr(auth._jwks, "refresh", lambda: None)
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", "https://sso.example/realms/meridian")
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", "kyc-engine")
    monkeypatch.setenv("PII_HMAC_KEY", "test-pii-key")
    from fastapi.testclient import TestClient
    from kyc_engine.main import app
    with TestClient(app) as c:
        yield c, key


def test_audience_valid_token_accepted(keycloak_env):
    client, key = keycloak_env
    tok = _make_token(key, "k1", "kyc-engine", "https://sso.example/realms/meridian")
    r = client.post("/v1/cases", json={"subject_type": "individual"},
                    headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 201, r.text


def test_audience_wrong_rejected(keycloak_env):
    client, key = keycloak_env
    tok = _make_token(key, "k1", "some-other-service",
                      "https://sso.example/realms/meridian")
    r = client.post("/v1/cases", json={"subject_type": "individual"},
                    headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 401


def test_audience_list_form_accepted(keycloak_env):
    client, key = keycloak_env
    tok = _make_token(key, "k1", ["auth", "kyc-engine"],
                      "https://sso.example/realms/meridian")
    r = client.post("/v1/cases", json={"subject_type": "individual"},
                    headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 201


def test_audience_unconfigured_fail_closed(keycloak_env, monkeypatch):
    client, key = keycloak_env
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", "")
    tok = _make_token(key, "k1", "kyc-engine", "https://sso.example/realms/meridian")
    r = client.post("/v1/cases", json={"subject_type": "individual"},
                    headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 401   # fail-closed: no audience configured
