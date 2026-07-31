"""NIMC NIN verification adapter. [SIM] deterministic when nin_verify_url unset."""
from __future__ import annotations

import hashlib
from typing import Any, Optional

import httpx

from ..config import get_settings


class NinVerifySim:
    sim = True

    def verify(self, nin: str, name: str | None = None, dob: str | None = None) -> dict[str, Any]:
        h = int(hashlib.sha256(nin.encode()).hexdigest(), 16)
        return {
            "nin": nin,
            "verified": h % 89 != 0,   # deterministic rare failure
            "name_match": None if name is None else (h % 7 != 0),
            "dob_match": None if dob is None else (h % 11 != 0),
            "sim": True,
        }


class NinVerifyHttp:
    sim = False

    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")

    def verify(self, nin: str, name: str | None = None, dob: str | None = None) -> dict[str, Any]:
        r = httpx.post(f"{self.base}/v1/verify/nin",
                       json={"nin": nin, "name": name, "dob": dob}, timeout=15.0)
        r.raise_for_status()
        out = r.json()
        out["sim"] = False
        return out


_adapter = None


def get_nin_verifier():
    global _adapter
    if _adapter is None:
        s = get_settings()
        _adapter = NinVerifyHttp(s.nin_verify_url) if s.nin_verify_url else NinVerifySim()
    return _adapter
