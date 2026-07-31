"""VLM forensics adapter: Ollama qwen2-vl:7b (fallback llava:13b), JSON-mode.

When Ollama is unreachable the caller falls back to classical-only forensics
(SPEC A §6: check marked degraded, decision capped at step_up).
"""
from __future__ import annotations

import base64
import json
from typing import Any, Optional

import httpx

from ..config import get_settings

PROMPT = (
    "You are a document-forensics analyst. Inspect this identity document "
    "image and return ONLY JSON: "
    "{\"tamper_signals\": [..], \"template_match\": 0.0-1.0, \"anomalies\": [..]}"
)


class OllamaDown(RuntimeError):
    pass


class OllamaVLM:
    def __init__(self, base_url: str | None = None, model: str | None = None):
        s = get_settings()
        self.base = (base_url or s.ollama_url).rstrip("/")
        self.model = model or s.ollama_model
        self.fallback_model = s.ollama_fallback_model
        self.timeout = s.ollama_timeout_seconds

    def analyze(self, image_png: bytes) -> dict[str, Any]:
        """POST {OLLAMA}/api/generate; returns parsed JSON verdict."""
        payload = {
            "model": self.model,
            "prompt": PROMPT,
            "images": [base64.b64encode(image_png).decode()],
            "format": "json",
            "stream": False,
        }
        try:
            r = httpx.post(f"{self.base}/api/generate", json=payload, timeout=self.timeout)
            if r.status_code == 404:  # primary model missing -> fallback
                payload["model"] = self.fallback_model
                r = httpx.post(f"{self.base}/api/generate", json=payload, timeout=self.timeout)
            r.raise_for_status()
        except (httpx.HTTPError, httpx.TransportError) as e:
            raise OllamaDown(str(e)) from e
        body = r.json()
        try:
            out = json.loads(body.get("response", "{}"))
        except json.JSONDecodeError:
            out = {"tamper_signals": [], "template_match": None, "anomalies": ["unparseable_vlm_output"]}
        out["_model"] = payload["model"]
        return out

    def available(self) -> bool:
        try:
            return httpx.get(f"{self.base}/api/tags", timeout=3.0).status_code == 200
        except httpx.HTTPError:
            return False


_vlm: Optional[OllamaVLM] = None


def get_vlm() -> OllamaVLM:
    global _vlm
    if _vlm is None:
        _vlm = OllamaVLM()
    return _vlm
