"""tin-graph integration: CAC->TIN provision on business case approval.

Endpoint names reconciled with meridian-core-platform services/tin-graph:
  POST /v1/verify/cac      {cac_rc} -> registry check
  POST /v1/tin/provision   {cac_rc, ...} -> TIN issuance
Fail-closed: with tin_graph_url unset, provision raises (business approvals
must not silently skip TIN issuance).
"""
from __future__ import annotations

from typing import Any

import httpx

from ..config import get_settings


class TinGraphDown(RuntimeError):
    pass


class TinGraphClient:
    def __init__(self, base_url: str | None = None):
        s = get_settings()
        self.base = (base_url or s.tin_graph_url).rstrip("/")
        if not self.base:
            raise TinGraphDown("tin_graph_url not configured (fail-closed)")

    def provision_cac_tin(self, cac_rc: str, company_name: str, subject_ref: str | None = None) -> dict[str, Any]:
        payload: dict[str, Any] = {"cac_rc": cac_rc, "legal_name": company_name}
        if subject_ref:
            payload["subject_ref"] = subject_ref
        try:
            r = httpx.post(f"{self.base}/v1/tin/provision", json=payload, timeout=15.0)
            r.raise_for_status()
            return r.json()
        except httpx.HTTPError as e:
            raise TinGraphDown(str(e)) from e

    def verify_cac(self, cac_rc: str) -> dict[str, Any]:
        try:
            r = httpx.post(f"{self.base}/v1/verify/cac", json={"cac_rc": cac_rc}, timeout=15.0)
            r.raise_for_status()
            return r.json()
        except httpx.HTTPError as e:
            raise TinGraphDown(str(e)) from e
