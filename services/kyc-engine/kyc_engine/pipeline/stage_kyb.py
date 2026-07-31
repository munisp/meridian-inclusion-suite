"""Stage 8: KYB — CAC parse -> directors/UBOs -> registry cross-check.

The CAC text parsing + UBO extraction are REAL (operate on parsed fields);
the registry cross-check adapter is [SIM]-tagged unless cac_registry_url is
set (SPEC A §5: rc checksum fail or registry mismatch -> step_up; UBO >25%
ownership extracted to ubo[]).
"""
from __future__ import annotations

from typing import Any

from ..adapters.cac_registry import get_registry
from ..config import get_settings


def run_kyb(fields: dict[str, Any]) -> dict[str, Any]:
    s = get_settings()
    rc = fields.get("rc_number")
    out: dict[str, Any] = {"rc_number": rc, "ubo": [], "directors": [],
                           "registry": None, "sim": True, "issues": []}
    if not rc:
        out["issues"].append("rc_number_missing")
        return out
    if not fields.get("rc_checksum_ok"):
        out["issues"].append("rc_checksum_fail")
    reg = get_registry().lookup(rc)
    out["registry"] = reg
    out["sim"] = bool(getattr(get_registry(), "sim", True))
    if reg is None:
        out["issues"].append("registry_not_found")
        return out
    # cross-check company name when both present
    cert_name = (fields.get("company_name") or "").strip().lower()
    reg_name = (reg.get("company_name") or "").strip().lower()
    if cert_name and reg_name and cert_name != reg_name and not out["sim"]:
        out["issues"].append("registry_name_mismatch")
    directors = reg.get("directors") or []
    out["directors"] = directors
    out["ubo"] = [d for d in directors
                  if float(d.get("ownership_pct", 0)) > s.ubo_ownership_pct]
    return out
