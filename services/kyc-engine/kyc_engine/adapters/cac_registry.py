"""CAC registry cross-check adapter.

[SIM] When cac_registry_url is unset, returns deterministic fixtures derived
from the RC number hash — the interface mirrors the real NRS/CAC public API
so the real adapter drops in without caller changes. sim=True is always
propagated into the check detail (SPEC A §6).
"""
from __future__ import annotations

import hashlib
from typing import Any, Optional

import httpx

from ..config import get_settings

SIM_DIRECTOR_POOL = [
    "Adaeze Okafor", "Ibrahim Musa", "Chinedu Eze", "Funke Adeyemi",
    "Ngozi Nwosu", "Tunde Bakare", "Aisha Bello", "Emeka Obi",
]


class CacRegistrySim:
    sim = True

    def lookup(self, rc_number: str) -> Optional[dict[str, Any]]:
        h = int(hashlib.sha256(rc_number.encode()).hexdigest(), 16)
        if h % 97 == 0:  # deterministic "not found" slice
            return None
        n = 1 + h % 3
        directors = [SIM_DIRECTOR_POOL[(h + i) % len(SIM_DIRECTOR_POOL)] for i in range(n)]
        return {
            "rc_number": rc_number,
            "company_name": f"SIM ENTERPRISES {h % 1000:03d} LTD",
            "status": "active",
            "reg_date": f"20{h % 20:02d}-{(h % 12) + 1:02d}-{(h % 27) + 1:02d}",
            "directors": [{"name": d, "ownership_pct": round(100.0 / n, 2)} for d in directors],
            "sim": True,
        }


class CacRegistryHttp:
    sim = False

    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")

    def lookup(self, rc_number: str) -> Optional[dict[str, Any]]:
        r = httpx.get(f"{self.base}/public/registry/companies/{rc_number}", timeout=15.0)
        if r.status_code == 404:
            return None
        r.raise_for_status()
        out = r.json()
        out["sim"] = False
        return out


_registry = None


def get_registry():
    global _registry
    if _registry is None:
        s = get_settings()
        _registry = CacRegistryHttp(s.cac_registry_url) if s.cac_registry_url else CacRegistrySim()
    return _registry


def reset_registry():  # test hook
    global _registry
    _registry = None
