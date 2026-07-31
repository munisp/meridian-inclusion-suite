"""Auth: Keycloak RS256 (prod) / dev mode (AUTH_MODE=dev).

Python port of the shared Go pattern (internal/platform/authx + httpx):
- AUTH_MODE=keycloak: RS256 Bearer validated against JWKS (5-min cache,
  refresh-on-unknown-kid). FAIL-CLOSED: any validation error -> 401.
- AUTH_MODE=dev: X-Dev-Role header (kyc.agent|kyc.reviewer|kyc.admin).
"""
from __future__ import annotations

import base64
import json
import time
from typing import Optional

import httpx
from fastapi import Depends, HTTPException, Request
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from .config import get_settings

_bearer = HTTPBearer(auto_error=False)

ROLE_AGENT = "kyc.agent"
ROLE_REVIEWER = "kyc.reviewer"
ROLE_ADMIN = "kyc.admin"


def _b64url_decode(s: str) -> bytes:
    s += "=" * (-len(s) % 4)
    return base64.urlsafe_b64decode(s.encode())


class _JwksCache:
    def __init__(self):
        self.keys: dict[str, dict] = {}
        self.fetched: float = 0.0

    def _url(self) -> str:
        s = get_settings()
        if s.keycloak_jwks_url:
            return s.keycloak_jwks_url
        return s.keycloak_issuer.rstrip("/") + "/protocol/openid-connect/certs"

    def refresh(self):
        r = httpx.get(self._url(), timeout=10.0)
        r.raise_for_status()
        self.keys = {k["kid"]: k for k in r.json().get("keys", []) if "kid" in k}
        self.fetched = time.time()

    def get(self, kid: str) -> Optional[dict]:
        if time.time() - self.fetched > 300 or kid not in self.keys:
            self.refresh()
        return self.keys.get(kid)


_jwks = _JwksCache()


def _verify_rs256(token: str) -> dict:
    """Minimal RS256 JWT verification against JWKS (fail-closed)."""
    try:
        from cryptography.hazmat.primitives.asymmetric import rsa, padding  # noqa
        from cryptography.hazmat.primitives import hashes
        from cryptography.exceptions import InvalidSignature
    except ImportError as e:  # fail-closed: cannot verify -> reject
        raise HTTPException(401, "auth backend unavailable") from e

    try:
        header_b64, payload_b64, sig_b64 = token.split(".")
        header = json.loads(_b64url_decode(header_b64))
        payload = json.loads(_b64url_decode(payload_b64))
        sig = _b64url_decode(sig_b64)
    except Exception as e:
        raise HTTPException(401, "malformed token") from e
    if header.get("alg") != "RS256":
        raise HTTPException(401, "unsupported alg")
    jwk = _jwks.get(header.get("kid", ""))
    if not jwk:
        raise HTTPException(401, "unknown kid")
    n = int.from_bytes(_b64url_decode(jwk["n"]), "big")
    e = int.from_bytes(_b64url_decode(jwk["e"]), "big")
    pub = rsa.RSAPublicNumbers(e, n).public_key()
    try:
        pub.verify(sig, f"{header_b64}.{payload_b64}".encode(), padding.PKCS1v15(), hashes.SHA256())
    except InvalidSignature as e:
        raise HTTPException(401, "bad signature") from e
    if payload.get("exp", 0) < time.time():
        raise HTTPException(401, "token expired")
    iss = get_settings().keycloak_issuer
    if iss and payload.get("iss") != iss:
        raise HTTPException(401, "bad issuer")
    return payload


def _roles_from_claims(payload: dict) -> list[str]:
    roles = payload.get("roles") or []
    realm = (payload.get("realm_access") or {}).get("roles") or []
    return list(roles) + list(realm)


def current_roles(
    request: Request,
    creds: Optional[HTTPAuthorizationCredentials] = Depends(_bearer),
) -> list[str]:
    s = get_settings()
    if s.auth_mode == "dev":
        role = request.headers.get("X-Dev-Role", "")
        if role in (ROLE_AGENT, ROLE_REVIEWER, ROLE_ADMIN):
            return [role]
        raise HTTPException(401, "provide X-Dev-Role header (AUTH_MODE=dev)")
    if not creds:
        raise HTTPException(401, "missing bearer token")
    return _roles_from_claims(_verify_rs256(creds.credentials))


def require_role(*allowed: str):
    def dep(roles: list[str] = Depends(current_roles)) -> list[str]:
        if not any(r in allowed for r in roles):
            raise HTTPException(403, "insufficient role")
        return roles
    return dep
