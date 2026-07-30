"""Rule-pack loading for services/education (SPEC §4 T14).

Loads the embedded rp-education-ng pack (§1.4 YAML format) and exposes the
effective-dated tables to the calculators.
"""
from __future__ import annotations

from functools import lru_cache
from pathlib import Path
from typing import Any

import yaml

PACK_PATH = Path(__file__).parent / "packs" / "rp-education-ng" / "1.0.0.yaml"


class PackError(RuntimeError):
    pass


@lru_cache(maxsize=1)
def load_pack(path: str | None = None) -> dict[str, Any]:
    p = Path(path) if path else PACK_PATH
    if not p.exists():
        raise PackError(f"pack not found: {p}")
    with p.open() as f:
        pack = yaml.safe_load(f)
    if pack.get("id") != "rp-education-ng":
        raise PackError(f"unexpected pack id {pack.get('id')}")
    if not pack.get("rules"):
        raise PackError("pack has no rules")
    return pack


def get_rule(pack: dict[str, Any], rule_id: str) -> dict[str, Any]:
    for rule in pack["rules"]:
        if rule.get("id") == rule_id:
            return rule
    raise PackError(f"rule {rule_id} not in pack")


def pack_ref(pack: dict[str, Any]) -> str:
    return f"{pack['id']}@{pack['version']}"
