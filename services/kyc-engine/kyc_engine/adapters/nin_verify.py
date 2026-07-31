"""NIMC identity verification adapter — vNIN-primary, raw-NIN legacy.

NIMC discontinued raw-NIN third-party verification in favour of vNIN
tokenisation: a 16-character alphanumeric token (2 letters + 12 digits + 2
letters, e.g. ``AB012345678910YZ``), single-use, enterprise-code-scoped, with
a 72-hour TTL from issuance. This adapter therefore treats the vNIN as the
primary verification credential; the raw 11-digit NIN path is retained as a
legacy fallback only.

[SIM] When nin_verify_url is unset, a deterministic simulator is used
(honestly tagged ``sim=True``; ``allow_sim_approve=false`` still blocks
auto-approve). The simulator models the real rail's failure personas:
expired token (``EX…`` prefix), not-found (``NF…`` prefix). [REAL] When
nin_verify_url is set, an HTTP adapter speaks the vNIN verify contract
(``POST {base}/v1/verify/vnin``) with distinct handling for verified /
not-found / expired / invalid-format responses — only 5xx raises (callers
retry); the interface boundary is identical, so a licensed NIMC rail drops in
without caller changes.

Raw credentials are never echoed back or logged: results carry a masked form
only (e.g. ``123****890``), matching the hash-only logging discipline.
"""
from __future__ import annotations

import hashlib
import re
from datetime import datetime, timedelta, timezone
from typing import Any, Optional

import httpx

from ..config import get_settings

VNIN_RE = re.compile(r"^[A-Za-z]{2}\d{12}[A-Za-z]{2}$")
NIN_RE = re.compile(r"^\d{11}$")


def vnin_format_valid(token: str) -> bool:
    """vNIN: 16 chars — 2 letters, 12 digits, 2 letters (NIMC token shape)."""
    return bool(VNIN_RE.fullmatch(token or ""))


def vnin_expired(issued_at: datetime, now: Optional[datetime] = None) -> bool:
    """vNIN TTL: 72 hours from issuance (config: vnin_ttl_hours)."""
    s = get_settings()
    now = now or datetime.now(timezone.utc)
    if issued_at.tzinfo is None:
        issued_at = issued_at.replace(tzinfo=timezone.utc)
    return now > issued_at + timedelta(hours=s.vnin_ttl_hours)


def mask_credential(value: str) -> str:
    """``12345678901`` -> ``123****890``; short values fully masked."""
    v = value or ""
    if len(v) <= 6:
        return "*" * len(v)
    return f"{v[:3]}****{v[-3:]}"


def _result(verified: bool, credential_type: str, credential: str, sim: bool,
            name: str | None, dob: str | None, h: int,
            reason: str | None = None) -> dict[str, Any]:
    return {
        "verified": verified,
        "credential_type": credential_type,
        "credential_masked": mask_credential(credential),
        "reason": reason,
        "name_match": None if name is None else (h % 7 != 0),
        "dob_match": None if dob is None else (h % 11 != 0),
        "sim": sim,
    }


class NinVerifySim:
    """[SIM] deterministic simulator with real-rail failure personas."""
    sim = True

    def verify(self, nin: str | None = None, name: str | None = None,
               dob: str | None = None, vnin: str | None = None,
               vnin_issued_at: datetime | None = None) -> dict[str, Any]:
        if vnin is not None:
            if not vnin_format_valid(vnin):
                return _result(False, "vnin", vnin, True, name, dob, 0,
                               reason="invalid_format")
            if vnin[:2].upper() == "EX":
                return _result(False, "vnin", vnin, True, name, dob, 0,
                               reason="token_expired")
            if vnin_issued_at is not None and vnin_expired(vnin_issued_at):
                return _result(False, "vnin", vnin, True, name, dob, 0,
                               reason="token_expired")
            if vnin[:2].upper() == "NF":
                return _result(False, "vnin", vnin, True, name, dob, 0,
                               reason="not_found")
            h = int(hashlib.sha256(f"vnin:{vnin}".encode()).hexdigest(), 16)
            return _result(h % 89 != 0, "vnin", vnin, True, name, dob, h)
        # legacy raw-NIN path (deprecated by NIMC; retained for migration)
        nin = nin or ""
        if not NIN_RE.fullmatch(nin):
            return _result(False, "nin_legacy", nin, True, name, dob, 0,
                           reason="invalid_format")
        h = int(hashlib.sha256(nin.encode()).hexdigest(), 16)
        return _result(h % 89 != 0, "nin_legacy", nin, True, name, dob, h)


class NinVerifyHttp:
    """[REAL] HTTP adapter for a licensed NIMC rail (vNIN contract)."""
    sim = False

    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")

    def verify(self, nin: str | None = None, name: str | None = None,
               dob: str | None = None, vnin: str | None = None,
               vnin_issued_at: datetime | None = None) -> dict[str, Any]:
        if vnin is not None:
            if not vnin_format_valid(vnin):
                return _result(False, "vnin", vnin, False, name, dob, 0,
                               reason="invalid_format")
            if vnin_issued_at is not None and vnin_expired(vnin_issued_at):
                return _result(False, "vnin", vnin, False, name, dob, 0,
                               reason="token_expired")
            r = httpx.post(f"{self.base}/v1/verify/vnin",
                           json={"vnin": vnin, "name": name, "dob": dob},
                           timeout=15.0)
            return self._map(r, "vnin", vnin, name, dob)
        nin = nin or ""
        if not NIN_RE.fullmatch(nin):
            return _result(False, "nin_legacy", nin, False, name, dob, 0,
                           reason="invalid_format")
        r = httpx.post(f"{self.base}/v1/verify/nin",
                       json={"nin": nin, "name": name, "dob": dob}, timeout=15.0)
        return self._map(r, "nin_legacy", nin, name, dob)

    @staticmethod
    def _map(r: httpx.Response, ctype: str, credential: str,
             name: str | None, dob: str | None) -> dict[str, Any]:
        """Distinct rail outcomes; only 5xx raises (retryable upstream)."""
        if r.status_code in (400, 422):
            return _result(False, ctype, credential, False, name, dob, 0,
                           reason="invalid_format")
        if r.status_code == 404:
            return _result(False, ctype, credential, False, name, dob, 0,
                           reason="not_found")
        if r.status_code in (409, 410):
            return _result(False, ctype, credential, False, name, dob, 0,
                           reason="token_expired")
        r.raise_for_status()
        out = r.json()
        out.pop("nin", None)   # never propagate a raw credential
        out.pop("vnin", None)
        out.setdefault("credential_type", ctype)
        out["credential_masked"] = mask_credential(credential)
        out["sim"] = False
        return out


_adapter = None


def get_nin_verifier():
    global _adapter
    if _adapter is None:
        s = get_settings()
        _adapter = NinVerifyHttp(s.nin_verify_url) if s.nin_verify_url else NinVerifySim()
    return _adapter


def reset_nin_verifier():  # test hook
    global _adapter
    _adapter = None
