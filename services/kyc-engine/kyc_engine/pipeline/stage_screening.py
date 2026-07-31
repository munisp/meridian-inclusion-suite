"""Stage: sanctions + PEP screening (FATF R.10/R.12).

Screens the subject name (and KYB directors/UBOs) extracted by stage_fields
against the configured screening provider. A result is ALWAYS recorded in the
decision evidence — a screening outcome is never silent:
- sanctions match  -> hard-fail reject (SANCTIONS_HIT) at decision time
- PEP match        -> EDD trigger (PEP_MATCH -> enhanced_review, K4)
"""
from __future__ import annotations

from typing import Any

from ..adapters.screening import get_screening_provider


def _names_from_fields(fields: dict[str, Any]) -> list[str]:
    names: list[str] = []
    subject = " ".join(p for p in (fields.get("surname"), fields.get("first_name")) if p)
    if subject:
        names.append(subject)
    for key in ("name", "given_names", "company_name"):
        if fields.get(key):
            names.append(str(fields[key]))
    for d in fields.get("directors") or []:
        names.append(str(d))
    return names


def run_screening(fields: dict[str, Any]) -> dict[str, Any]:
    """Screen all names found in the extracted fields. Aggregated result."""
    provider = get_screening_provider()
    dob = fields.get("dob")
    all_matches: list[dict[str, Any]] = []
    screened_names: list[str] = []
    for name in _names_from_fields(fields):
        res = provider.screen(name, dob=dob)
        screened_names.append(name)
        for m in res.get("matches", []):
            all_matches.append({**m, "screened_name": name})
    all_matches.sort(key=lambda m: -m["score"])
    return {
        "screened": True,
        "provider": getattr(provider, "provider", "unknown"),
        "names": screened_names,
        "matches": all_matches,
        "sanctions_hit": any(m["kind"] == "sanctions" for m in all_matches),
        "pep_hit": any(m["kind"] == "pep" for m in all_matches),
        "sim": bool(getattr(provider, "sim", True)),
    }
