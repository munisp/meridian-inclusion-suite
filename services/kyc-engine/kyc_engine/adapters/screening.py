"""Sanctions + PEP screening adapter (FATF R.10/R.12, UNSC obligations).

Two providers behind one interface:
- ``OfflineListScreening`` [SIM]: bundled SAMPLE list (OFAC-style
  consolidated format + NGA local list + PEP sample) with fuzzy name
  matching. Honest tagging: ``sim=True`` always propagates into the check
  detail, so ``allow_sim_approve=false`` blocks auto-approve on it.
- ``HttpScreeningProvider`` [REAL]: a licensed list provider behind
  ``screening_provider_url`` with the identical interface — drop-in swap.

Matching: normalised Levenshtein similarity combined with token (Jaccard)
overlap over case-folded, punctuation-stripped names; the max of the two is
compared against ``screening_match_threshold`` (env-configurable).
"""
from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any, Optional

import httpx

from ..config import get_settings

_SAMPLE = Path(__file__).with_name("screening_sample.json")

_NORM_RE = re.compile(r"[^a-z0-9 ]+")


def normalize_name(name: str) -> str:
    return " ".join(_NORM_RE.sub(" ", (name or "").lower()).split())


def _lev(a: str, b: str) -> int:
    if abs(len(a) - len(b)) > max(len(a), len(b)):
        return max(len(a), len(b))
    prev = list(range(len(b) + 1))
    for i, ca in enumerate(a, 1):
        cur = [i]
        for j, cb in enumerate(b, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (ca != cb)))
        prev = cur
    return prev[-1]


def name_similarity(a: str, b: str) -> float:
    """max(normalised Levenshtein similarity, token Jaccard overlap)."""
    a, b = normalize_name(a), normalize_name(b)
    if not a or not b:
        return 0.0
    lev = 1.0 - _lev(a, b) / max(len(a), len(b))
    ta, tb = set(a.split()), set(b.split())
    jaccard = len(ta & tb) / len(ta | tb)
    return max(lev, jaccard)


def _mk_match(entry: dict, score: float) -> dict[str, Any]:
    return {"list": entry.get("list", ""), "kind": entry.get("kind", "sanctions"),
            "name": entry.get("name", ""), "score": round(score, 4),
            "program": entry.get("program") or entry.get("position")}


def _summarise(matches: list[dict], sim: bool, provider: str) -> dict[str, Any]:
    return {
        "screened": True,
        "provider": provider,
        "matches": matches,
        "sanctions_hit": any(m["kind"] == "sanctions" for m in matches),
        "pep_hit": any(m["kind"] == "pep" for m in matches),
        "sim": sim,
    }


class OfflineListScreening:
    """[SIM] bundled-sample list screening (deterministic, offline)."""
    sim = True
    provider = "offline-sample"

    def __init__(self, list_path: str | None = None):
        path = Path(list_path) if list_path else _SAMPLE
        data = json.loads(path.read_text())
        self.entries = data.get("entries", [])

    def screen(self, name: str, dob: str | None = None) -> dict[str, Any]:
        s = get_settings()
        matches: list[dict[str, Any]] = []
        for e in self.entries:
            score = name_similarity(name, e.get("name", ""))
            for alias in e.get("aliases", []):
                score = max(score, name_similarity(name, alias))
            if score >= s.screening_match_threshold:
                if dob and e.get("dob") and e["dob"] != dob:
                    continue  # dob disambiguation: same-ish name, different person
                matches.append(_mk_match(e, score))
        matches.sort(key=lambda m: -m["score"])
        return _summarise(matches, True, self.provider)


class HttpScreeningProvider:
    """[REAL] licensed sanctions/PEP list provider via HTTP (same interface)."""
    sim = False
    provider = "http"

    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")

    def screen(self, name: str, dob: str | None = None) -> dict[str, Any]:
        r = httpx.post(f"{self.base}/v1/screen",
                       json={"name": name, "dob": dob}, timeout=15.0)
        r.raise_for_status()
        out = r.json()
        out["sim"] = False
        out.setdefault("provider", self.provider)
        return out


_provider = None


def get_screening_provider():
    global _provider
    if _provider is None:
        s = get_settings()
        if s.screening_provider_url:
            _provider = HttpScreeningProvider(s.screening_provider_url)
        else:
            _provider = OfflineListScreening(s.screening_list_path or None)
    return _provider


def reset_screening_provider():  # test hook
    global _provider
    _provider = None
